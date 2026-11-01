package cli

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"dsgo/internal/tui"
)

type inputAction uint8

const (
	inputRead inputAction = iota
	inputBeginInteraction
	inputEndInteraction
)

var errComposerSuspended = errors.New("composer suspended for interactive input")

type inputChunk struct {
	data []byte
	err  error
}
type stdinStream struct {
	mu       sync.Mutex
	pending  *inputChunk
	partial  bool
	backlog  bool
	stopped  bool
	ready    chan struct{}
	consumed chan struct{}
}
type brokerRead struct {
	request   inputRequest
	cancel    context.CancelCauseFunc
	cancelled bool
	discard   bool
}
type brokerResult struct {
	read  *brokerRead
	line  inputLine
	draft tui.LineDraft
}

// A single goroutine owns the physical stream. Logical readers can be stopped
// between input characters without abandoning another read on the same stdin.
// UTF-8 and a buffered CRLF arrive as one packet, so a takeover cannot split a
// character or leave a newline from the old prompt as an answer to the new one.
func pumpInput(ctx context.Context, source *bufio.Reader, stream *stdinStream) {
	for {
		b, err := source.ReadByte()
		chunk := inputChunk{err: err}
		if err == nil {
			chunk.data = []byte{b}
			if !utf8.FullRune(chunk.data) {
				stream.mu.Lock()
				stream.partial = true
				stream.mu.Unlock()
				for !utf8.FullRune(chunk.data) && len(chunk.data) < utf8.UTFMax {
					next, readErr := source.ReadByte()
					if readErr != nil {
						chunk.err = readErr
						break
					}
					chunk.data = append(chunk.data, next)
				}
			}
			if b == '\r' && source.Buffered() > 0 {
				if next, _ := source.Peek(1); len(next) > 0 && next[0] == '\n' {
					_, _ = source.ReadByte()
					chunk.data = append(chunk.data, '\n')
				}
			}
		}
		stop := chunk.err != nil && !errors.Is(chunk.err, os.ErrDeadlineExceeded) && !os.IsTimeout(chunk.err)
		stream.mu.Lock()
		stream.partial, stream.pending, stream.stopped = false, &chunk, stop
		stream.backlog = source.Buffered() > 0
		stream.mu.Unlock()
		select {
		case stream.ready <- struct{}{}:
		default:
		}
		if stop {
			return
		}
		select {
		case <-stream.consumed:
		case <-ctx.Done():
			return
		}
	}
}

func (s *stdinStream) next(ctx context.Context, canSuspend func() bool) (inputChunk, error) {
	for {
		cause := context.Cause(ctx)
		if cause != nil && !errors.Is(cause, errComposerSuspended) {
			return inputChunk{}, cause
		}
		s.mu.Lock()
		if s.pending != nil {
			chunk := *s.pending
			s.pending = nil
			s.mu.Unlock()
			select {
			case s.consumed <- struct{}{}:
			default:
			}
			return chunk, nil
		}
		partial, backlog, stopped := s.partial, s.backlog, s.stopped
		s.mu.Unlock()
		if cause != nil && !partial && !backlog && (canSuspend == nil || canSuspend()) {
			return inputChunk{}, cause
		}
		if stopped {
			return inputChunk{}, io.EOF
		}
		done := ctx.Done()
		if cause != nil {
			done = nil
		}
		select {
		case <-s.ready:
		case <-done:
		}
	}
}

type logicalInput struct {
	ctx        context.Context
	stream     *stdinStream
	pending    []byte
	tailErr    error
	canSuspend func() bool
	deadline   time.Time
}

func (r *logicalInput) SetReadDeadline(deadline time.Time) error { r.deadline = deadline; return nil }

