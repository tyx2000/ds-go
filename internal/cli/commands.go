package cli

import (
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

	"dsgo/internal/core"
	"dsgo/internal/state"
	"dsgo/internal/tools"
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
	if _, ok := obj[key]; !ok && key != "api_key" && key != "mcp_servers" && key != "provider_profiles" && key != "subagent_max_depth" && key != "subagent_model_selection" && key != "file_references" && key != "session_references" && key != "jobs" && key != "time_context" {
		return a.fail(fmt.Errorf("unknown config key %q", key))
	}
	switch key {
	case "max_tokens", "max_steps", "context_tokens", "subagent_max_depth", "subagent_max_active", "goal_default_max_rounds", "goal_blocked_after_rounds":
		n, err := strconv.Atoi(value)
		if err != nil {
			return a.fail(err)
		}
		obj[key] = core.JSON(n)
	case "mcp_servers", "provider_profiles", "subagent_model_selection", "file_references", "session_references", "jobs", "time_context":
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
		a.print(map[string]any{"session": s, "events": len(events), "plan": a.plan.Load()})
		return false, nil
	case "plan":
		if strings.TrimSpace(arg) == "off" {
			if a.hasDraftAttachments(a.session.ID) {
				return false, errors.New("attachments cannot accompany /plan off")
			}
			return false, a.setPlan(a.session.ID, false)
		}
		if err := a.setPlan(a.session.ID, true); err != nil {
			return false, err
		}
		if strings.TrimSpace(arg) != "" || a.hasDraftAttachments(a.session.ID) {
			return false, a.turn(arg)
		}
		a.line("Plan mode selected; /plan off to leave.\n")
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
		return false, a.jobsCommand(strings.Fields(arg))
	case "agents":
		a.print(a.manager.list(a.session.ID))
		return false, nil
	case "artifacts":
		return false, a.artifactCommand(strings.Fields(arg))
	case "trajectory", "stats":
		if cmd == "stats" {
			arg = "stats"
		}
		return false, a.trajectoryCommand(arg)
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
	tc := core.ToolContext{SessionID: a.session.ID, CWD: cwd, WorkspaceRoot: a.session.CWD, Permission: a.session.Permission, ApprovalPolicy: a.cfg.ApprovalPolicy, Approve: a.approve, Emit: func(kind string, data any) error { _, err := a.store.Append(a.session.ID, "", kind, data); return err }}
	result, err := a.registry.Execute(a.ctx, tc, name, core.JSON(input))
	if err != nil {
		return err
	}
	text := result.Content
	if rendered, ok := tools.RenderShellResult(name, text); ok {
		text = rendered
	}
	a.line("%s\n", clean(text))
	return nil
}

func (a *App) attach(path string) error {
	if path == "" {
		return errors.New("file path required")
	}
	id := a.session.ID
	if !filepath.IsAbs(path) {
		cwd, err := a.ensureWorkingDirectory(a.ctx, id)
		if err != nil {
			return err
		}
		path = filepath.Join(cwd, path)
	}
	b, err := readAttachmentFile(a.ctx, path)
	if err != nil {
		return err
	}
	kind := http.DetectContentType(b)
	if strings.HasPrefix(kind, "image/") {
		if kind != "image/png" && kind != "image/jpeg" && kind != "image/gif" && kind != "image/webp" {
			return errors.New("unsupported image type")
		}
		if err := a.checkDraftImageLimit(id, len(b)); err != nil {
			return err
		}
		if err := a.assertImageRoute(a.ctx, id, path); err != nil {
			return err
		}
		ref, err := a.attachmentStore.SaveImage(a.ctx, b, kind, filepath.Base(path))
		if err != nil {
			return err
		}
		if err := a.addDraftAttachment(id, core.Block{Type: "image", Source: ref.Source()}, len(b)); err != nil {
			return err
		}
	} else {
		if len(b) > 1<<20 {
			return errors.New("text attachment exceeds 1 MiB")
		}
		if strings.IndexByte(string(b), 0) >= 0 {
			return errors.New("binary document attachments are not yet supported")
		}
		if err := a.addDraftAttachment(id, core.Block{Type: "text", Text: fmt.Sprintf("Attached file %s:\n%s", path, b)}, 0); err != nil {
			return err
		}
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

const slashHelp = `Sessions
  /new                    Start a new session in this workspace
  /sessions [QUERY]       List sessions, or search titles and folders
  /resume [ID]            Switch to a session (default: the latest)
  /fork                   Copy this session into a new one
  /rename TITLE           Rename this session
  /archive                Archive this session and start a new one
  /history                Show this session's messages
  /export FORMAT [PATH]   Export as markdown, json or jsonl
  /status                 Show session details and plan mode

Model and behavior
  /model [NAME]           Set the model (no NAME: show settings)
  /provider [NAME]        Switch to a configured provider
  /thinking [LEVEL]       Set reasoning: off, low, high or max
  /permission [MODE]      File access: read-only, workspace-write, full-access
  /plan [off|MESSAGE]     Select plan guidance; review with exit_plan_mode

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
  /attach PATH            Attach a file or image to your next message
  /diff                   Show uncommitted Git changes
  /artifacts [ARGS]       Per-turn file changes and full tool outputs

Background work
  /jobs [ID|kill ID]      List background jobs, show one's output or stop it
  /agents                 List subagents and their status

Inspection
  /trajectory [N]         Browse turns, tool calls and timing
  /stats                  Show session statistics and token usage

  /help                   Show this list
  /quit                   Exit dsgo (also /exit)

End a line with \ to continue on the next line. Ctrl+C cancels the running
turn or clears the line being typed. Ctrl+D on an empty line exits.
`

// jobsCommand lists this session's background jobs, shows one job's retained
// output, or stops one: /jobs, /jobs ID, /jobs kill ID.
func (a *App) jobsCommand(args []string) error {
	switch {
	case len(args) == 0:
		return a.invoke("job_list", map[string]any{})
	case len(args) == 1:
		text, err := a.registry.JobText(a.session.ID, args[0])
		if err != nil {
			return err
		}
		a.line("%s\n", clean(text))
		return nil
	case len(args) == 2 && args[0] == "kill":
		return a.registry.StopJob(a.ctx, a.session.ID, args[1])
	}
	return errors.New("usage: /jobs [ID | kill ID]")
}
