package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"dsgo/internal/agent"
	"dsgo/internal/attachments"
	instructions "dsgo/internal/context"
	"dsgo/internal/core"
	"dsgo/internal/mcp"
	"dsgo/internal/reference"
	"dsgo/internal/state"
	"dsgo/internal/tools"
	"dsgo/internal/tui"
)

const Version = "0.1.0-dev"

type flags struct {
	home, cwd, model, provider, thinking, permission, approval, resume string
	json                                                               bool
}

func (f *flags) bind(fs *flag.FlagSet) {
	fs.StringVar(&f.home, "home", f.home, "data/config directory (DSGO_HOME)")
	fs.StringVar(&f.cwd, "cwd", f.cwd, "workspace directory")
	fs.StringVar(&f.model, "model", f.model, "model name")
	fs.StringVar(&f.provider, "provider", f.provider, "configured provider profile (default deepseek-official)")
	fs.StringVar(&f.thinking, "thinking", f.thinking, "off, low, high, max")
	fs.StringVar(&f.permission, "permission", f.permission, "read-only, workspace-write, full-access")
	fs.StringVar(&f.approval, "approval", f.approval, "on-request, never")
	fs.StringVar(&f.resume, "resume", f.resume, "session ID or latest")
	fs.BoolVar(&f.json, "json", f.json, "newline-delimited JSON output")
}

type App struct {
	in               *bufio.Reader
	out, err         io.Writer
	flags            flags
	store            *state.Store
	cfg              state.Config
	registry         *tools.Registry
	engine           *agent.Engine
	skills           []instructions.Skill
	session          core.Session
	writeMu          sync.Mutex
	editor           *tui.LineEditor // nil unless stdin and stdout are terminals
	inputMu          sync.Mutex
	inputOnce        sync.Once
	reads            chan inputRequest
	manager          *agents
	ctx              context.Context
	cancel           context.CancelFunc
	multiline        bool
	plan             atomic.Bool
	planState        planRuntime
	timeContext      timeContextRuntime
	instructionState instructionBaselines
	current          atomic.Value
	drafts           attachmentDrafts
	attachmentStore  *attachments.Store
	pricingCatalogs  deepSeekCatalogCache
	mcpMu            sync.RWMutex
	mcpReloadMu      sync.Mutex
	mcpSupervisors   map[string]*mcp.Supervisor
	mcpInstructions  map[string]string
	mcpEntries       map[string]state.MCPServer
	mcpContext       context.Context
	mcpCancel        context.CancelFunc
	mcpWatchDone     chan struct{}
	unattended       bool
	receipts         sync.Map
	background       sync.WaitGroup
	streamKinds      sync.Map
	signalMu         sync.Mutex
	signalRefs       int
	signalStop       func()
	foreground       sync.Map // session id -> *foregroundTurn
	fileMu           sync.Mutex
	fileSearches     map[string]fileSearch
	goals            goalRuntime
	jobWakeMu        sync.Mutex
	jobWakes         map[string]jobWakeCount
	jobOutputBounds  sync.Map
}

type inputLine struct {
	text string
	err  error
}
type inputRequest struct {
	reply   chan inputLine
	options tui.LineOptions
}

type fileSearch struct {
	cwd   string
	index *reference.Files
}

