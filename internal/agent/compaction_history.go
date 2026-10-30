package agent

import "dsgo/internal/core"

// Compaction replays real message/block structure, including attachment refs
// and tool correlations. Auxiliary calls cannot inherit opaque route authority:
// visible reasoning is neutral text; signatures and encrypted blocks stay only
// in the original audit records and never become model-readable text.
func compactionHistory(messages []core.Message) []core.Message {
	result := make([]core.Message, 0, len(messages))
	for _, message := range messages {
		out := message
		out.ReplayModel, out.ReplayProvider, out.ReplayIdentity = "", "", ""
		if out.Role == "assistant" {
			out.Source = nil
		}
		out.Content = make([]core.Block, 0, len(message.Content))
		for _, block := range message.Content {
			if block.Type == "redacted_thinking" {
				continue
			}
			block.Signature = ""
			if block.Type == "thinking" || block.Type == "reasoning" {
				text := block.Text
				if text == "" {
					text = block.Thinking
				}
				if text == "" {
					continue
				}
				block = core.Block{Type: "text", Text: text}
			}
			out.Content = append(out.Content, block)
		}
		if len(out.Content) > 0 {
			result = append(result, out)
		}
	}
	return result
}
