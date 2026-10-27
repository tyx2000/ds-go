package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"dsgo/internal/core"
	"dsgo/internal/schedule"
	"dsgo/internal/state"
)

func (a *App) config(args []string) int {
	if len(args) == 0 || args[0] == "show" {
		a.print(a.cfg.Masked())
		return 0
	}
	if args[0] == "reset" {
		if err := state.SaveConfig(a.flags.home, state.DefaultConfig()); err != nil {
			return a.fail(err)
		}
		return 0
	}
	if args[0] != "set" || len(args) < 3 {
		return a.fail(errors.New("usage: dsgo config set KEY VALUE (or api_key --stdin)"))
	}
	key := strings.ReplaceAll(args[1], "-", "_")
	value := strings.Join(args[2:], " ")
	if value == "--stdin" {
		b, err := io.ReadAll(io.LimitReader(a.in, 1<<20))
		if err != nil {
			return a.fail(err)
		}
		value = strings.TrimSpace(string(b))
	}
	raw := core.JSON(a.cfg)
	var obj map[string]json.RawMessage
	_ = json.Unmarshal(raw, &obj)
	if _, ok := obj[key]; !ok && key != "api_key" && key != "mcp_servers" && key != "provider_profiles" && key != "subagent_max_depth" && key != "subagent_model_selection" {
		return a.fail(fmt.Errorf("unknown config key %q", key))
	}
	switch key {
	case "max_tokens", "max_steps", "context_tokens", "subagent_max_depth":
		n, err := strconv.Atoi(value)
		if err != nil {
			return a.fail(err)
		}
		obj[key] = core.JSON(n)
	case "mcp_servers", "provider_profiles", "subagent_model_selection":
		if !json.Valid([]byte(value)) {
			return a.fail(errors.New("configuration value must be JSON"))
		}
		obj[key] = json.RawMessage(value)
	default:
		obj[key] = core.JSON(value)
	}
	if err := json.Unmarshal(core.JSON(obj), &a.cfg); err != nil {
		return a.fail(err)
	}
	if err := state.SaveConfig(a.flags.home, a.cfg); err != nil {
		return a.fail(err)
	}
	fmt.Fprintf(a.out, "Saved %s.\n", key)
	return 0
}

func (a *App) sessions(args []string) error {
	if len(args) == 0 || args[0] == "list" || args[0] == "search" {
		list, err := a.store.List(true)
		if err != nil {
			return err
		}
		if len(args) > 1 {
			query := strings.ToLower(strings.Join(args[1:], " "))
			filtered := make([]core.Session, 0)
			for _, session := range list {
				if strings.Contains(strings.ToLower(session.Title), query) || strings.Contains(strings.ToLower(session.CWD), query) {
					filtered = append(filtered, session)
				}
			}
			list = filtered
		}
		if a.flags.json {
			a.print(list)
			return nil
		}
		for _, s := range list {
			mark := ""
			if s.Archived {
				mark = " [archived]"
			}
			a.line("%s  %s  %s%s\n", s.ID, s.UpdatedAt.Local().Format("2006-01-02 15:04"), clean(s.Title), mark)
		}
		return nil
	}
	if len(args) < 2 {
		return errors.New("session ID required")
	}
	id := args[1]
	switch args[0] {
	case "show":
		events, err := a.store.Events(id)
		if err != nil {
			return err
		}
		a.print(events)
		return nil
	case "fork":
		seq := int64(0)
		if len(args) > 2 {
			var err error
			seq, err = strconv.ParseInt(args[2], 10, 64)
			if err != nil {
				return err
			}
		}
		s, err := a.store.Fork(id, seq)
		if err != nil {
			return err
		}
		a.print(s)
		return nil
	case "export":
		format := "markdown"
		if len(args) > 2 {
			format = args[2]
		}
		b, err := a.store.Export(id, format)
		if err != nil {
			return err
		}
		if len(args) > 3 {
			return os.WriteFile(args[3], b, 0600)
		}
		a.line("%s", clean(string(b)))
		return nil
	case "delete":
		return a.store.Delete(id)
	case "rename", "archive", "unarchive":
		s, err := a.store.Get(id)
		if err != nil {
			return err
		}
		if args[0] == "rename" {
			if len(args) < 3 {
				return errors.New("new title required")
			}
			s.Title = strings.Join(args[2:], " ")
		} else {
			s.Archived = args[0] == "archive"
		}
		return a.store.Update(s)
	default:
		return errors.New("session action must be list, show, fork, export, rename, archive, unarchive, or delete")
	}
}