func Main(args []string, in io.Reader, out, errOut io.Writer) int {
	a := &App{in: bufio.NewReader(in), out: out, err: errOut, flags: flags{home: state.DefaultRoot()}}
	// Interactive prompts edit whole characters; pipes keep line reading, and
	// Emacs shell buffers keep canonical mode because Emacs edits the line.
	if tty, ok := in.(*os.File); ok && tui.IsTerminal(tty) && os.Getenv("INSIDE_EMACS") == "" {
		if screen, ok := out.(*os.File); ok && tui.IsTerminal(screen) {
			a.editor = tui.NewLineEditor(a.in, tty, screen, func(s string) {
				a.writeMu.Lock()
				defer a.writeMu.Unlock()
				_, _ = io.WriteString(a.out, s)
			})
			defer a.editor.Close()
		}
	}
	fs := flag.NewFlagSet("dsgo", flag.ContinueOnError)
	fs.SetOutput(errOut)
	a.flags.bind(fs)
	versionFlag := fs.Bool("version", false, "print version")
	fs.Usage = func() { fmt.Fprint(out, help) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *versionFlag {
		fmt.Fprintf(out, "dsgo %s\nupstream %s\n", Version, core.UpstreamCommit)
		return 0
	}
	args = fs.Args()
	command := "chat"
	if len(args) > 0 {
		command = args[0]
		args = args[1:]
	}
	if command == "help" {
		fmt.Fprint(out, help)
		return 0
	}
	if command == "version" || command == "--version" {
		fmt.Fprintf(out, "dsgo %s\nupstream %s\n", Version, core.UpstreamCommit)
		return 0
	}
	a.unattended = command == "run"
	if command == "run" || command == "chat" {
		sub := flag.NewFlagSet(command, flag.ContinueOnError)
		sub.SetOutput(errOut)
		a.flags.bind(sub)
		if err := sub.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			return 2
		}
		args = sub.Args()
	}
	root, err := filepath.Abs(a.flags.home)
	if a.flags.json && a.editor != nil {
		a.editor.Close()
		a.editor = nil
	}
	if err != nil {
		return a.fail(err)
	}
	a.flags.home = root
	a.cfg, err = state.LoadConfig(root)
	if err != nil {
		return a.fail(err)
	}
	if command == "config" {
		return a.config(args)
	}
	if command == "auth" {
		if err = a.auth(args); err != nil {
			return a.fail(err)
		}
		return 0
	}
	a.cfg = a.cfg.WithEnvironment()
	if a.flags.model != "" {
		a.cfg.Model = a.flags.model
	}
	if a.flags.provider != "" {
		a.cfg.SelectedProvider = a.flags.provider
	}
	if a.flags.thinking != "" {
		a.cfg.Thinking = a.flags.thinking
	}
	if a.flags.permission != "" {
		a.cfg.Permission = a.flags.permission
	}
	if a.flags.approval != "" {
		a.cfg.ApprovalPolicy = a.flags.approval
	}
	if err = a.cfg.Validate(); err != nil {
		return a.fail(err)
	}
	a.store, err = state.Open(root)
	if err != nil {
		return a.fail(err)
	}
	if a.flags.cwd == "" {
		a.flags.cwd, err = os.Getwd()
	}
	if err != nil {
		return a.fail(err)
	}
	a.flags.cwd, err = filepath.Abs(a.flags.cwd)
	if err != nil {
		return a.fail(err)
	}
	info, err := os.Stat(a.flags.cwd)
	if err != nil {
		return a.fail(err)
	}
	if !info.IsDir() {
		return a.fail(errors.New("workspace is not a directory"))
	}
	a.ctx, a.cancel = context.WithCancel(context.Background())
	defer a.cancel()
	defer a.closeFileSearches()
	if command == "models" {
		if err = a.modelsCommand(args); err != nil {
			return a.fail(err)
		}
		return 0
	}
	if command == "sessions" || command == "session" {
		if err = a.sessions(args); err != nil {
			return a.fail(err)
		}
		return 0
	}
	if command == "doctor" {
		return a.doctor()
	}
	a.skills, err = instructions.Skills(a.flags.cwd)
	if err != nil {
		return a.fail(err)
	}
	if command == "skills" {
		a.print(a.skills)
		return 0
	}
	if command != "chat" && command != "run" && command != "tools" {
		return a.fail(fmt.Errorf("unknown command %q; run dsgo help", command))
	}
	system, err := instructions.System(a.flags.cwd, a.skills)
	if err != nil {
		return a.fail(err)
	}
	a.registry = tools.New()
	defer a.registry.Close()
	if err = a.configureJobs(); err != nil {
		return a.fail(err)
	}
	a.registry.SetSpillDir(filepath.Join(root, "artifacts", "shell-output"))
	replayKey, err := a.store.ReplayKey()
	if err != nil {
		return a.fail(err)
	}
	models, err := buildModels(a.cfg, replayKey)
	if err != nil {
		return a.fail(err)
	}
	a.attachmentStore, err = attachments.Open(root)
	if err != nil {
		return a.fail(err)
	}
	models = a.attachmentStore.ResolveProjected(models, a.projectRequestImage)
	a.engine = agent.New(a.store, models, a, agent.Options{Provider: a.cfg.SelectedProvider, Model: a.cfg.Model, Thinking: a.cfg.Thinking, MaxTokens: a.cfg.MaxTokens, MaxSteps: a.cfg.MaxSteps, ContextTokens: a.cfg.ContextTokens, ApprovalPolicy: a.cfg.ApprovalPolicy, System: system, Approve: a.approve, SystemFor: a.systemFor, ToolsFor: a.specsFor, RequestContext: a.requestContext, ContextFor: a.contextRoute})
	a.engine.Notify = a.notice
	a.engine.UpdateOptions(func(o *agent.Options) {
		o.PrepareInput = a.prepareInput
		o.CommitPreparedInput = a.commitGoalInput
		o.BeforeModel = a.beforeModel
		o.BeforeStep = a.prepareStep
		o.AfterCompaction = a.ensureInstructionBaseline
		o.BeforeTurn = a.workspaceTurnStarted
		o.AfterTurn = a.workspaceTurnFinished
		o.RenderToolResult = a.spillToolResult
	})
	defer a.engine.Close()
	a.manager = newAgents(a)
	defer a.manager.close()
	defer a.closeGoals()
	defer a.closeMCP()
	if err = a.registerTools(); err != nil {
		return a.fail(err)
	}
	if command == "run" || command == "chat" {
		if err = a.selectSession(a.flags.resume); err != nil {
			return a.fail(err)
		}
		a.flags.cwd = a.session.CWD
	}
	if err = a.connectMCP(); err != nil {
		return a.fail(err)
	}
	if command == "tools" {
		a.print(a.registry.Specs())
		return 0
	}
	if command == "run" {
		stopSignals := a.acquireSignals()
		defer stopSignals()
		prompt := strings.Join(args, " ")
		if prompt == "" {
			b, x := io.ReadAll(io.LimitReader(a.in, 4<<20+1))
			if x != nil {
				return a.fail(x)
			}
			if len(b) > 4<<20 {
				return a.fail(errors.New("prompt exceeds 4 MiB"))
			}
			prompt = string(b)
		}
		if strings.TrimSpace(prompt) == "" {
			return a.fail(errors.New("prompt is empty"))
		}
		if err = a.turn(prompt); err != nil {
			return a.fail(err)
		}
		if err = a.settleDelegations(); err != nil {
			return a.fail(err)
		}
		return 0
	}
	if err = a.chat(); err != nil {
		return a.fail(err)
	}
	return 0
}

