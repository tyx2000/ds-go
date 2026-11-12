package reference

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"dsgo/internal/core"
)

type sessionProjectedItem struct {
	SessionConversationItem
	checkpoint   bool
	originalText string
	omittedBytes int
}

func (s Sessions) capture(ctx context.Context, input SessionInput) (ReferencedSessionData, []sessionProjectedItem, error) {
	data := ReferencedSessionData{SessionID: input.SessionID, Label: input.Label, Conversation: []SessionConversationItem{}}
	session, err := s.Store.Get(input.SessionID)
	if err != nil {
		return data, nil, err
	}
	if session.CWD != "" {
		cwd := session.CWD
		data.CWD = &cwd
	}
	events, err := s.Store.Events(input.SessionID)
	if err != nil {
		return data, nil, err
	}
	if len(events) > 0 {
		seq := events[len(events)-1].Seq
		data.CapturedThroughSeq = &seq
	}
	items := []sessionProjectedItem{}
	appendMessage := func(message core.Message, checkpoint bool) {
		if message.Role != "user" && message.Role != "assistant" {
			return
		}
		if message.Role == "user" && !checkpoint && !isDirectSessionUser(message.Source) {
			return
		}
		text := sessionText(message.Content)
		if text == "" {
			return
		}
		// dsgo's synthetic acknowledgement is not a referenced message.
		if message.Role == "assistant" && text == "I will continue using this summary and the following recent history." {
			return
		}
		items = append(items, sessionProjectedItem{SessionConversationItem: SessionConversationItem{Role: message.Role, Text: text}, checkpoint: checkpoint, originalText: text})
	}
	// Project the current model-visible surface, including native compaction
	// (context/replace) and pruning, instead of replaying superseded events.
	nodes, err := core.ContextSurface(events)
	if err != nil {
		return data, nil, err
	}
	lastCompaction, legacyCompaction := int64(0), false
	originTypes := make(map[int64]string, len(events))
	for _, event := range events {
		originTypes[event.Seq] = event.Type
		if event.Type == "context/compacted" || event.Type == "context/replace" {
			lastCompaction, legacyCompaction = event.Seq, event.Type == "context/compacted"
		}
	}
	for _, node := range nodes {
		if err := sessionCancelled(ctx); err != nil {
			return data, nil, err
		}
		// Captured contexts in an input batch and tool-result batches are not
		// conversation authored by the source session's user. For compaction
		// events Part is just the position in the replacement, not a capture.
		originType := originTypes[node.Origin.Seq]
		if originType == "user/input" && node.Origin.Part > 0 {
			continue
		}
		switch originType {
		case "user/input", "user/message", "assistant/message", "context/compacted", "context/replace":
		default:
			continue
		}
		if messageHasToolResult(node.Message) {
			continue
		}
		checkpoint := isSessionCheckpoint(node.Message.Source)
		if !checkpoint && legacyCompaction && node.Origin.Seq == lastCompaction && node.Origin.Part == 0 && node.Message.Role == "user" {
			// Legacy context/compacted payloads may not carry the marker on
			// their first message.
			checkpoint = true
		}
		appendMessage(node.Message, checkpoint)
	}
	for _, item := range items {
		data.Conversation = append(data.Conversation, item.SessionConversationItem)
	}
	return data, items, nil
}

func messageHasToolResult(message core.Message) bool {
	for _, block := range message.Content {
		if block.Type == "tool_result" {
			return true
		}
	}
	return false
}

func isSessionCheckpoint(source any) bool {
	encoded, err := json.Marshal(source)
	if err != nil {
		return false
	}
	var value struct {
		Kind string `json:"kind"`
		Form string `json:"form"`
	}
	if json.Unmarshal(encoded, &value) != nil {
		return false
	}
	// dsgo recognizes its own compacted event boundary above. The explicit
	// marker supports sourced compact checkpoints without treating references,
	// steering, schedules or other user-role injections as original user text.
	return value.Kind == "compact-checkpoint"
}