func (a *App) scheduleCommand(args []string) error {
	if len(args) == 0 || args[0] == "list" {
		tasks, err := a.schedules.Catalog(a.ctx)
		if err != nil {
			return err
		}
		a.print(tasks)
		return nil
	}
	if len(args) < 2 {
		return errors.New("usage: dsgo schedule create JSON | cancel ID")
	}
	switch args[0] {
	case "create":
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(strings.Join(args[1:], " ")), &fields); err != nil {
			return err
		}
		if _, legacy := fields["kind"]; !legacy {
			var sessionID string
			if err := json.Unmarshal(fields["session_id"], &sessionID); err != nil {
				return errors.New("session_id is required")
			}
			delete(fields, "session_id")
			request, err := schedule.DecodeCreate(core.JSON(fields))
			if err != nil {
				return err
			}
			view, err := a.schedules.CreateOfficial(a.ctx, sessionID, request)
			if err != nil {
				return err
			}
			a.print(view)
			return nil
		}
		var t schedule.Task
		if err := json.Unmarshal([]byte(strings.Join(args[1:], " ")), &t); err != nil {
			return err
		}
		if _, err := a.store.Get(t.SessionID); err != nil {
			return err
		}
		t, err := a.schedules.Create(t)
		if err != nil {
			return err
		}
		a.print(t)
		return nil
	case "cancel":
		return a.schedules.Cancel(args[1])
	case "get", "history", "delete":
		if len(args) < 3 {
			return errors.New("session ID and reminder ID are required")
		}
		sessionID, id := args[1], args[2]
		if args[0] == "delete" {
			result, err := a.schedules.DeleteOfficial(a.ctx, sessionID, id)
			if err != nil {
				return err
			}
			a.print(result)
			return nil
		}
		if args[0] == "get" {
			record, err := a.schedules.GetRecord(a.ctx, sessionID, id)
			if err != nil {
				return err
			}
			a.print(record)
			return nil
		}
		limit := 50
		if len(args) > 3 {
			n, err := strconv.Atoi(args[3])
			if err != nil {
				return err
			}
			limit = n
		}
		before := ""
		if len(args) > 4 {
			before = args[4]
		}
		history, err := a.schedules.History(a.ctx, sessionID, id, limit, before)
		if err != nil {
			return err
		}
		a.print(history)
		return nil
	case "update":
		var p struct {
			SessionID string               `json:"session_id"`
			ID        string               `json:"id"`
			Expected  schedule.Record      `json:"expected"`
			Patch     schedule.UpdatePatch `json:"patch"`
		}
		if err := json.Unmarshal([]byte(strings.Join(args[1:], " ")), &p); err != nil {
			return err
		}
		if p.SessionID == "" || p.ID == "" || p.Expected.ID == "" {
			return errors.New("update requires session_id, id, expected full record, and patch")
		}
		result, err := a.schedules.UpdateOfficial(a.ctx, p.SessionID, p.ID, p.Expected, p.Patch)
		if err != nil {
			return err
		}
		if result.Updated && result.View != nil {
			a.print(result.View)
		} else {
			a.print(result)
		}
		return nil
	default:
		return errors.New("unknown schedule action")
	}
}