// promptLabel highlights a prompt on an interactive terminal so typed input
// stands apart from output. Pipes, dumb terminals and NO_COLOR keep the plain
// text.
func (a *App) promptLabel(label string) string {
	if a.editor == nil || os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return label
	}
	return "\x1b[1;36m" + label + "\x1b[0m"
}

func (a *App) fail(err error) int { a.errorLine("dsgo: " + clean(err.Error())); return 1 }
func (a *App) print(v any) {
	if a.flags.json {
		a.writeJSON(v)
		return
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	a.line("%s\n", clean(string(b)))
}
func (a *App) line(format string, args ...any) {
	text := fmt.Sprintf(format, args...)
	if a.flags.json {
		a.writeJSON(map[string]any{"type": "output", "text": text})
		return
	}
	if a.editor != nil {
		a.editor.WriteAbove(text)
		return
	}
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	_, _ = io.WriteString(a.out, text)
}

func (a *App) writeJSON(value any) {
	data, err := json.Marshal(value)
	if err != nil {
		a.errorLine("dsgo: could not encode JSON output: " + err.Error())
		return
	}
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	_, _ = a.out.Write(append(data, '\n'))
}
func (a *App) errorLine(text string) {
	if a.editor != nil {
		a.editor.WriteAbove(text + "\n")
		return
	}
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	if a.err != nil {
		fmt.Fprintln(a.err, text)
	}
}

// Untrusted model/file output must never inject terminal control sequences.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || r >= 32 && r != 127 && !(r >= 0x80 && r <= 0x9f) {
			return r
		}
		return -1
	}, s)
}

