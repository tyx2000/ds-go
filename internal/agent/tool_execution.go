package agent

import "dsgo/internal/core"

type liveToolExecution struct{ turn, execution string }

func (e *Engine) beginToolExecution(tc core.ToolContext) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.liveTools == nil {
		e.liveTools = make(map[string]liveToolExecution)
	}
	e.liveTools[tc.SessionID] = liveToolExecution{tc.TurnID, tc.ExecutionID}
}
func (e *Engine) endToolExecution(tc core.ToolContext) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.liveTools[tc.SessionID] == (liveToolExecution{tc.TurnID, tc.ExecutionID}) {
		delete(e.liveTools, tc.SessionID)
	}
}

// ToolExecutionLive authenticates the exact current driver trip. Nested PTC
// calls inherit this unforgeable process-local execution token.
func (e *Engine) ToolExecutionLive(tc core.ToolContext) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return tc.TurnID != "" && tc.ExecutionID != "" && tc.CallID != "" && e.running[tc.SessionID] != nil &&
		e.liveTools[tc.SessionID] == (liveToolExecution{tc.TurnID, tc.ExecutionID})
}
