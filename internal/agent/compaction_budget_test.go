package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"dsgo/internal/core"
)

func TestCompactionHonorsConfiguredOutputBudget(t *testing.T) {
	for _, scenario := range []string{"manual", "pressure", "overflow"} {
		t.Run(scenario, func(t *testing.T) {
			store, session := setup(t)
			longHistory(t, store, session.ID, 6, 12000)
			ordinary, summaries := 0, 0
			model := fakeModel{func(_ context.Context, request core.Request, _ func(core.Delta)) (core.Response, error) {
				if request.MaxTokens > 8192 {
					return core.Response{}, fmt.Errorf("model output limit 8192; got %d for %s", request.MaxTokens, request.Purpose)
				}
				if request.Purpose == "compaction" {
					summaries++
					if request.MaxTokens != 4096 {
						t.Errorf("summary widened last routed output budget: %d", request.MaxTokens)
					}
				} else {
					ordinary++
					if scenario == "overflow" && ordinary == 1 {
						return core.Response{}, errors.New("CONTEXT_WINDOW_EXCEEDED")
					}
				}
				return core.Response{Content: []core.Block{{Type: "text", Text: "Completed prior work."}}, StopReason: "end_turn"}, nil
			}}
			window := 1000000
			if scenario == "pressure" {
				window = 10000
			}
			engine := New(store, model, &fakeTools{}, Options{Model: "small-output-model", MaxTokens: 4096, ContextTokens: window})
			defer engine.Close()
			if err := engine.Run(context.Background(), session.ID, core.TextMessage("user", "Continue.")); err != nil {
				t.Fatal(err)
			}
			if scenario == "manual" {
				// A later global default must not widen the recorded route's
				// last known legal cap during manual compaction.
				engine.UpdateOptions(func(o *Options) { o.MaxTokens = 32768 })
				if err := engine.Compact(context.Background(), session.ID); err != nil {
					t.Fatal(err)
				}
			}
			if summaries == 0 || ordinary == 0 {
				t.Fatalf("scenario never exercised compaction and ordinary call: %d/%d", summaries, ordinary)
			}
		})
	}
}

func TestCompactionOutputBudgetUsesKnownRouteLimits(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		request, options, route int
		want                    int
	}{
		{"request", 4096, 16384, 8192, 4096},
		{"catalog-cap", 16384, 16384, 8192, 8192},
		{"options-fallback", 0, 4096, 8192, 4096},
		{"catalog-fallback", 0, 0, 8192, 8192},
		{"engine-default", 0, 0, 0, 16384},
		{"auxiliary-ceiling", 100000, 100000, 256000, defaultCompactionMaxTokens},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := New(nil, nil, nil, Options{MaxTokens: tc.options})
			defer engine.Close()
			got := engine.compactionOutputBudget(core.Request{MaxTokens: tc.request}, ContextRoute{DefaultMaxTokens: tc.route})
			if got != tc.want {
				t.Fatalf("budget = %d; want %d", got, tc.want)
			}
		})
	}
}