func (a *App) slash(line string) (bool, error) {
	cmd, arg, _ := strings.Cut(strings.TrimPrefix(line, "/"), " ")
	arg = strings.TrimSpace(arg)
	switch cmd {
	case "quit", "exit":
		return true, nil
	case "help":
		a.line(slashHelp)
		return false, nil
	case "sessions":
		if arg != "" {
			return false, a.sessions([]string{"search", arg})
		}
		return false, a.sessions(nil)
	case "new":
		return false, a.selectSession("")
	case "resume":
		if arg == "" {
			arg = "latest"
		}
		if err := a.selectSession(arg); err != nil {
			return false, err
		}
		a.showHistory()
		return false, nil
	case "fork":
		s, err := a.store.Fork(a.session.ID, 0)
		if err != nil {
			return false, err
		}
		return false, a.selectSession(s.ID)
	case "rename":
		if arg == "" {
			return false, errors.New("title required")
		}
		a.session.Title = arg
		return false, a.store.Update(a.session)
	case "archive":
		a.session.Archived = true
		if err := a.store.Update(a.session); err != nil {
			return false, err
		}
		return false, a.selectSession("")
	case "history":
		a.showHistory()
		return false, nil
	case "export":
		args := []string{"export", a.session.ID}
		args = append(args, strings.Fields(arg)...)
		return false, a.sessions(args)
	case "model", "thinking", "permission", "provider":
		if arg == "" {
			a.print(a.session)
			return false, nil
		}
		next := a.session
		cfg := a.cfg
		switch cmd {
		case "model":
			next.Model = arg
			cfg.Model = arg
		case "thinking":
			next.Thinking = arg
			cfg.Thinking = arg
		case "permission":
			next.Permission = arg
			cfg.Permission = arg
		case "provider":
			if arg != "deepseek-official" {
				if _, ok := cfg.ProviderProfiles[arg]; !ok {
					return false, errors.New("provider is not configured")
				}
			}
			next.Provider = arg
		}
		cfg.SelectedProvider = next.Provider
		if err := cfg.Validate(); err != nil {
			return false, err
		}
		if err := a.store.Update(next); err != nil {
			return false, err
		}
		a.session = next
		if cmd == "permission" {
			a.registry.InvalidateSession(next.ID)
		}
		return false, nil
	case "mode":
		if arg == "" {
			a.line("%s\n", a.sessionMode(a.session))
			return false, nil
		}
		if arg != "native" && arg != "ptc" && arg != "ptc-repl" {
			return false, errors.New("mode must be native, ptc, or ptc-repl")
		}
		if sessionPreset(a.session) == "minimal" && arg != "native" {
			return false, errors.New("minimal uses its native persistent bash tool")
		}
		next := a.session
		next.ExecutionMode = arg
		if err := a.store.Update(next); err != nil {
			return false, err
		}
		a.registry.InvalidateSession(next.ID)
		a.session = next
		return false, nil
	case "preset":
		return false, a.presetCommand(arg)
	case "cwd":
		input := map[string]any{}
		if arg != "" {
			input["cd"] = arg
		}
		return false, a.invoke("working_directory", input)
	case "compact":
		return false, a.engine.Compact(a.ctx, a.session.ID)
	case "continue":
		return false, a.turn("Continue from the current session state and unfinished work.")
	case "status":
		s, err := a.store.Get(a.session.ID)
		if err != nil {
			return false, err
		}
		events, err := a.store.Events(s.ID)
		if err != nil {
			return false, err
		}
		a.print(map[string]any{"session": s, "events": len(events), "plan": a.plan.Load(), "mode": a.sessionMode(s), "preset": sessionPreset(s)})
		return false, nil
	case "plan":
		switch arg {
		case "on":
			return false, a.setPlan(a.session.ID, true)
		case "off":
			return false, a.setPlan(a.session.ID, false)
		default:
			a.line("plan=%v; /plan on or /plan off\n", a.plan.Load())
		}
		return false, nil
	case "skills":
		if arg == "" {
			a.print(a.skills)
			return false, nil
		}
		text, err := a.loadSkill(arg)
		if err != nil {
			return false, err
		}
		a.line("%s\n", clean(text))
		return false, nil
	case "attach":
		return false, a.attach(arg)
	case "files":
		return false, a.invoke("list_directory", map[string]any{"path": arg})
	case "read":
		return false, a.invoke("read", map[string]any{"file_path": arg})
	case "diff":
		return false, a.diff()
	case "jobs":
		if arg == "" {
			return false, a.invoke("jobs", map[string]any{"action": "list"})
		}
		var p map[string]any
		if err := json.Unmarshal([]byte(arg), &p); err != nil {
			return false, err
		}
		return false, a.invoke("jobs", p)
	case "terminal":
		if arg == "" {
			return false, a.invoke("terminal", map[string]any{"action": "list"})
		}
		var p map[string]any
		if err := json.Unmarshal([]byte(arg), &p); err != nil {
			return false, err
		}
		return false, a.invoke("terminal", p)
	case "agents":
		a.print(a.manager.list(a.session.ID))
		return false, nil
	case "workflows":
		return false, a.workflowCommand(strings.Fields(arg))
	case "artifacts":
		return false, a.artifactCommand(strings.Fields(arg))
	case "preview":
		return false, a.previewCommand(arg)
	case "trajectory", "stats":
		if cmd == "stats" {
			arg = "stats"
		}
		return false, a.trajectoryCommand(arg)
	case "schedules":
		return false, a.scheduleCommand(nil)
	case "goal":
		return false, a.goalCommand(arg)
	case "todo":
		return false, a.todoCommand(arg)
	default:
		return false, fmt.Errorf("unknown command /%s; see /help", cmd)
	}
}

