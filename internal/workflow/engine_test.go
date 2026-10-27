package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testEngine(t *testing.T, child Child, opts ...func(*Options)) *Engine {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node unavailable")
	}
	o := Options{Command: func(ctx context.Context, cwd, workspace, mode, node string, args ...string) (*exec.Cmd, func(), error) {
		cmd := exec.CommandContext(ctx, node, args...)
		cmd.Dir = cwd
		return cmd, func() {}, nil
	}, Child: child, SyncTimeoutMS: 100}
	for _, set := range opts {
		set(&o)
	}
	e := New(o)
	t.Cleanup(e.Close)
	return e
}
func startFixture(t *testing.T, e *Engine, script string) *Run {
	t.Helper()
	r, err := e.Start(context.Background(), Request{ParentID: "parent", CWD: t.TempDir(), Permission: "read-only", Script: script, Meta: Meta{Name: "test", Description: "test workflow"}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func resultFixture(t *testing.T, r *Run) Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	v, err := r.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func TestWorkflowHooksAndDurableEventProtocol(t *testing.T) {
	var active, maxActive atomic.Int32
	e := testEngine(t, func(ctx context.Context, parent string, req ChildRequest, publish func(string)) (ChildResult, error) {
		if parent != "parent" {
			t.Errorf("parent=%q", parent)
		}
		n := active.Add(1)
		for {
			prev := maxActive.Load()
			if n <= prev || maxActive.CompareAndSwap(prev, n) {
				break
			}
		}
		defer active.Add(-1)
		publish("child-" + req.Prompt)
		time.Sleep(5 * time.Millisecond)
		return ChildResult{Text: req.Prompt, StopReason: "completed"}, nil
	}, func(o *Options) { o.MaxConcurrentAgents = 2 })
	r := startFixture(t, e, `phase(''); return null;`)
	v := resultFixture(t, r)
	if v.StopReason != "error" {
		t.Fatalf("empty phase accepted: %+v", v)
	}
	r = startFixture(t, e, `phase('review'); log('hello'); const x=await parallel([()=>agent('a'),()=>agent('b',{phase:''}),()=>agent('c')]);return {x,node:typeof process,timer:typeof setTimeout};`)
	v = resultFixture(t, r)
	if v.StopReason != "completed" || v.AgentsStarted != 3 {
		t.Fatalf("result %+v", v)
	}
	if maxActive.Load() > 2 {
		t.Fatalf("concurrency=%d", maxActive.Load())
	}
	m := v.Value.(map[string]any)
	if m["node"] != "undefined" || m["timer"] != "undefined" {
		t.Fatalf("unexpected globals %+v", m)
	}
	s := r.Snapshot()
	open := map[int]bool{}
	ended := false
	emptyPhase := false
	for _, ev := range s.Events {
		if ended {
			t.Fatal("event after run ending")
		}
		switch ev.Type {
		case "agent-start":
			if open[ev.Seq] {
				t.Fatal("duplicate seq")
			}
			open[ev.Seq] = true
			if ev.Phase != nil && *ev.Phase == "" {
				emptyPhase = true
			}
		case "agent-end":
			if !open[ev.Seq] {
				t.Fatal("end without start")
			}
			delete(open, ev.Seq)
		case "run-end":
			if len(open) != 0 {
				t.Fatal("run ended with members")
			}
			ended = true
		}
	}
	if !ended || !emptyPhase {
		t.Fatalf("missing terminal/empty phase %+v", s.Events)
	}
}
func TestFatalMisuseSurvivesCombinatorsAndOrdinaryErrorsDropItems(t *testing.T) {
	e := testEngine(t, func(ctx context.Context, parent string, req ChildRequest, publish func(string)) (ChildResult, error) {
		publish("child")
		return ChildResult{Text: "ok", StopReason: "completed"}, nil
	})
	for _, script := range []string{`return await parallel([()=>agent('x',{effort:'high'})]);`, `return await pipeline([1],()=>agent('x',{schema:{type:'object',minProperties:1}}));`, `return await parallel([()=>agent('')]);`} {
		v := resultFixture(t, startFixture(t, e, script))
		if v.StopReason != "error" {
			t.Fatalf("fatal swallowed: %s %+v", script, v)
		}
	}
	v := resultFixture(t, startFixture(t, e, `return await pipeline([1,2],(x)=>{if(x===1)throw new Error('ordinary');return x+1},(x,item,index)=>x+item+index);`))
	if v.StopReason != "completed" {
		t.Fatal(v)
	}
	b, _ := json.Marshal(v.Value)
	if string(b) != "[null,6]" {
		t.Fatalf("pipeline %s", b)
	}
}
func TestWorkflowRejectsLossyJSONAndParseBeforePublication(t *testing.T) {
	e := testEngine(t, func(context.Context, string, ChildRequest, func(string)) (ChildResult, error) {
		return ChildResult{}, nil
	})
	for _, script := range []string{`return {bad:undefined};`, `return new Date();`, `return [,,];`, `return {x:Infinity};`, `const x={};x.x=x;return x;`} {
		v := resultFixture(t, startFixture(t, e, script))
		if v.StopReason != "error" {
			t.Fatalf("lossy JSON accepted: %s %+v", script, v)
		}
	}
	_, err := e.Start(context.Background(), Request{ParentID: "parent", CWD: t.TempDir(), Script: `return }`, Meta: Meta{Name: "bad", Description: "bad"}})
	if err == nil || !strings.Contains(err.Error(), "SCRIPT_PARSE") {
		t.Fatalf("parse %+v", err)
	}
	if len(e.List("parent")) != 5 {
		t.Fatal("bad parse published a run")
	}
}
func TestWorkflowCancellationDrainsChildrenAndPairsEvents(t *testing.T) {
	started := make(chan struct{})
	drained := make(chan struct{})
	var once sync.Once
	e := testEngine(t, func(ctx context.Context, parent string, req ChildRequest, publish func(string)) (ChildResult, error) {
		publish("child")
		once.Do(func() { close(started) })
		<-ctx.Done()
		close(drained)
		return ChildResult{StopReason: "cancelled"}, nil
	})
	r := startFixture(t, e, `return await agent('wait');`)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("child not started")
	}
	r.Cancel("test stop")
	v := resultFixture(t, r)
	if v.StopReason != "cancelled" || v.Error != "test stop" {
		t.Fatalf("cancel %+v", v)
	}
	select {
	case <-drained:
	default:
		t.Fatal("result preceded child cleanup")
	}
	events := r.Snapshot().Events
	if events[len(events)-2].Type != "agent-end" || events[len(events)-2].Outcome != "cancelled" || events[len(events)-1].Type != "run-end" {
		t.Fatalf("unpaired %+v", events)
	}
	r.Dispose()
	r.Dispose()
}

func TestWorkflowOversizedFramesFailAndReleaseProcess(t *testing.T) {
	for _, script := range []string{`return 'x'.repeat(9*1024*1024);`, `log('x'.repeat(9*1024*1024));return null;`} {
		t.Run(script[:min(12, len(script))], func(t *testing.T) {
			e := testEngine(t, func(context.Context, string, ChildRequest, func(string)) (ChildResult, error) {
				t.Error("oversized frame unexpectedly dispatched a child")
				return ChildResult{}, nil
			})
			r := startFixture(t, e, script)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			value, err := r.Wait(ctx)
			if err != nil {
				r.Cancel("test cleanup")
				t.Fatalf("oversized frame did not settle without external cancellation: %v", err)
			}
			if value.StopReason != "error" || !strings.Contains(value.Error, "workflow transport:") {
				t.Fatalf("transport fault misclassified: %+v", value)
			}
			e.Close()
		})
	}
}
func TestWorkflowCapsAndSynchronousTimeout(t *testing.T) {
	e := testEngine(t, func(ctx context.Context, parent string, req ChildRequest, publish func(string)) (ChildResult, error) {
		publish("child")
		return ChildResult{Text: "ok", StopReason: "completed"}, nil
	}, func(o *Options) { o.MaxTotalAgents = 1; o.MaxItemsPerCall = 2 })
	for _, script := range []string{`return await parallel([()=>agent('a'),()=>agent('b')]);`, `return await parallel([()=>1,()=>2,()=>3]);`, `while(true){}`} {
		v := resultFixture(t, startFixture(t, e, script))
		if v.StopReason != "error" {
			t.Fatalf("cap not enforced %+v", v)
		}
	}
}
func TestWorkflowProviderStartFailureIsFatal(t *testing.T) {
	e := testEngine(t, func(context.Context, string, ChildRequest, func(string)) (ChildResult, error) {
		return ChildResult{}, errors.New("unknown provider")
	})
	v := resultFixture(t, startFixture(t, e, `return await parallel([()=>agent('x',{provider:'missing'})]);`))
	if v.StopReason != "error" || !strings.Contains(v.Error, "unknown provider") {
		t.Fatal(v)
	}
}
func TestWorkflowMetaAndStructuredOutput(t *testing.T) {
	for _, raw := range []string{`null`, `{"name":"x","description":"x","bad":1}`, `{"name":"x","description":"x","phases":[{"title":"x","bad":1}]}`} {
		if _, err := ValidateMeta([]byte(raw)); err == nil {
			t.Fatalf("bad meta accepted %s", raw)
		}
	}
	schema := json.RawMessage(`{"type":"object","properties":{"x":{"type":"integer"}},"required":["x"],"additionalProperties":false}`)
	if err := ValidateStructured(schema, map[string]any{"x": float64(1)}); err != nil {
		t.Fatal(err)
	}
	for _, v := range []any{map[string]any{"x": 1.5}, map[string]any{"x": float64(1), "extra": true}, map[string]any{}} {
		if err := ValidateStructured(schema, v); err == nil {
			t.Fatalf("invalid structured output %+v", v)
		}
	}
}