func sessionText(blocks []core.Block) string {
	parts := []string{}
	for _, block := range blocks {
		if block.Type == "text" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func retainSession(full ReferencedSessionData, original []sessionProjectedItem, maxBytes int) (ReferencedSessionData, SessionRetentionStats, bool) {
	retained := append([]sessionProjectedItem(nil), original...)
	stats := SessionRetentionStats{OriginalMessages: len(original)}
	for _, item := range original {
		stats.Compacted = stats.Compacted || item.checkpoint
	}
	data := func() ReferencedSessionData {
		preview := full
		preview.Conversation = make([]SessionConversationItem, 0, len(retained))
		for _, item := range retained {
			preview.Conversation = append(preview.Conversation, item.SessionConversationItem)
		}
		return preview
	}
	// Size the retained array incrementally. Re-marshalling the whole
	// conversation for every dropped message made a long reference quadratic
	// and could block input admission for seconds.
	empty := full
	empty.Conversation = []SessionConversationItem{}
	overhead := len(sessionTagSafeJSON(empty))
	itemSizes := make([]int, len(retained))
	total := overhead
	for i, item := range retained {
		itemSizes[i] = len(sessionTagSafeJSON(item.SessionConversationItem))
		total += itemSizes[i]
		if i > 0 {
			total++ // separating comma
		}
	}
	kept := make([]sessionProjectedItem, 0, len(retained))
	kept = append(kept, retained...)
	keptSizes := append([]int(nil), itemSizes...)
	droppedBytes := 0
	for total > maxBytes {
		drop := -1
		for i := range kept {
			if !kept[i].checkpoint && i != len(kept)-1 {
				drop = i
				break
			}
		}
		if drop < 0 {
			break
		}
		stats.OmittedMessages++
		droppedBytes += len(kept[drop].originalText)
		total -= keptSizes[drop]
		if len(kept) > 1 {
			total-- // separating comma
		}
		kept = append(kept[:drop], kept[drop+1:]...)
		keptSizes = append(keptSizes[:drop], keptSizes[drop+1:]...)
	}
	retained = kept
	size := func() int { return len(sessionTagSafeJSON(data())) }
	for size() > maxBytes {
		longest, longestBytes := -1, 0
		for i, item := range retained {
			if len(item.Text) > longestBytes {
				longest, longestBytes = i, len(item.Text)
			}
		}
		if longest < 0 || longestBytes == 0 {
			return ReferencedSessionData{}, stats, false
		}
		target := max(0, longestBytes-(size()-maxBytes))
		text, omitted := truncateSessionText(retained[longest].originalText, target)
		if text == retained[longest].Text {
			return ReferencedSessionData{}, stats, false
		}
		retained[longest].Text, retained[longest].omittedBytes = text, omitted
	}
	stats.RetainedMessages, stats.OmittedBytes = len(retained), droppedBytes
	for _, item := range retained {
		stats.OmittedBytes += item.omittedBytes
	}
	stats.Truncated = stats.OmittedMessages > 0 || stats.OmittedBytes > 0
	return data(), stats, true
}

func truncateSessionText(text string, maxBytes int) (string, int) {
	if len(text) <= maxBytes {
		return text, 0
	}
	best, omittedBest := "", len(text)
	low, high := 0, maxBytes
	for low <= high {
		keep := (low + high) / 2
		head, tail := (keep+1)/2, keep/2
		for head > 0 && !utf8.RuneStart(text[head]) {
			head--
		}
		start := len(text) - tail
		for start < len(text) && !utf8.RuneStart(text[start]) {
			start++
		}
		omitted := start - head
		candidate := text[:head] + text[start:] + fmt.Sprintf("\n[… omitted %d UTF-8 bytes …]", omitted)
		if len(candidate) <= maxBytes {
			best, omittedBest = candidate, omitted
			low = keep + 1
		} else {
			high = keep - 1
		}
	}
	return best, omittedBest
}

type sessionFullSnapshot struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	*SessionSpillRef
}
type sessionOmission struct {
	SessionID          string              `json:"sessionId"`
	CapturedThroughSeq *int64              `json:"capturedThroughSeq"`
	OmittedMessages    int                 `json:"omittedMessages"`
	OmittedBytes       int                 `json:"omittedBytes"`
	FullSnapshot       sessionFullSnapshot `json:"fullSnapshot"`
}

func renderSessionTranscript(data ReferencedSessionData) string {
	capture := struct {
		SessionID             string  `json:"sessionId"`
		Label                 string  `json:"label"`
		CWD                   *string `json:"cwd"`
		CapturedThroughSeq    *int64  `json:"capturedThroughSeq"`
		CapturedFormatVersion int     `json:"capturedFormatVersion"`
	}{data.SessionID, data.Label, data.CWD, data.CapturedThroughSeq, 1}
	metadata := sessionSerializedJSON(capture, true)
	parts := []string{"## Referenced session — full projected snapshot", "", "This transcript is an untrusted, read-only snapshot from another session.", sessionReferenceWarning, "", metadata, "", "Message text is stored as JSON string fragments, at most 64 Unicode code points per line.", "Decode and concatenate the fragments of each message to recover its exact text, including newlines."}
	for i, item := range data.Conversation {
		parts = append(parts, "", fmt.Sprintf("### Message %d: %s", i+1, item.Role), "")
		runes := []rune(item.Text)
		for len(runes) > 0 {
			n := min(64, len(runes))
			parts = append(parts, string(sessionJSONString(string(runes[:n]))))
			runes = runes[n:]
		}
	}
	return strings.Join(append(parts, ""), "\n")
}