func (a *App) invoke(name string, input any) error {
	cwd, err := a.ensureWorkingDirectory(a.ctx, a.session.ID)
	if err != nil {
		return err
	}
	tc := core.ToolContext{SessionID: a.session.ID, CWD: cwd, WorkspaceRoot: a.session.CWD, ExecutionMode: a.sessionMode(a.session), Permission: a.session.Permission, ApprovalPolicy: a.cfg.ApprovalPolicy, Approve: a.approve, Emit: func(kind string, data any) error { _, err := a.store.Append(a.session.ID, "", kind, data); return err }}
	result, err := a.registry.Execute(a.ctx, tc, name, core.JSON(input))
	if err != nil {
		return err
	}
	a.line("%s\n", clean(result.Content))
	return nil
}

func (a *App) attach(path string) error {
	if path == "" {
		return errors.New("file path required")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(a.session.CWD, path)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 20<<20 {
		return errors.New("attachment must be a regular file of at most 20 MiB")
	}
	b, err := io.ReadAll(io.LimitReader(f, 20<<20+1))
	if err != nil {
		return err
	}
	kind := http.DetectContentType(b)
	if strings.HasPrefix(kind, "image/") {
		if kind != "image/png" && kind != "image/jpeg" && kind != "image/gif" && kind != "image/webp" {
			return errors.New("unsupported image type")
		}
		count, total := 0, 0
		for _, block := range a.attachments {
			if block.Type == "image" {
				count++
				if source, ok := block.Source.(map[string]any); ok {
					switch size := source["bytes"].(type) {
					case int:
						total += size
					case int64:
						total += int(size)
					case float64:
						total += int(size)
					}
				}
			}
		}
		if count >= 20 || total+len(b) > 200<<20 {
			return errors.New("message image attachments exceed 20 images or 200 MiB")
		}
		if err := a.assertImageRoute(a.ctx, a.session.ID, path); err != nil {
			return err
		}
		ref, err := a.attachmentStore.SaveImage(a.ctx, b, kind, filepath.Base(path))
		if err != nil {
			return err
		}
		a.attachments = append(a.attachments, core.Block{Type: "image", Source: ref.Source()})
	} else {
		if len(b) > 1<<20 {
			return errors.New("text attachment exceeds 1 MiB")
		}
		if strings.IndexByte(string(b), 0) >= 0 {
			return errors.New("binary document attachments are not yet supported")
		}
		a.attachments = append(a.attachments, core.Block{Type: "text", Text: fmt.Sprintf("Attached file %s:\n%s", path, b)})
	}
	a.line("Attached %s to the next message.\n", clean(path))
	return nil
}

func (a *App) diff() error {
	cmd := exec.CommandContext(a.ctx, "git", "-C", a.session.CWD, "diff", "--no-ext-diff", "--no-color", "--")
	b, err := cmd.Output()
	if err != nil {
		return err
	}
	if len(b) > 2<<20 {
		b = b[:2<<20]
		b = append(b, []byte("\n[diff truncated]\n")...)
	}
	a.line("%s", clean(string(b)))
	return nil
}

func (a *App) daemon() error {
	ctx, cancel := signalContext(a.ctx)
	defer func() { cancel(); a.background.Wait() }()
	a.line("dsgo schedule daemon: %s\n", a.flags.home)
	return a.schedules.RunWithStart(ctx, a.recoverScheduled, func(deliveryCtx context.Context, t schedule.Task) error {
		session, err := a.store.Get(t.SessionID)
		if err != nil {
			return err
		}
		if session.Archived {
			return errors.New("target session is archived")
		}
		accepted, err := a.hasDelivery(t.SessionID, t.Pending)
		if err != nil {
			return err
		}
		if accepted {
			return a.acknowledgeDelivery(ctx, t)
		}
		admitted := make(chan struct{})
		a.receipts.Store(t.Pending, admitted)
		defer a.receipts.Delete(t.Pending)
		prompt := t.Prompt
		if t.PendingPrompt != nil {
			prompt = *t.PendingPrompt
		}
		input := core.TextMessage("user", "Scheduled reminder ("+t.PendingAt.UTC().Format(time.RFC3339)+"):\n"+prompt)
		input.DeliveryID = t.Pending
		done := make(chan error, 1)
		turnCtx, cancelTurn := context.WithCancel(ctx)
		a.background.Add(1)
		go func() {
			defer a.background.Done()
			defer cancelTurn()
			err := a.engine.Run(turnCtx, t.SessionID, input)
			if err != nil && !errors.Is(err, context.Canceled) {
				a.line("Schedule %s turn stopped: %s\n", t.ID, clean(err.Error()))
			}
			done <- err
		}()
		select {
		case <-admitted:
			return a.acknowledgeDelivery(ctx, t)
		case err := <-done:
			accepted, readErr := a.hasDelivery(t.SessionID, t.Pending)
			if readErr != nil {
				return readErr
			}
			if accepted {
				return a.acknowledgeDelivery(ctx, t)
			}
			return err
		case <-deliveryCtx.Done():
			select {
			case <-admitted:
				return a.acknowledgeDelivery(ctx, t)
			default:
				cancelTurn()
				return deliveryCtx.Err()
			}
		}
	})
}

func (a *App) hasDelivery(id, delivery string) (bool, error) {
	events, err := a.store.Events(id)
	if err != nil {
		return false, err
	}
	for _, ev := range events {
		if ev.Type == "user/message" {
			var m core.Message
			if json.Unmarshal(ev.Data, &m) == nil && m.DeliveryID == delivery {
				return true, nil
			}
		}
	}
	return false, nil
}

func (a *App) recoverScheduled(ctx context.Context) error {
	sessions, err := a.store.List(false)
	if err != nil {
		return err
	}
	for _, session := range sessions {
		events, err := a.store.Events(session.ID)
		if err != nil {
			return err
		}
		latestTurn := ""
		finished := map[string]bool{}
		var lastUser core.Message
		for _, ev := range events {
			if ev.Type == "turn/started" {
				latestTurn = ev.TurnID
			}
			if ev.Type == "turn/finished" {
				finished[ev.TurnID] = true
			}
			if ev.Type == "user/message" {
				_ = json.Unmarshal(ev.Data, &lastUser)
			}
		}
		if latestTurn != "" && !finished[latestTurn] && lastUser.DeliveryID != "" {
			id := session.ID
			a.background.Add(1)
			go func() {
				defer a.background.Done()
				if err := a.engine.Continue(ctx, id); err != nil && !errors.Is(err, context.Canceled) {
					a.line("Scheduled session %s recovery stopped: %s\n", id, clean(err.Error()))
				}
			}()
		}
	}
	return nil
}

const slashHelp = `Sessions
  /new                    Start a new session in this workspace
  /sessions [QUERY]       List sessions, or search titles and folders
  /resume [ID]            Switch to a session (default: the latest)
  /fork                   Copy this session into a new one
  /rename TITLE           Rename this session
  /archive                Archive this session and start a new one
  /history                Show this session's messages
  /export FORMAT [PATH]   Export as markdown, json or jsonl
  /status                 Show session details, mode and preset

Model and behavior
  /model [NAME]           Set the model (no NAME: show settings)
  /provider [NAME]        Switch to a configured provider
  /thinking [LEVEL]       Set reasoning: off, low, high or max
  /permission [MODE]      File access: read-only, workspace-write, full-access
  /preset [NAME]          Show or set: standard, ptc, minimal or cordis
  /mode [MODE]            Code execution: native, ptc or ptc-repl
  /plan [on|off]          Plan mode: no changes until you approve

Context and tasks
  /compact                Summarize history to free context space
  /continue               Ask the model to continue unfinished work
  /goal TEXT              Set a goal the agent keeps working toward
  /goal ACTION            Goal control: status, pause, resume or clear
  /todo                   Show the current todo list
  /skills [NAME]          List skills, or show one skill's instructions

Files and workspace
  /cwd [PATH]             Show or change the working directory
  /files [PATH]           List a directory
  /read PATH              Show a text file with line numbers
  /preview PATH [JSON]    Preview documents, images, sheets, HTML, PDF
  /attach PATH            Attach a file or image to your next message
  /diff                   Show uncommitted Git changes
  /artifacts [ARGS]       Deliverables, per-turn changes, full outputs

Background work
  /jobs [JSON]            List background jobs, or act on one (JSON)
  /terminal [JSON]        List terminals, or start/read/write/stop one
  /agents                 List subagents and their status
  /workflows [RUN_ID]     Show workflow runs (cancel RUN_ID stops one)
  /schedules              List saved reminders

Inspection
  /trajectory [N]         Browse turns, tool calls and timing
  /stats                  Show session statistics and token usage

  /help                   Show this list
  /quit                   Exit dsgo (also /exit)

End a line with \ to continue on the next line. Ctrl+C cancels the running
turn or clears the line being typed. Ctrl+D on an empty line exits.
`
