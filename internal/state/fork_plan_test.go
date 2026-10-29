package state

import (
	"testing"

	"dsgo/internal/core"
)

func TestForkPlanUsesSelectedPrefixRatherThanFutureMetadata(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(t.TempDir(), "plan history")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = store.Append(session.ID, "", "user/message", core.TextMessage("user", "before plan"))
	_, _ = store.Append(session.ID, "", "plan/mode", map[string]bool{"active": true})
	session.Plan = true
	if err := store.Update(session); err != nil {
		t.Fatal(err)
	}
	early, err := store.Fork(session.ID, 1)
	if err != nil || early.Plan {
		t.Fatalf("early fork inherited a future plan edit: %+v %v", early, err)
	}
	full, err := store.Fork(session.ID, 0)
	if err != nil || !full.Plan {
		t.Fatalf("full fork lost a captured plan edit: %+v %v", full, err)
	}
	_, _ = store.Append(session.ID, "", "plan/updated", map[string]bool{"enabled": false})
	session.Plan = false
	if err := store.Update(session); err != nil {
		t.Fatal(err)
	}
	beforeLegacy, err := store.Fork(session.ID, 2)
	if err != nil || !beforeLegacy.Plan {
		t.Fatalf("prefix read later legacy state: %+v %v", beforeLegacy, err)
	}
	afterLegacy, err := store.Fork(session.ID, 0)
	if err != nil || afterLegacy.Plan {
		t.Fatalf("legacy transition did not apply: %+v %v", afterLegacy, err)
	}
}

func TestForkCompletedPlanExcludesUnsettledPrefixEdits(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(t.TempDir(), "completed plan")
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []struct {
		kind string
		data any
	}{
		{"turn/started", map[string]string{"id": "old"}},
		{"user/message", core.TextMessage("user", "request")},
		{"plan/mode", map[string]bool{"active": true}},
		{"turn/finished", map[string]string{"status": "completed"}},
		{"plan/mode", map[string]bool{"active": false}},
		{"turn/started", map[string]string{"id": "open"}},
	} {
		if _, err := store.Append(session.ID, "", event.kind, event.data); err != nil {
			t.Fatal(err)
		}
	}
	child, err := store.ForkCompleted(session.ID)
	if err != nil || !child.Plan {
		t.Fatalf("completed fork inherited later plan=false: %+v %v", child, err)
	}
}