func (a *App) selectSession(id string) error {
	if id == "latest" {
		sessions, err := a.store.List(false)
		if err != nil {
			return err
		}
		if len(sessions) == 0 {
			return errors.New("no session to resume")
		}
		id = sessions[0].ID
	}
	var next core.Session
	var err error
	fresh := id == ""
	if fresh {
		next, err = a.store.Create(a.flags.cwd, "New session")
		if err != nil {
			return err
		}
		next.Permission, next.Model, next.Provider, next.Thinking = a.cfg.Permission, a.cfg.Model, a.cfg.SelectedProvider, a.cfg.Thinking
	} else {
		next, err = a.store.Get(id)
		if err != nil {
			return err
		}
		if next.Archived {
			return errors.New("session is archived; unarchive it before resuming")
		}
		if a.flags.permission != "" {
			next.Permission = a.flags.permission
		}
		if a.flags.model != "" {
			next.Model = a.flags.model
		}
		if a.flags.provider != "" {
			next.Provider = a.flags.provider
		}
		if a.flags.thinking != "" {
			next.Thinking = a.flags.thinking
		}
	}
	publish := false
	defer func() {
		if fresh && !publish {
			_ = a.store.Delete(next.ID)
		}
	}()
	skills, err := instructions.Skills(next.CWD)
	if err != nil {
		return err
	}
	if err = a.store.Update(next); err != nil {
		return err
	}
	if fresh {
		if err = a.sampleSubagentPolicy(next.ID); err != nil {
			return err
		}
	}
	if a.session.ID != "" {
		a.goalOpened(a.session.ID)
	}
	a.goalOpened(next.ID)
	a.resetJobWakeBudget(next.ID)
	a.session, a.skills = next, skills
	a.current.Store(next.ID)
	a.plan.Store(next.Plan)
	publish = true
	return nil
}

func (a *App) notice(n agent.Notice) {
	a.jobInputAccepted(n)
	a.goalNotice(n)
	if n.Type == "tool_finished" {
		a.invalidateFileIndex(n.SessionID)
	}
	if n.Type == "event" {
		if event, ok := n.Data.(core.Event); ok {
			if messages, recognized, err := core.UserInputMessages(event); recognized && err == nil && len(messages) > 0 && messages[0].DeliveryID != "" {
				m := messages[0]
				if ch, ok := a.receipts.LoadAndDelete(m.DeliveryID); ok {
					close(ch.(chan struct{}))
				}
			}
		}
	}
	if a.flags.json {
		a.writeMu.Lock()
		defer a.writeMu.Unlock()
		_ = json.NewEncoder(a.out).Encode(n)
		return
	}
	if n.Type == "warning" {
		var warning struct{ Phase, Message, Error string }
		if json.Unmarshal(core.JSON(n.Data), &warning) == nil {
			text := "Warning"
			if warning.Phase != "" {
				text += " [" + clean(warning.Phase) + "]"
			}
			if warning.Message != "" {
				text += ": " + clean(warning.Message)
			}
			if warning.Error != "" {
				text += " " + clean(warning.Error)
			}
			a.errorLine(text)
		}
		return
	}
	if n.SessionID != a.current.Load() {
		if n.Type == "turn_finished" {
			a.line("\n[agent %s finished]\n", n.SessionID)
		}
		return
	}
	switch n.Type {
	case "delta":
		d, ok := n.Data.(core.Delta)
		if !ok {
			return
		}
		kind := ""
		if d.Type == "text" || d.Type == "text_delta" {
			kind = "answer"
		}
		if d.Type == "thinking" || d.Type == "thinking_delta" {
			kind = "thinking"
		}
		if kind != "" && d.Text != "" {
			previous, loaded := a.streamKinds.Load(n.SessionID)
			if !loaded || previous != kind {
				a.line("\n[%s]\n", kind)
			}
			a.streamKinds.Store(n.SessionID, kind)
			a.line("%s", clean(d.Text))
		}
	case "tool_started":
		if b, ok := n.Data.(core.Block); ok {
			a.line("\n[tool %s] %s\n", clean(b.Name), clean(string(b.Input)))
		}
	case "tool_finished":
		a.line("[tool finished]\n")
	case "turn_finished":
		a.streamKinds.Delete(n.SessionID)
		a.line("\n")
	}
}

func (a *App) approve(ctx context.Context, q core.Approval) (bool, error) {
	if a.flags.json || a.unattended {
		return false, errors.New("approval requires an interactive terminal; JSON mode denies unavailable interaction")
	}
	a.inputMu.Lock()
	defer a.inputMu.Unlock()
	a.line("\nApproval %s\n%s\n%s\nAllow once? [y/N] ", clean(q.Tool), clean(q.Description), clean(string(q.Input)))
	answer, err := a.readPrivateLine(ctx, "Allow once? [y/N] ")
	if err != nil {
		return false, err
	}
	if err = ctx.Err(); err != nil {
		return false, err
	}
	return strings.EqualFold(strings.TrimSpace(answer), "y") || strings.EqualFold(strings.TrimSpace(answer), "yes"), nil
}

