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
	"dsgo/internal/schedule"
	"dsgo/internal/state"
	"dsgo/internal/tools"
	"dsgo/internal/tui"
	"dsgo/internal/workflow"
)

const Version = "0.1.0-dev"

type flags struct {
	home, cwd, model, provider, thinking, permission, approval, mode, preset, resume string
	json                                                                             bool
}

func (f *flags) bind(fs *flag.FlagSet) {
	fs.StringVar(&f.home, "home", f.home, "data/config directory (DSGO_HOME)")
	fs.StringVar(&f.cwd, "cwd", f.cwd, "workspace directory")
	fs.StringVar(&f.model, "model", f.model, "model name")
	fs.StringVar(&f.provider, "provider", f.provider, "configured provider profile (default deepseek-official)")
	fs.StringVar(&f.thinking, "thinking", f.thinking, "off, low, high, max")
	fs.StringVar(&f.permission, "permission", f.permission, "read-only, workspace-write, full-access")
	fs.StringVar(&f.approval, "approval", f.approval, "on-request, never")
	fs.StringVar(&f.mode, "mode", f.mode, "native, ptc, ptc-repl")
	fs.StringVar(&f.preset, "preset", f.preset, "standard, ptc, minimal, cordis")
	fs.StringVar(&f.resume, "resume", f.resume, "session ID or latest")
	fs.BoolVar(&f.json, "json", f.json, "newline-delimited JSON output")
}

type App struct {
	in              *bufio.Reader
	out, err        io.Writer
	flags           flags
	store           *state.Store
	cfg             state.Config
	registry        *tools.Registry
	engine          *agent.Engine
	schedules       *schedule.Store
	skills          []instructions.Skill
	session         core.Session
	writeMu         sync.Mutex
	editor          *tui.LineEditor // nil unless stdin and stdout are terminals
	inputMu         sync.Mutex
	inputOnce       sync.Once
	reads           chan inputRequest
	manager         *agents
	ctx             context.Context
	cancel          context.CancelFunc
	multiline       bool
	plan            atomic.Bool
	current         atomic.Value
	attachments     []core.Block
	attachmentStore *attachments.Store
	mcpClients      map[string]*mcp.Client
	mcpInstructions string
	unattended      bool
	receipts        sync.Map
	background      sync.WaitGroup
	streamKinds     sync.Map
	workflows       *workflow.Engine
	workflowNotices workflowJobNotices
	signalMu        sync.Mutex
	signalRefs      int
	signalStop      func()
	foreground      sync.Map // session id -> *foregroundTurn
}

type inputLine struct {
	text string
	err  error
}
type inputRequest struct{ reply chan inputLine }

