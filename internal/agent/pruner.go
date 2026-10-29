package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"dsgo/internal/core"
)

const ToolResultPruneMarker = "\n\n[... tool result middle pruned ...]\n\n"

type ToolResultPruneConfig struct {
	ThresholdChars *int `json:"thresholdChars,omitempty"`
	HeadChars      *int `json:"headChars,omitempty"`
	TailChars      *int `json:"tailChars,omitempty"`
}

type resolvedPruneConfig struct{ threshold, head, tail int }

func resolvePruneConfig(config *ToolResultPruneConfig) (resolvedPruneConfig, error) {
	resolved := resolvedPruneConfig{8192, 4096, 1024}
	if config != nil {
		if config.ThresholdChars != nil {
			resolved.threshold = *config.ThresholdChars
		}
		if config.HeadChars != nil {
			resolved.head = *config.HeadChars
		}
		if config.TailChars != nil {
			resolved.tail = *config.TailChars
		}
	}
	if resolved.threshold < 1 || resolved.head < 0 || resolved.tail < 0 || resolved.head > resolved.threshold || resolved.tail > resolved.threshold || resolved.head+utf8.RuneCountInString(ToolResultPruneMarker)+resolved.tail > resolved.threshold {
		return resolved, errors.New("tool-result prune: head + marker + tail must fit its positive threshold")
	}
	return resolved, nil
}

// PruneToolResultContent removes one global text middle across rich blocks,
// retaining non-text order and text-block metadata. Boundaries count Unicode
// code points, as the official pruner does, rather than UTF-16 units or bytes.
func PruneToolResultContent(blocks []core.Block, config *ToolResultPruneConfig) ([]core.Block, bool, error) {
	resolved, err := resolvePruneConfig(config)
	if err != nil {
		return nil, false, err
	}
	raw, err := json.Marshal(blocks)
	if err != nil {
		return nil, false, err
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, false, err
	}
	pruned, _, _, changed, err := pruneRawContent(entries, resolved)
	if err != nil || !changed {
		return nil, changed, err
	}
	result := make([]core.Block, len(pruned))
	for index := range pruned {
		if err := json.Unmarshal(pruned[index], &result[index]); err != nil {
			return nil, false, err
		}
	}
	return result, true, nil
}

func pruneRawContent(blocks []json.RawMessage, config resolvedPruneConfig) ([]json.RawMessage, int, int, bool, error) {
	texts := make([]*string, len(blocks))
	total := 0
	for index, block := range blocks {
		var typed struct {
			Type string
			Text string
		}
		if err := json.Unmarshal(block, &typed); err != nil {
			return nil, 0, 0, false, err
		}
		if typed.Type == "text" {
			text := typed.Text
			texts[index] = &text
			total += utf8.RuneCountInString(text)
		}
	}
	if total <= config.threshold {
		return nil, total, total, false, nil
	}
	removedStart, removedEnd := config.head, total-config.tail
	consumed, after := 0, 0
	markerInserted := false
	pruned := []json.RawMessage{}
	for index, block := range blocks {
		if texts[index] == nil {
			pruned = append(pruned, block)
			continue
		}
		points := []rune(*texts[index])
		start, end := consumed, consumed+len(points)
		headEnd := min(len(points), max(0, removedStart-start))
		tailStart := min(len(points), max(0, removedEnd-start))
		marker := ""
		if start < removedEnd && end > removedStart && !markerInserted {
			marker, markerInserted = ToolResultPruneMarker, true
		}
		text := string(points[:headEnd]) + marker + string(points[tailStart:])
		if text != "" {
			var original map[string]json.RawMessage
			if err := json.Unmarshal(block, &original); err != nil {
				return nil, 0, 0, false, err
			}
			original["text"] = core.JSON(text)
			pruned = append(pruned, core.JSON(original))
			after += utf8.RuneCountInString(text)
		}
		consumed = end
	}
	if !markerInserted || after > config.threshold || after >= total {
		return nil, 0, 0, false, errors.New("tool-result prune failed to produce a smaller bounded replacement")
	}
	return pruned, total, after, true, nil
}

func pruneToolValue(value any, config resolvedPruneConfig) (any, int, bool, error) {
	if text, ok := value.(string); ok {
		blocks, before, after, changed, err := pruneRawContent([]json.RawMessage{core.JSON(core.Block{Type: "text", Text: text})}, config)
		if err != nil || !changed {
			return value, 0, false, err
		}
		var block core.Block
		if len(blocks) != 1 || json.Unmarshal(blocks[0], &block) != nil {
			return nil, 0, false, errors.New("invalid pruned single-text result")
		}
		return block.Text, before - after, true, nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, 0, false, err
	}
	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return value, 0, false, nil
	}
	pruned, before, after, changed, err := pruneRawContent(blocks, config)
	if err != nil || !changed {
		return value, 0, false, err
	}
	return pruned, before - after, true, nil
}

type contextReplacementStore interface {
	AppendContextReplacement(string, string, core.ContextReplacement) (core.Event, error)
}

// pruneToolResults lands one atomic native surface replacement per stored
// tool-results batch. The canonical tool records remain in the audit log;
// replay derives the exact new surface instead of relying on a mutable cache.
func (e *Engine) pruneToolResults(ctx context.Context, id, turn string) (int, error) {
	config, err := resolvePruneConfig(e.options().ToolResultPruner)
	if err != nil {
		return 0, err
	}
	nodes, _, err := e.contextSurface(id)
	if err != nil {
		return 0, err
	}
	prunedResults := 0
	for _, candidate := range nodes {
		if err := ctx.Err(); err != nil {
			return prunedResults, err
		}
		message := candidate.Message
		message.Content = append([]core.Block(nil), message.Content...)
		changed := false
		for index, block := range message.Content {
			if block.Type != "tool_result" {
				continue
			}
			content, _, pruned, err := pruneToolValue(block.Content, config)
			if err != nil {
				return prunedResults, err
			}
			if pruned {
				message.Content[index].Content = content
				prunedResults++
				changed = true
			}
		}
		if !changed {
			continue
		}
		store, ok := e.Store.(contextReplacementStore)
		if !ok {
			return prunedResults, errors.New("durable tool pruning requires atomic context replacement")
		}
		current, _, err := e.contextSurface(id)
		if err != nil {
			return prunedResults, err
		}
		position := -1
		for index, node := range current {
			if node.Origin == candidate.Origin {
				position = index
				break
			}
		}
		if position < 0 {
			return prunedResults, fmt.Errorf("tool-result surface changed before pruning origin %+v", candidate.Origin)
		}
		event, err := store.AppendContextReplacement(id, turn, core.ContextReplacement{Version: 1, Expected: core.ContextOrigins(current), Start: position, End: position + 1, Messages: []core.Message{message}})
		if err != nil {
			return prunedResults, err
		}
		e.emit(id, "event", event)
	}
	return prunedResults, nil
}
