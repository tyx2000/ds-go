package cli

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dsgo/internal/core"
	"dsgo/internal/tui"
)

type takeoverOutput struct {
	mu      sync.Mutex
	text    strings.Builder
	changed chan struct{}
	onWrite func(string)
}

func (w *takeoverOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.text.Write(p)
	onWrite := w.onWrite
	w.mu.Unlock()
	select {
	case w.changed <- struct{}{}:
	default:
	}
	if onWrite != nil {
		onWrite(string(p))
	}
	return len(p), nil
}
func (w *takeoverOutput) count(text string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Count(w.text.String(), text)
}
func waitTakeoverOutput(t *testing.T, w *takeoverOutput, text string, count int) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for w.count(text) < count {
		select {
		case <-w.changed:
		case <-deadline.C:
			t.Fatalf("input never displayed %q", text)
		}
	}
}
func takeoverInput(t *testing.T) (*App, *io.PipeWriter, *takeoverOutput) {
	t.Helper()
	reader, writer := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	out := &takeoverOutput{changed: make(chan struct{}, 32)}
	a := &App{ctx: ctx, in: bufio.NewReader(reader), out: out}
	t.Cleanup(func() { cancel(); reader.Close(); writer.Close() })
	return a, writer, out
}

func TestSCOR027ApprovalPreservesPartialUTF8Composer(t *testing.T) {
	a, writer, out := takeoverInput(t)
	type line struct {
		text string
		err  error
	}
	composer := make(chan line, 1)
	go func() {
		text, err := a.readInput(a.ctx, tui.LineOptions{Prompt: "compose> ", History: true}, true)
		composer <- line{text, err}
	}()
	waitTakeoverOutput(t, out, "compose> ", 1)
	bytes := []byte("你")
	if _, err := writer.Write(bytes[:1]); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(time.Second); ; {
		a.inputStream.mu.Lock()
		partial := a.inputStream.partial
		a.inputStream.mu.Unlock()
		if partial {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("physical reader did not reach partial UTF-8 input")
		}
		time.Sleep(time.Millisecond)
	}
	approved := make(chan bool, 1)
	go func() {
		ok, err := a.approve(a.ctx, core.Approval{Description: "take over the prompt"})
		if err != nil {
			approved <- false
			return
		}
		approved <- ok
	}()
	if _, err := writer.Write(bytes[1:]); err != nil {
		t.Fatal(err)
	}
	waitTakeoverOutput(t, out, "Allow once?", 1)
	if _, err := io.WriteString(writer, "yes\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case ok := <-approved:
		if !ok {
			t.Fatal("answer was not delivered to approval")
		}
	case <-time.After(time.Second):
		t.Fatal("approval stayed blocked behind composer")
	}
	waitTakeoverOutput(t, out, "compose> ", 2)
	select {
	case got := <-composer:
		t.Fatalf("private answer escaped to composer: %+v", got)
	default:
	}
	if _, err := io.WriteString(writer, "好\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-composer:
		if got.err != nil || got.text != "你好" {
			t.Fatalf("draft not restored: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("composer did not resume")
	}
}

func TestSCOR027CancelledPrivateAnswerNeverBecomesChatInput(t *testing.T) {
	a, writer, out := takeoverInput(t)
	composer := make(chan string, 1)
	go func() {
		text, _ := a.readInput(a.ctx, tui.LineOptions{Prompt: "compose> ", History: true}, true)
		composer <- text
	}()
	waitTakeoverOutput(t, out, "compose> ", 1)
	ctx, cancel := context.WithCancel(a.ctx)
	defer cancel()
	cancelledAtDisplay := make(chan struct{})
	var cancelOnce sync.Once
	out.mu.Lock()
	out.onWrite = func(text string) {
		if strings.Contains(text, "Allow once?") {
			cancelOnce.Do(func() { cancel(); close(cancelledAtDisplay) })
		}
	}
	out.mu.Unlock()
	approval := make(chan error, 1)
	go func() {
		_, err := a.approve(ctx, core.Approval{Description: "cancel this private prompt"})
		approval <- err
	}()
	waitTakeoverOutput(t, out, "Allow once?", 1)
	select {
	case <-cancelledAtDisplay:
	case <-time.After(time.Second):
		t.Fatal("private prompt was not cancelled at its display boundary")
	}
	select {
	case err := <-approval:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation blocked")
	}
	// This belongs to the cancelled private prompt, even though its ordinary
	// composer is waiting to resume. Its affirmative text is never reused.
	if _, err := io.WriteString(writer, "yes\n"); err != nil {
		t.Fatal(err)
	}
	waitTakeoverOutput(t, out, "compose> ", 2)
	select {
	case got := <-composer:
		t.Fatalf("cancelled private text entered chat: %q", got)
	default:
	}
	if _, err := io.WriteString(writer, "safe public message\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-composer:
		if got != "safe public message" {
			t.Fatalf("wrong public input: %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("composer did not resume after discard")
	}
}

func TestSCOR027AutomaticQuestionCanTakeOverIdleChatPrompt(t *testing.T) {
	var calls atomic.Int32
	completed := make(chan struct{})
	a, parent := lifecycleApp(t, lifecycleModel(func(_ context.Context, req core.Request, _ func(core.Delta)) (core.Response, error) {
		if calls.Add(1) == 1 {
			return core.Response{Content: []core.Block{{Type: "tool_use", ID: "ask-takeover", Name: "ask_user_question", Input: core.JSON(map[string]any{"questions": []any{map[string]any{"id": "q", "question": "AUTOMATIC_PRIVATE_QUESTION", "options": []any{map[string]any{"label": "Yes"}}}}})}}}, nil
		}
		close(completed)
		return core.Response{Content: []core.Block{{Type: "text", Text: "completed"}}}, nil
	}))
	a.session = parent
	reader, writer := io.Pipe()
	a.in = bufio.NewReader(reader)
	out := &takeoverOutput{changed: make(chan struct{}, 32)}
	a.out = out
	chatDone := make(chan error, 1)
	go func() { chatDone <- a.chat() }()
	t.Cleanup(func() {
		a.cancel()
		reader.Close()
		writer.Close()
		select {
		case <-chatDone:
		case <-time.After(time.Second):
			t.Error("chat did not shut down")
		}
	})
	waitTakeoverOutput(t, out, "dsgo>", 1)
	if _, err := a.manager.deliver(a.ctx, parent.ID, core.TextMessage("user", "background job finished"), true); err != nil {
		t.Fatal(err)
	}
	waitTakeoverOutput(t, out, "AUTOMATIC_PRIVATE_QUESTION", 1)
	if _, err := io.WriteString(writer, "1\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("answer did not let automatic model work complete")
	}
	ctx, cancel := context.WithTimeout(a.ctx, time.Second)
	defer cancel()
	if err := a.manager.settle(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	events, err := a.store.Events(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	answerSeen := false
	for _, event := range events {
		if event.Type == "user/answer" {
			answerSeen = true
		}
		if messages, ok, err := core.UserInputMessages(event); ok && err == nil {
			for _, message := range messages {
				if len(message.Content) == 1 && message.Content[0].Text == "1" {
					t.Fatal("private answer was also admitted as public user input")
				}
			}
		}
	}
	if !answerSeen {
		t.Fatal("question answer was not durably delivered")
	}
}

func TestSCOR027InteractionWaitCancellationDoesNotAcquireNextLease(t *testing.T) {
	a, _, _ := takeoverInput(t)
	release, err := a.beginInputInteraction(a.ctx)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(a.ctx)
	waiter := make(chan error, 1)
	go func() {
		next, err := a.beginInputInteraction(ctx)
		if next != nil {
			next()
		}
		waiter <- err
	}()
	cancel()
	select {
	case err := <-waiter:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled interaction stayed queued behind another question")
	}
	release()
	last, err := a.beginInputInteraction(a.ctx)
	if err != nil {
		t.Fatal(err)
	}
	last()
}