func (r *logicalInput) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.pending) == 0 {
		if r.tailErr != nil {
			return 0, r.tailErr
		}
		ctx := r.ctx
		if !r.deadline.IsZero() {
			var cancel context.CancelFunc
			ctx, cancel = context.WithDeadline(ctx, r.deadline)
			defer cancel()
		}
		chunk, err := r.stream.next(ctx, r.canSuspend)
		if err != nil {
			return 0, err
		}
		if chunk.err != nil && len(chunk.data) == 0 {
			return 0, chunk.err
		}
		r.tailErr = chunk.err
		r.pending = chunk.data
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

// Idle polls let Page refresh and check cancellation while the physical TTY
// remains blocking. A logical timeout never stops the shared stdin pump.
type pagerInput struct{ logicalInput }

func (r *pagerInput) Read(p []byte) (int, error) {
	r.deadline = time.Now().Add(100 * time.Millisecond)
	n, err := r.logicalInput.Read(p)
	if errors.Is(err, context.DeadlineExceeded) && r.ctx.Err() == nil {
		return n, nil
	}
	return n, err
}

func (a *App) ensureInputBroker() {
	a.inputOnce.Do(func() {
		a.reads = make(chan inputRequest)
		a.inputGate = make(chan struct{}, 1)
		a.inputGate <- struct{}{}
		ctx := a.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		stream := &stdinStream{ready: make(chan struct{}, 1), consumed: make(chan struct{}, 1)}
		a.inputStream = stream
		go pumpInput(ctx, a.in, stream)
		go a.inputBroker(ctx, stream)
	})
}

// beginInputInteraction serializes an entire approval/question group. The
// broker first parks its ordinary composer, then acknowledges the lease;
// only after that acknowledgement is a private prompt displayed.
func (a *App) beginInputInteraction(ctx context.Context) (func(), error) {
	a.ensureInputBroker()
	select {
	case <-a.inputGate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	release := func() {
		select {
		case a.reads <- inputRequest{action: inputEndInteraction}:
		case <-a.inputContext().Done():
		}
		a.inputGate <- struct{}{}
	}
	reply := make(chan inputLine, 1)
	select {
	case a.reads <- inputRequest{action: inputBeginInteraction, ctx: ctx, reply: reply}:
	case <-ctx.Done():
		a.inputGate <- struct{}{}
		return nil, ctx.Err()
	}
	select {
	case answer := <-reply:
		if answer.err != nil {
			release()
			return nil, answer.err
		}
		if err := ctx.Err(); err != nil {
			release()
			return nil, err
		}
		return release, nil
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	}
}

func (a *App) inputContext() context.Context {
	if a.ctx != nil {
		return a.ctx
	}
	return context.Background()
}

func (a *App) readInput(ctx context.Context, options tui.LineOptions, composer bool) (string, error) {
	return a.readInputRequest(ctx, options, composer, false)
}

func (a *App) readInputRequest(ctx context.Context, options tui.LineOptions, composer, private bool) (string, error) {
	a.ensureInputBroker()
	reply := make(chan inputLine, 1)
	registration := ctx
	if private {
		// The caller has already displayed this private prompt while owning
		// an interaction lease. Register its line even if cancellation raced
		// the display; otherwise a late affirmative could enter the composer.
		registration = a.inputContext()
	}
	select {
	case a.reads <- inputRequest{ctx: ctx, reply: reply, options: options, composer: composer, private: private}:
	case <-registration.Done():
		return "", registration.Err()
	}
	select {
	case answer := <-reply:
		return strings.TrimSuffix(strings.TrimSuffix(answer.text, "\n"), "\r"), answer.err
	case <-ctx.Done():
		if !composer {
			a.line("\nInput cancelled. Press Enter to discard the previous prompt before continuing.\n")
		}
		return "", ctx.Err()
	}
}

func (a *App) inputBroker(ctx context.Context, stream *stdinStream) {
	finished := make(chan brokerResult, 1)
	var active *brokerRead
	var parked *inputRequest
	var takeover *inputRequest
	var pending []inputRequest
	leased := false
	start := func(req inputRequest, discard bool) {
		workCtx, cancel := context.WithCancelCause(ctx)
		read := &brokerRead{request: req, cancel: cancel, discard: discard}
		active = read
		if req.composer && !a.flags.json {
			a.line("\n%s", req.options.Prompt)
		}
		go func() {
			input := &logicalInput{ctx: workCtx, stream: stream}
			if a.editor != nil && req.composer {
				input.canSuspend = a.editor.CanSuspendRead
			}
			reader := bufio.NewReader(input)
			var answer inputLine
			var draft tui.LineDraft
			if a.editor != nil && !discard {
				answer.text, answer.err = a.editor.ReadLineWithInput(req.options, reader, input.SetReadDeadline)
				draft = a.editor.InputDraft()
			} else {
				answer.text, answer.err = reader.ReadString('\n')
				if req.options.Draft != nil {
					answer.text = req.options.Draft.Text + answer.text
				}
				draft = tui.LineDraft{Text: answer.text, Cursor: len([]rune(answer.text))}
			}
			finished <- brokerResult{read: read, line: answer, draft: draft}
		}()
	}
	advance := func() {
		if active != nil {
			return
		}
		if takeover != nil {
			leased = takeover.ctx.Err() == nil
			takeover.reply <- inputLine{err: takeover.ctx.Err()}
			takeover = nil
		}
		if !leased && parked != nil {
			req := *parked
			parked = nil
			if req.ctx.Err() == nil {
				start(req, false)
				return
			}
			req.reply <- inputLine{err: req.ctx.Err()}
		}
		for i := 0; i < len(pending); i++ {
			req := pending[i]
			if leased && req.composer {
				continue
			}
			pending = append(pending[:i], pending[i+1:]...)
			if req.ctx.Err() != nil {
				req.reply <- inputLine{err: req.ctx.Err()}
				if req.private {
					// Even a cancelled read that has not started owns the next
					// line once its prompt was shown. Discard that stale answer.
					start(inputRequest{}, true)
					return
				}
				i--
				continue
			}
			start(req, false)
			return
		}
	}
	for {
		var cancelled <-chan struct{}
		if active != nil && !active.cancelled && !active.discard {
			cancelled = active.request.ctx.Done()
		}
		select {
		case req := <-a.reads:
			switch req.action {
			case inputBeginInteraction:
				takeover = &req
				if active != nil && active.request.composer {
					active.cancelled = true
					active.cancel(errComposerSuspended)
				}
			case inputEndInteraction:
				leased = false
				if takeover != nil {
					takeover.reply <- inputLine{err: context.Canceled}
					takeover = nil
				}
			default:
				pending = append(pending, req)
			}
		case <-cancelled:
			active.cancelled = true
			active.cancel(active.request.ctx.Err())
		case result := <-finished:
			read := result.read
			active = nil
			read.cancel(nil)
			if errors.Is(result.line.err, errComposerSuspended) {
				req := read.request
				req.options.Draft = &result.draft
				parked = &req
			} else if !read.discard {
				read.request.reply <- result.line
				if errors.Is(result.line.err, context.Canceled) || errors.Is(result.line.err, context.DeadlineExceeded) {
					// The cancelled prompt still owns its eventual newline.
					// Drain it privately before accepting any later question.
					start(inputRequest{}, true)
				}
			}
		case <-ctx.Done():
			if active != nil {
				active.cancel(ctx.Err())
			}
			return
		}
		advance()
	}
}