func Main(args []string, in io.Reader, out, errOut io.Writer) int {
	a := &App{in: bufio.NewReader(in), out: out, err: errOut, flags: flags{home: state.DefaultRoot()}}
	// Interactive prompts edit whole characters; pipes keep line reading.
	if tty, ok := in.(*os.File); ok && tui.IsTerminal(tty) {
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
	a.unattended = command == "run" || command == "daemon"
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
	if a.flags.mode != "" {
		a.cfg.ExecutionMode = a.flags.mode
	}
	if a.flags.preset != "" {
		a.cfg.AgentPreset = a.flags.preset
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
	a.schedules, err = schedule.Open(root)
	if err != nil {
		return a.fail(err)
	}
	if err = a.configureSchedules(); err != nil {
		return a.fail(err)
	}
	if command == "schedule" || command == "schedules" {
		if err = a.scheduleCommand(args); err != nil {
			return a.fail(err)
		}
		return 0
	}
	a.skills, err = instructions.Skills(a.flags.cwd)
	if err != nil {
		return a.fail(err)
	}
	if command == "skills" {
		a.print(a.skills)
		return 0
	}
	if command != "chat" && command != "run" && command != "daemon" && command != "tools" {
		return a.fail(fmt.Errorf("unknown command %q; run dsgo help", command))
	}
	system, err := instructions.System(a.flags.cwd, a.skills)
	if err != nil {
		return a.fail(err)
	}
	a.registry = tools.New()
	defer a.registry.Close()
	a.registry.SetDispatcher(a.Execute)
	a.registry.SetNodePath(a.cfg.NodePath)
	a.registry.SetSpillDir(filepath.Join(root, "artifacts", "shell-output"))
	ptcMode := "per-call"
	if a.cfg.ExecutionMode == "ptc-repl" {
		ptcMode = "persistent-in-memory"
	}
	if err = a.registry.SetPTCMode(ptcMode); err != nil {
		return a.fail(err)
	}
	models, err := buildModels(a.cfg)
	if err != nil {
		return a.fail(err)
	}
	a.attachmentStore, err = attachments.Open(root)
	if err != nil {
		return a.fail(err)
	}
	models = a.attachmentStore.Resolve(models)
	a.engine = agent.New(a.store, models, a, agent.Options{Provider: a.cfg.SelectedProvider, Model: a.cfg.Model, Thinking: a.cfg.Thinking, MaxTokens: a.cfg.MaxTokens, MaxSteps: a.cfg.MaxSteps, ContextTokens: a.cfg.ContextTokens, ApprovalPolicy: a.cfg.ApprovalPolicy, ExecutionMode: a.cfg.ExecutionMode, System: system, Approve: a.approve, SystemFor: a.systemFor, ToolsFor: a.specsFor, RequestContext: a.requestContext})
	a.engine.Notify = a.notice
	a.engine.UpdateOptions(func(o *agent.Options) {
		o.BeforeTurn = a.workspaceTurnStarted
		o.AfterTurn = a.workspaceTurnFinished
		o.RenderToolResult = a.spillToolResult
	})
	defer a.engine.Close()
	a.manager = newAgents(a)
	defer a.manager.close()
	a.mcpClients = map[string]*mcp.Client{}
	defer func() {
		for _, client := range a.mcpClients {
			_ = client.Close()
		}
	}()
	if err = a.registerTools(); err != nil {
		return a.fail(err)
	}
	defer func() {
		if a.workflows != nil {
			a.workflows.Close()
		}
	}()
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
	if command == "daemon" {
		if len(args) > 0 && args[0] != "run" {
			return a.fail(errors.New("usage: dsgo daemon run"))
		}
		if err = a.daemon(); err != nil && !errors.Is(err, context.Canceled) {
			return a.fail(err)
		}
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

func (a *App) fail(err error) int { fmt.Fprintf(a.err, "dsgo: %s\n", clean(err.Error())); return 1 }
func (a *App) print(v any) {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Fprintln(a.out, clean(string(b)))
}
func (a *App) line(format string, args ...any) {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	fmt.Fprintf(a.out, format, args...)
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
	var err error
	if id == "latest" {
		sessions, x := a.store.List(false)
		if x != nil {
			return x
		}
		if len(sessions) == 0 {
			return errors.New("no session to resume")
		}
		id = sessions[0].ID
	}
	if id == "" {
		a.session, err = a.store.Create(a.flags.cwd, "New session")
		if err != nil {
			return err
		}
		a.current.Store(a.session.ID)
		a.plan.Store(a.session.Plan)
		a.session.Permission = a.cfg.Permission
		a.session.Model = a.cfg.Model
		a.session.Provider = a.cfg.SelectedProvider
		a.session.Thinking = a.cfg.Thinking
		a.session.Preset = a.cfg.AgentPreset
		a.session.ExecutionMode = a.cfg.ExecutionMode
		if a.flags.mode == "" && a.cfg.AgentPreset == "ptc" {
			a.session.ExecutionMode = "ptc"
		}
		if a.cfg.AgentPreset == "minimal" {
			a.session.ExecutionMode = "native"
		}
		if err = a.store.Update(a.session); err != nil {
			return err
		}
		// The child-route preference is sampled once, while the Session is new.
		return a.sampleSubagentPolicy(a.session.ID)
	}
	a.session, err = a.store.Get(id)
	if err != nil {
		return err
	}
	if a.session.Archived {
		return errors.New("session is archived; unarchive it before resuming")
	}
	a.current.Store(a.session.ID)
	a.plan.Store(a.session.Plan)
	if a.flags.permission != "" {
		a.session.Permission = a.flags.permission
	}
	if a.flags.model != "" {
		a.session.Model = a.flags.model
	}
	if a.flags.provider != "" {
		a.session.Provider = a.flags.provider
	}
	if a.flags.thinking != "" {
		a.session.Thinking = a.flags.thinking
	}
	if a.flags.preset != "" {
		a.session.Preset = a.flags.preset
		a.session.ExecutionMode = "native"
		if a.flags.preset == "ptc" {
			a.session.ExecutionMode = "ptc"
		}
	}
	if a.flags.mode != "" {
		a.session.ExecutionMode = a.flags.mode
	}
	if sessionPreset(a.session) == "minimal" && a.sessionMode(a.session) != "native" {
		return errors.New("minimal uses its native persistent bash tool")
	}
	if err = a.store.Update(a.session); err != nil {
		return err
	}
	skills, err := instructions.Skills(a.session.CWD)
	if err != nil {
		return err
	}
	a.skills = skills
	return nil
}

func (a *App) notice(n agent.Notice) {
	if n.Type == "event" {
		if event, ok := n.Data.(core.Event); ok && event.Type == "user/message" {
			var m core.Message
			if json.Unmarshal(event.Data, &m) == nil && m.DeliveryID != "" {
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
	answer, err := a.readLine(ctx)
	if err != nil {
		return false, err
	}
	if err = ctx.Err(); err != nil {
		return false, err
	}
	return strings.EqualFold(strings.TrimSpace(answer), "y") || strings.EqualFold(strings.TrimSpace(answer), "yes"), nil
}

func (a *App) readLine(ctx context.Context) (string, error) {
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
						s, err = a.editor.ReadLine()
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
	case a.reads <- inputRequest{reply: done}:
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
	if a.manager != nil {
		a.manager.resumeWakes(a.session.ID)
	}
	input := core.TextMessage("user", prompt)
	input.Content = append(input.Content, a.attachments...)
	input.DeliveryID = "input:" + agent.ID()
	ctx, cancel := context.WithCancel(a.ctx)
	defer cancel()
	owner := &foregroundTurn{cancel: cancel}
	a.foreground.Store(a.session.ID, owner)
	defer a.foreground.CompareAndDelete(a.session.ID, owner)
	done := make(chan error, 1)
	go func() { done <- a.engine.Run(ctx, a.session.ID, input) }()
	for {
		select {
		case err := <-done:
			if errors.Is(err, agent.ErrBusy) {
				// Another automatic owner may have won the admission race. The
				// full user input is committed before the composer clears media.
				if _, queueErr := a.manager.deliver(ctx, a.session.ID, input, false); queueErr != nil {
					return queueErr
				}
				a.attachments = nil
				a.line("\n[Message queued for the active session]\n")
				return nil
			}
			if a.inputAdmitted(a.session.ID, input.DeliveryID) {
				a.attachments = nil
			}
			if err != nil {
				return err
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continuation, x := a.goalContinuation()
			if x != nil {
				return x
			}
			if continuation == "" {
				return nil
			}
			a.line("\n[Continuing active goal]\n")
			go func() { done <- a.engine.Run(ctx, a.session.ID, core.TextMessage("user", continuation)) }()
		}
	}
}

func (a *App) inputAdmitted(id, delivery string) bool {
	events, err := a.store.Events(id)
	if err != nil {
		return false
	}
	for _, event := range events {
		if event.Type == "user/message" {
			var message core.Message
			if json.Unmarshal(event.Data, &message) == nil && message.DeliveryID == delivery {
				return true
			}
		}
	}
	return false
}

func (a *App) chat() error {
	stopSignals := a.acquireSignals()
	defer stopSignals()
	a.line("dsgo %s · %s · %s\nWorkspace: %s\nSession: %s\n/help commands · Ctrl+C cancel turn · /quit exit\n", Version, clean(a.session.Model), clean(a.session.Permission), clean(a.session.CWD), a.session.ID)
	if a.session.Provider == "deepseek-official" && a.cfg.APIKey == "" && len(a.cfg.Account) == 0 {
		a.line("Set DEEPSEEK_API_KEY or use dsgo config set api_key --stdin before sending a task.\n")
	}
	if a.flags.resume != "" {
		a.showHistory()
	}
	for {
		a.inputMu.Lock()
		a.line("\ndsgo> ")
		line, err := a.readLine(a.ctx)
		a.inputMu.Unlock()
		eof := errors.Is(err, io.EOF)
		if err != nil && !eof {
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		for strings.HasSuffix(line, "\\") && !eof {
			line = strings.TrimSuffix(line, "\\") + "\n"
			a.line("... ")
			a.inputMu.Lock()
			next, x := a.readLine(a.ctx)
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
	for _, m := range messages {
		for _, b := range m.Content {
			if b.Type == "text" {
				a.line("\n%s: %s\n", m.Role, clean(b.Text))
			}
		}
	}
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
	node, err := exec.LookPath(a.cfg.NodePath)
	if err != nil {
		node = "unavailable (PTC requires Node >=22.13)"
	}
	a.print(map[string]any{"version": Version, "upstream": core.UpstreamCommit, "go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH, "home": a.flags.home, "cwd": a.flags.cwd, "api_key_configured": a.cfg.APIKey != "", "base_url": a.cfg.BaseURL, "sandbox": backend, "node": node, "image_codecs": attachments.ImageCodecStatus(), "preview": a.previewDoctorInfo(), "parity": "in progress; see docs/PARITY.md"})
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
  dsgo schedule [list|create JSON|update JSON|get|history|delete]
  dsgo daemon run                     Deliver persistent schedules
  dsgo skills                         List discovered filesystem skills
  dsgo tools                          List built-in tool schemas
  dsgo doctor                         Inspect dependencies and configuration
  dsgo version

Flags: --home PATH --cwd PATH --model NAME --thinking off|low|high|max
       --permission read-only|workspace-write|full-access
       --approval on-request|never --preset standard|ptc|minimal|cordis
       --mode native|ptc|ptc-repl --resume ID|latest --json

Credentials: DEEPSEEK_API_KEY or DSGO_API_KEY (preferred over saved config).
             dsgo config set api_key --stdin reads a key without argv exposure.
Upstream: deepseek-ai/deepseek-harness @ 687f282e8625c6b8d11ff58211a90524be2e4317
`