func (a *App) readLine(ctx context.Context) (string, error) {
	return a.readLineWithOptions(ctx, tui.LineOptions{})
}
func (a *App) readPrivateLine(ctx context.Context, prompt string) (string, error) {
	return a.readLineWithOptions(ctx, tui.LineOptions{Prompt: prompt})
}
func (a *App) readLineWithOptions(ctx context.Context, options tui.LineOptions) (string, error) {
	// A single broker owns stdin. Each prompt has a private reply queue: after
	// cancellation its eventual line is discarded, never reused as approval.
	a.inputOnce.Do(func() {
		a.reads = make(chan inputRequest)
		go func() {
			for {
				select {
				case req := <-a.reads:
					var s string
					var err error
					if a.editor != nil {
						s, err = a.editor.ReadLineWithOptions(req.options)
					} else {
						s, err = a.in.ReadString('\n')
					}
					req.reply <- inputLine{s, err}
				case <-a.ctx.Done():
					return
				}
			}
		}()
	})
	done := make(chan inputLine, 1)
	select {
	case a.reads <- inputRequest{reply: done, options: options}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	select {
	case r := <-done:
		return strings.TrimSuffix(strings.TrimSuffix(r.text, "\n"), "\r"), r.err
	case <-ctx.Done():
		a.line("\nInput cancelled. Press Enter to discard the previous prompt before continuing.\n")
		return "", ctx.Err()
	}
}

func (a *App) turn(prompt string) error {
	stopSignals := a.acquireSignals()
	defer stopSignals()
	id := a.session.ID
	if a.manager != nil {
		a.manager.resumeWakes(id)
	}
	input := core.TextMessage("user", prompt)
	attachments, draftThrough := a.snapshotDraftAttachments(id)
	input.Content = append(input.Content, attachments...)
	input.DeliveryID = "input:" + agent.ID()
	pendingInput := input
	ctx, cancel := context.WithCancel(a.ctx)
	defer cancel()
	owner := &foregroundTurn{cancel: cancel}
	a.foreground.Store(id, owner)
	defer a.foreground.CompareAndDelete(id, owner)
	done := make(chan error, 1)
	go func() { done <- a.engine.Run(ctx, id, input) }()
	for {
		select {
		case err := <-done:
			_, goalRound, _ := state.GoalSource(pendingInput)
			if goalRound && !a.inputAdmitted(id, pendingInput.DeliveryID) {
				a.dropGoalReservation(id, pendingInput.DeliveryID)
			}
			if errors.Is(err, agent.ErrBusy) {
				if goalRound {
					return nil
				}
				// Another automatic owner may have won the admission race. The
				// full user input is committed before the composer clears media.
				if _, queueErr := a.manager.deliver(ctx, id, pendingInput, false); queueErr != nil {
					return queueErr
				}
				a.retireDraftAttachments(id, draftThrough)
				a.line("\n[Message queued for the active session]\n")
				return nil
			}
			if a.inputAdmitted(id, input.DeliveryID) {
				a.retireDraftAttachments(id, draftThrough)
			}
			if err != nil {
				return err
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continuation, x := a.goalContinuation(ctx, id)
			if x != nil {
				return x
			}
			if continuation == nil {
				return nil
			}
			a.line("\n[Continuing active goal]\n")
			pendingInput = *continuation
			go func() { done <- a.engine.Run(ctx, id, *continuation) }()
		}
	}
}

func (a *App) inputAdmitted(id, delivery string) bool {
	events, err := a.store.Events(id)
	if err != nil {
		return false
	}
	for _, event := range events {
		if messages, recognized, err := core.UserInputMessages(event); recognized && err == nil && len(messages) > 0 && messages[0].DeliveryID == delivery {
			return true
		}
	}
	return false
}

func (a *App) chat() error {
	stopSignals := a.acquireSignals()
	defer stopSignals()
	if a.flags.json {
		a.writeJSON(map[string]any{"type": "session", "session": a.session})
	} else {
		a.line("dsgo %s · %s · %s\nWorkspace: %s\nSession: %s\n/help commands · Ctrl+C cancel turn · /quit exit\n", Version, clean(a.session.Model), clean(a.session.Permission), clean(a.session.CWD), a.session.ID)
	}
	if a.session.Provider == "deepseek-official" && a.cfg.APIKey == "" && len(a.cfg.Account) == 0 {
		a.line("Set DEEPSEEK_API_KEY or use dsgo config set api_key --stdin before sending a task.\n")
	}
	if a.flags.resume != "" {
		a.showHistory()
	}
	for {
		a.inputMu.Lock()
		if !a.flags.json {
			a.line("\n%s ", a.promptLabel("dsgo>"))
		}
		line, err := a.readLineWithOptions(a.ctx, tui.LineOptions{Prompt: a.promptLabel("dsgo>") + " ", History: true, Complete: a.completionFor(a.session.ID), DisplaySubmitted: submittedUserDisplay})
		a.inputMu.Unlock()
		eof := errors.Is(err, io.EOF)
		if err != nil && !eof {
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		for strings.HasSuffix(line, "\\") && !eof {
			line = strings.TrimSuffix(line, "\\") + "\n"
			if !a.flags.json {
				a.line("%s ", a.promptLabel("..."))
			}
			a.inputMu.Lock()
			next, x := a.readLineWithOptions(a.ctx, tui.LineOptions{Prompt: a.promptLabel("...") + " ", History: true, Complete: a.completionFor(a.session.ID), DisplaySubmitted: submittedUserDisplay})
			a.inputMu.Unlock()
			line += strings.TrimRight(next, "\r\n")
			eof = errors.Is(x, io.EOF)
			if x != nil && !eof {
				return x
			}
		}
		if strings.TrimSpace(line) == "" {
			if eof {
				return nil
			}
			continue
		}
		if strings.HasPrefix(line, "/") {
			quit, x := a.slash(line)
			if x != nil {
				a.fail(x)
			}
			if quit {
				return nil
			}
		} else if x := a.turn(line); x != nil {
			a.fail(x)
		}
		if eof {
			return nil
		}
		if err = a.ctx.Err(); err != nil {
			return err
		}
	}
}

func (a *App) showHistory() {
	messages, err := a.store.Messages(a.session.ID)
	if err != nil {
		a.fail(err)
		return
	}
	if a.flags.json {
		a.writeJSON(map[string]any{"type": "history", "session_id": a.session.ID, "messages": messages})
		return
	}
	for _, m := range messages {
		for _, b := range m.Content {
			if b.Type == "text" {
				text := b.Text
				if m.Role == "user" {
					text = tui.MaskSentUserText(text)
				}
				a.line("\n%s: %s\n", m.Role, clean(text))
			}
		}
	}
}

func submittedUserDisplay(text string) string {
	if strings.HasPrefix(strings.TrimSpace(text), "/") {
		return text
	}
	return tui.MaskSentUserText(text)
}

func (a *App) doctor() int {
	backend := "unavailable"
	if runtime.GOOS == "darwin" {
		if _, err := exec.LookPath("sandbox-exec"); err == nil {
			backend = "macOS Seatbelt (file effects)"
		}
	} else if runtime.GOOS == "linux" {
		if _, err := exec.LookPath("bwrap"); err == nil {
			backend = "Linux bubblewrap"
		}
	}
	a.print(map[string]any{"version": Version, "upstream": core.UpstreamCommit, "go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH, "home": a.flags.home, "cwd": a.flags.cwd, "api_key_configured": a.cfg.APIKey != "", "base_url": a.cfg.BaseURL, "sandbox": backend, "image_codecs": attachments.ImageCodecStatus(), "parity": "in progress; see docs/PARITY.md"})
	return 0
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

const help = `dsgo — Go terminal port of DeepSeek Harness (in development)

Usage:
  dsgo [flags]                        Interactive conversation
  dsgo run [flags] "task"              Run a task; read stdin if omitted
  dsgo --resume latest                Resume a saved session
  dsgo sessions [list|search|show|fork|export|rename|archive|unarchive|delete]
  dsgo config [show|set KEY VALUE|reset]
  dsgo auth [login|logout|status|profile]  Official DeepSeek account login
  dsgo models [account]                Inspect available model routes
  dsgo skills                         List discovered filesystem skills
  dsgo tools                          List built-in tool schemas
  dsgo doctor                         Inspect dependencies and configuration
  dsgo version

Flags: --home PATH --cwd PATH --model NAME --thinking off|low|high|max
       --permission read-only|workspace-write|full-access
       --approval on-request|never --resume ID|latest --json

Credentials: DEEPSEEK_API_KEY or DSGO_API_KEY (preferred over saved config).
             dsgo config set api_key --stdin reads a key without argv exposure.
Upstream: deepseek-ai/deepseek-harness @ 687f282e8625c6b8d11ff58211a90524be2e4317
`
