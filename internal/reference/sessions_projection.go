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
		if text != "" {
			items = append(items, sessionProjectedItem{SessionConversationItem: SessionConversationItem{Role: message.Role, Text: text}, checkpoint: checkpoint, originalText: text})
		}
	}
	for _, event := range events {
		if err := sessionCancelled(ctx); err != nil {
			return data, nil, err
		}
		switch event.Type {
		case "user/input":
			messages, recognized, err := core.UserInputMessages(event)
			if err != nil || !recognized || len(messages) == 0 {
				if err == nil {
					err = fmt.Errorf("invalid stored input batch")
				}
				return data, nil, err
			}
			appendMessage(messages[0], isSessionCheckpoint(messages[0].Source))
		case "user/message", "assistant/message":
			var message core.Message
			if err := json.Unmarshal(event.Data, &message); err != nil {
				return data, nil, err
			}
			appendMessage(message, isSessionCheckpoint(message.Source))
		case "context/compacted":
			var compacted struct {
				Messages []core.Message `json:"messages"`
			}
			if err := json.Unmarshal(event.Data, &compacted); err != nil {
				return data, nil, err
			}
			items = nil // only the current compacted surface is referenced
			for i, message := range compacted.Messages {
				if i == 1 && message.Role == "assistant" && sessionText(message.Content) == "I will continue using this summary and the following recent history." {
					continue // dsgo's synthetic compaction acknowledgement
				}
				appendMessage(message, i == 0 && message.Role == "user" || isSessionCheckpoint(message.Source))
			}
		}
	}
	for _, item := range items {
		data.Conversation = append(data.Conversation, item.SessionConversationItem)
	}
	return data, items, nil
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
	size := func() int { return len(sessionTagSafeJSON(data())) }
	droppedBytes := 0
	for size() > maxBytes {
		drop := -1
		for i, item := range retained {
			if !item.checkpoint && i != len(retained)-1 {
				drop = i
				break
			}
		}
		if drop < 0 {
			break
		}
		stats.OmittedMessages++
		droppedBytes += len(retained[drop].originalText)
		retained = append(retained[:drop], retained[drop+1:]...)
	}
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
