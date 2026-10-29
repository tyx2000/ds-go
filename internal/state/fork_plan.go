package state

import (
	"encoding/json"
	"fmt"

	"dsgo/internal/core"
)

// forkPlanState reads only the prefix actually inherited by a conversation
// fork. The source session's current metadata may describe a later plan edit.
func forkPlanState(events []core.Event) (bool, error) {
	active := false
	for _, event := range events {
		switch event.Type {
		case "plan/mode":
			var mode struct{ Active *bool }
			if err := json.Unmarshal(event.Data, &mode); err != nil || mode.Active == nil {
				return false, fmt.Errorf("invalid plan mode event %d", event.Seq)
			}
			active = *mode.Active
		case "plan/updated":
			var legacy struct{ Enabled *bool }
			if err := json.Unmarshal(event.Data, &legacy); err != nil || legacy.Enabled == nil {
				return false, fmt.Errorf("invalid legacy plan event %d", event.Seq)
			}
			active = *legacy.Enabled
		}
	}
	return active, nil
}
