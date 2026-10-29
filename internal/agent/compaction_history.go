package agent

import "dsgo/internal/core"

// Compaction is a new user prompt and may run on a newly selected route. Native
// signatures and encrypted reasoning must never be interpolated into that text;
// durable neutral text and tool correlations remain the source for the summary.
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
				block.Source, block.Data = nil, ""
			}
			out.Content = append(out.Content, block)
		}
		result = append(result, out)
	}
	return result
}
