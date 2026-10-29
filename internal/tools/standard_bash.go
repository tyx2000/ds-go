package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"dsgo/internal/core"
)

// The standard, ptc and cordis presets use the pinned official tool-bash and
// tool-jobs contracts (DeepSeek Harness 687f282e). Each bash call is a fresh
// `bash -c`; every command is a job from its start, and a foreground command
// that outlives its timeout keeps running as that job. The minimal preset's
// persistent shell is a separate tool.
const (
	standardBashTimeoutMS    = 120000
	standardBashMaxTimeoutMS = 600000
	standardStreamBytes      = 64000
	standardSpillBytes       = 64 << 20
	jobWaitMS                = 30000
	jobMaxWaitMS             = 600000
)

const standardBashDescription = "Execute a bash command (`bash -c`) and return its stdout/stderr. " +
	"Each call runs in a fresh shell; pass `workdir` instead of using `cd`. " +
	"Long output is truncated to its tail; the full output is saved to a file whose path is reported when available. " +
	"Provide `description` before `command` in the arguments. " +
	"Before any delete or move, verify that the resolved absolute target path is the intended one; never run it against a computed path you have not checked. " +
	"An unset variable expands to an empty string, so guard variables in such paths with `${VAR:?}`. " +
	"Commands may run under a file sandbox; a blocked file operation is reported as `[sandbox: file access denied under <mode> mode]`, a policy denial: do not retry another way."

const standardBashSchema = `{"type":"object","additionalProperties":false,"required":["description","command"],"properties":{` +
	`"description":{"type":"string","description":"Clear, concise description of what this command does in active voice, 5-10 words (shown in the UI). Examples: \"ls\" → \"List files in current directory\"; \"git status\" → \"Show working tree status\"; \"npm install\" → \"Install package dependencies\"."},` +
	`"command":{"type":"string","description":"The bash command to execute."},` +
	`"timeoutMs":{"type":"number","description":"Timeout in milliseconds. The executor applies its configured default and cap; on expiry the command moves to the background as a job instead of being killed."},` +
	`"workdir":{"type":"string","description":"Working directory for this command. Defaults to the session workspace; a relative path is resolved against it."},` +
	`"run_in_background":{"type":"boolean","description":"Run in the background and return a job id immediately (collect with job_output, stop with job_kill). No timeout applies."},` +
	`"sandbox_permissions":{"type":"string","enum":["workspace-write","danger-full-access"],"description":"The narrowest wider sandbox mode for a one-shot retry of the exact command the sandbox just denied; the retry asks the user for approval."},` +
	`"justification":{"type":"string","description":"Required with sandbox_permissions: one sentence for the user explaining why this exact command needs the wider access. Use the language of the user’s current request."}}}`

const jobIDDescription = `"Job id returned by the tool that started the background work."`
const escalationHint = "[sandbox: escalation available — retry this exact command once with sandbox_permissions (the narrowest wider mode that suffices) + justification; the approval prompt asks the user]"

func (r *Registry) standardShell() {
	for _, d := range []struct {
		s core.ToolSpec
		h Handler
	}{
		{spec("standard_bash", standardBashDescription, standardBashSchema), r.standardBash},
		{spec("job_output", "Read a background job: output since the previous read for stream jobs, or the result of a finished final-output job.", `{"type":"object","additionalProperties":false,"required":["job_id"],"properties":{"job_id":{"type":"string","description":`+jobIDDescription+`},"wait":{"type":"boolean","description":"Block until the job finishes or the timeout expires; a timed-out wait leaves the job running. Defaults to false."},"timeout_ms":{"type":"number","description":"Max wait in milliseconds with wait: true. Defaults to and is capped by configuration."}}}`), r.jobOutput},
		{spec("job_list", "List your background jobs (running and finished) with their ids, kinds, and statuses.", `{"type":"object","additionalProperties":false,"properties":{}}`), r.jobList},
		{spec("job_kill", "Request cancellation of a running background job.", `{"type":"object","additionalProperties":false,"required":["job_id"],"properties":{"job_id":{"type":"string","description":`+jobIDDescription+`},"reason":{"type":"string","description":"Optional short reason, recorded in the log and forwarded to the job."}}}`), r.jobKill},
	} {
		if err := r.Register(d.s, d.h); err != nil {
			panic(err)
		}
	}
}

// SetSpillDir selects where complete bash streams are kept once their in-memory
// tail overflows. Empty reports the full output as unavailable.
func (r *Registry) SetSpillDir(dir string) {
	r.mu.Lock()
	r.spillDir = dir
	r.mu.Unlock()
}

// streamCapture keeps the tail of one stream in memory and, after the first
// overflow, the complete stream in a private file. Crossing the spill cap
// revokes that file rather than advertising a prefix as complete output.
type streamCapture struct {
	mu        sync.Mutex
	dir       string
	tail      []byte
	total     int64
	spill     *os.File
	spillPath string
	spilled   int64
	failed    bool
}

func (s *streamCapture) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if int64(len(p)) > standardSpillBytes-s.total {
		// The public spillPath means every byte is recoverable. Once the
		// bounded file cannot provide that guarantee, remove it and keep only
		// the ordinary in-memory tail. Never reopen it for this stream.
		s.dropSpill()
	}
	if s.spill == nil && !s.failed && s.total+int64(len(p)) > standardStreamBytes {
		// Before the first overflow the tail still holds every byte written.
		s.openSpill()
	}
	if s.spill != nil && s.spilled < standardSpillBytes {
		n := min(int64(len(p)), standardSpillBytes-s.spilled)
		if _, err := s.spill.Write(p[:n]); err != nil {
			s.dropSpill()
		} else {
			s.spilled += n
		}
	}
	s.tail = append(s.tail, p...)
	if len(s.tail) > 2*standardStreamBytes {
		s.tail = append([]byte(nil), s.tail[len(s.tail)-standardStreamBytes:]...)
	}
	s.total += int64(len(p))
	return len(p), nil
}

func (s *streamCapture) openSpill() {
	s.failed = true
	if s.dir == "" || os.MkdirAll(s.dir, 0700) != nil {
		return
	}
	f, err := os.CreateTemp(s.dir, "bash-*.out")
	if err != nil {
		return
	}
	s.spill, s.spillPath, s.failed = f, f.Name(), false
	if _, err = f.Write(s.tail); err != nil {
		s.dropSpill()
		return
	}
	s.spilled = int64(len(s.tail))
}

func (s *streamCapture) dropSpill() {
	if s.spill != nil {
		_ = s.spill.Close()
	}
	if s.spillPath != "" {
		_ = os.Remove(s.spillPath)
	}
	s.spill, s.spillPath, s.failed = nil, "", true
}

func (s *streamCapture) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.spill != nil {
		if err := s.spill.Sync(); err != nil {
			s.dropSpill()
			return
		}
		if err := s.spill.Close(); err != nil {
			s.dropSpill()
			return
		}
		s.spill = nil
	}
}

// window returns the retained tail and the absolute offset of its first byte.
func (s *streamCapture) window() ([]byte, int64) {
	w := s.tail
	if len(w) > standardStreamBytes {
		w = w[len(w)-standardStreamBytes:]
	}
	return w, s.total - int64(len(w))
}

func (s *streamCapture) value() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, start := s.window()
	if start > 0 {
		w = trimLeadingContinuation(w)
	}
	v := map[string]any{"text": strings.ToValidUTF8(string(w), "�"), "truncated": start > 0}
	if s.spillPath != "" {
		v["spillPath"] = s.spillPath
	}
	return v
}

func (s *streamCapture) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, _ := s.window()
	return string(w)
}

func (s *streamCapture) path() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spillPath
}

// readFrom returns retained bytes after offset. lossy reports bytes that left
// the tail unread. A live stream holds back a trailing partial UTF-8 sequence.
func (s *streamCapture) readFrom(offset int64, final bool) (string, int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, start := s.window()
	lossy := offset < start
	if lossy {
		offset = start
	}
	b := w[offset-start:]
	if lossy {
		skipped := len(b) - len(trimLeadingContinuation(b))
		b, offset = b[skipped:], offset+int64(skipped)
	}
	if !final {
		b = b[:completeUTF8(b)]
	}
	return strings.ToValidUTF8(string(b), "�"), offset + int64(len(b)), lossy
}

func trimLeadingContinuation(b []byte) []byte {
	for len(b) > 0 && !utf8.RuneStart(b[0]) {
		b = b[1:]
	}
	return b
}

// completeUTF8 is the length of b without an incomplete trailing sequence.
func completeUTF8(b []byte) int {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if !utf8.FullRune(b[i:]) {
				return i
			}
			break
		}
	}
	return len(b)
}

// standardBashState is the official job record of one standard bash command:
// split streams, the model's consuming cursor and settled process facts.
type standardBashState struct {
	description, cwd, mode, backend string
	stdout, stderr                  *streamCapture
	mu                              sync.Mutex
	status, detail                  string // running, stopping, completed, killed, failed
	stopped, signal                 string
	exitCode                        *int
	denied                          bool
	cursor                          [2]int64
}

// requestStop marks a live job stopping; reason is what a waiting call reports.
func (s *standardBashState) requestStop(reason string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status != "running" && s.status != "stopping" {
		return false
	}
	s.status = "stopping"
	if s.stopped == "" {
		s.stopped = reason
	}
	return true
}

func (s *standardBashState) settle(ps *os.ProcessState, waitErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ps == nil {
		s.status, s.detail = "failed", errorString(waitErr)
		return
	}
	if ws, ok := ps.Sys().(interface {
		Signaled() bool
		Signal() syscall.Signal
	}); ok && ws.Signaled() {
		s.signal = signalName(ws.Signal())
	} else {
		code := ps.ExitCode()
		s.exitCode = &code
	}
	failed := s.signal != "" || *s.exitCode != 0
	s.denied = failed && s.mode != "full-access" && deniedBySandbox(s.backend, s.stderr.text())
	switch {
	case s.stopped != "":
		s.status, s.detail = "killed", s.stopped
	case s.status == "stopping" || s.signal != "":
		s.status, s.detail = "killed", "signal: "+s.signal
		if s.signal == "" {
			s.detail = "killed before exit"
		}
	default:
		s.status, s.detail = "completed", fmt.Sprintf("exit code: %d", *s.exitCode)
	}
	if notes := s.sandboxNotes(); len(notes) > 0 {
		s.detail += "; " + strings.Join(notes, " ")
	}
}

func (s *standardBashState) sandboxNotes() []string {
	if !s.denied {
		return nil
	}
	return []string{"[sandbox: file access denied under " + officialMode(s.mode) + " mode]", escalationHint}
}

func (s *standardBashState) settled() bool {
	return s.status != "running" && s.status != "stopping"
}

// consume is one model read: stdout, then one [stderr] section, then the
// dropped-output notice when the cursor fell behind the retained tail.
func (s *standardBashState) consume() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	final := s.settled()
	out, nextOut, lostOut := s.stdout.readFrom(s.cursor[0], final)
	errText, nextErr, lostErr := s.stderr.readFrom(s.cursor[1], final)
	s.cursor = [2]int64{nextOut, nextErr}
	body := joinStreams(out, errText)
	if lostOut || lostErr {
		var paths []string
		for _, p := range []string{s.stdout.path(), s.stderr.path()} {
			if p != "" {
				paths = append(paths, p)
			}
		}
		where := "(unavailable)"
		if len(paths) > 0 {
			where = strings.Join(paths, ", ")
		}
		body = appendLine(body, "[some output was dropped from memory; full output: "+where+"]")
	}
	return body
}

// legacySnapshot keeps the original jobs/`/jobs` view working for official
// bash jobs: the combined write-order output plus the official job facts.
func (s *standardBashState) legacySnapshot(j *job, offset int, consume bool) map[string]any {
	m := j.legacyView(offset, consume)
	s.mu.Lock()
	defer s.mu.Unlock()
	m["kind"] = "bash"
	m["description"] = s.description
	m["cwd"] = s.cwd
	m["job_status"] = s.status
	if s.detail != "" {
		m["detail"] = s.detail
	}
	if s.signal != "" {
		m["signal"] = s.signal
	}
	if s.stopped != "" {
		m["stopped"] = s.stopped
	}
	if sandbox, ok := m["sandbox"].(map[string]any); ok {
		sandbox["denied"] = s.denied
	}
	return m
}

func (s *standardBashState) foregroundValue(timeoutMS int) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := map[string]any{"kind": "foreground", "cwd": s.cwd, "exitCode": nil, "signal": nil, "timedOut": false, "aborted": false, "timeoutMs": timeoutMS, "stdout": s.stdout.value(), "stderr": s.stderr.value(), "sandbox": map[string]any{"mode": officialMode(s.mode), "denied": s.denied}}
	if s.exitCode != nil {
		v["exitCode"] = *s.exitCode
	}
	if s.signal != "" {
		v["signal"] = s.signal
	}
	if s.stopped != "" {
		v["stopped"] = s.stopped
	}
	return v
}

func joinStreams(out, errText string) string {
	if errText == "" {
		return out
	}
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out + "[stderr]\n" + errText
}

func appendLine(body, line string) string {
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return body + line
}

func officialMode(mode string) string {
	if mode == "full-access" {
		return "danger-full-access"
	}
	return mode
}

func dsgoMode(mode string) string {
	if mode == "danger-full-access" {
		return "full-access"
	}
	return mode
}

// Denials are inferred from a failed run's stderr in the selected backend's
// dialect: EPERM under Seatbelt, EROFS under bubblewrap's read-only binds.
func deniedBySandbox(backend, stderr string) bool {
	s := strings.ToLower(stderr)
	switch backend {
	case "macos-seatbelt":
		return strings.Contains(s, "operation not permitted")
	case "linux-bubblewrap":
		return strings.Contains(s, "read-only file system")
	}
	return false
}

var signalNames = map[syscall.Signal]string{syscall.SIGHUP: "SIGHUP", syscall.SIGINT: "SIGINT", syscall.SIGQUIT: "SIGQUIT", syscall.SIGABRT: "SIGABRT", syscall.SIGKILL: "SIGKILL", syscall.SIGSEGV: "SIGSEGV", syscall.SIGPIPE: "SIGPIPE", syscall.SIGTERM: "SIGTERM"}

func signalName(sig syscall.Signal) string {
	if name, ok := signalNames[sig]; ok {
		return name
	}
	return sig.String()
}

type standardBashArgs struct {
	Description        string   `json:"description"`
	Command            string   `json:"command"`
	TimeoutMS          *float64 `json:"timeoutMs"`
	Workdir            *string  `json:"workdir"`
	RunInBackground    bool     `json:"run_in_background"`
	SandboxPermissions *string  `json:"sandbox_permissions"`
	Justification      *string  `json:"justification"`
}

// bashSandboxMode applies the official escalation rules: repeating the
// effective mode needs no approval, widening needs a reason and approval, and
// anything narrower fails before execution.
func bashSandboxMode(ctx context.Context, tc core.ToolContext, raw json.RawMessage, a standardBashArgs) (string, error) {
	effective := tc.Permission
	if effective == "" {
		effective = "workspace-write"
	}
	if a.SandboxPermissions != nil && dsgoMode(*a.SandboxPermissions) == effective {
		return effective, nil
	}
	justification := a.Justification
	if a.SandboxPermissions == nil && justification != nil && strings.TrimSpace(*justification) == "" {
		justification = nil
	}
	switch {
	case a.SandboxPermissions != nil && justification == nil:
		return "", errors.New("invalid escalation: sandbox_permissions requires a justification")
	case a.SandboxPermissions == nil && justification != nil:
		return "", errors.New("invalid escalation: justification is only valid together with sandbox_permissions")
	case justification != nil && strings.TrimSpace(*justification) == "":
		return "", errors.New("invalid justification: expected a non-empty sentence")
	case a.SandboxPermissions == nil:
		return effective, nil
	}
	requested := *a.SandboxPermissions
	rank := map[string]int{"read-only": 0, "workspace-write": 1, "full-access": 2}
	if rank[dsgoMode(requested)] <= rank[effective] {
		return "", fmt.Errorf("sandbox escalation to %q is not strictly wider than this call's current %q mode", requested, officialMode(effective))
	}
	if err := ask(ctx, tc, "bash", "Allow this command with "+requested+" permissions: "+*justification, raw); err != nil {
		if errors.Is(err, errUserDenied) {
			return "", fmt.Errorf("the user rejected escalating this command to %q; it stays denied, so stop and explain instead of working around it", requested)
		}
		return "", err
	}
	return dsgoMode(requested), nil
}

func (r *Registry) standardBash(ctx context.Context, tc core.ToolContext, raw json.RawMessage) (core.ToolResult, error) {
	var a standardBashArgs
	if err := decode(raw, &a); err != nil {
		return core.ToolResult{}, err
	}
	if strings.TrimSpace(a.Command) == "" {
		return core.ToolResult{}, errors.New("invalid command: expected a non-empty string")
	}
	if strings.TrimSpace(a.Description) == "" {
		return core.ToolResult{}, errors.New("invalid description: expected a non-empty string")
	}
	timeoutMS := standardBashTimeoutMS
	if a.TimeoutMS != nil {
		if *a.TimeoutMS <= 0 || math.IsInf(*a.TimeoutMS, 0) {
			return core.ToolResult{}, fmt.Errorf("invalid timeoutMs: expected a positive number, got %s", strconv.FormatFloat(*a.TimeoutMS, 'f', -1, 64))
		}
		timeoutMS = int(min(math.Ceil(*a.TimeoutMS), standardBashMaxTimeoutMS))
	}
	mode, err := bashSandboxMode(ctx, tc, raw, a)
	if err != nil {
		return core.ToolResult{}, err
	}
	workdir := tc.CWD
	if a.Workdir != nil {
		workdir = *a.Workdir
		if !filepath.IsAbs(workdir) {
			workdir = filepath.Join(tc.CWD, workdir)
		}
	}
	tc.CWD = filepath.Clean(workdir)
	if err = ctx.Err(); err != nil {
		return core.ToolResult{}, err
	}
	j, err := r.startStandardBash(tc, a, mode, !a.RunInBackground)
	if err != nil {
		return core.ToolResult{}, err
	}
	if a.RunInBackground {
		return result(map[string]any{"kind": "background", "jobId": j.id, "cwd": tc.CWD}), nil
	}
	timer := time.NewTimer(time.Duration(timeoutMS) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-j.done:
		// The model never saw this id, so the record leaves with the result.
		r.removeJob(j.id)
		return result(j.standard.foregroundValue(timeoutMS)), nil
	case <-timer.C:
		// The call stops waiting, so a later settlement notifies as usual.
		j.mu.Lock()
		j.waiters--
		j.mu.Unlock()
		// One consuming read seeds the result; job_output continues after it.
		return result(map[string]any{"kind": "promoted", "cwd": tc.CWD, "jobId": j.id, "timeoutMs": timeoutMS, "output": j.standard.consume()}), nil
	case <-ctx.Done():
		j.requestKill("tool call aborted")
		<-j.done
		r.removeJob(j.id)
		return core.ToolResult{}, ctx.Err()
	}
}

// startStandardBash registers the command as a job from its start. A
// foreground caller is its first waiter, so a settlement inside its wait
// carries no notice.
func (r *Registry) startStandardBash(tc core.ToolContext, a standardBashArgs, mode string, foreground bool) (*job, error) {
	r.mu.RLock()
	dir := r.spillDir
	r.mu.RUnlock()
	// The job outlives the call when promoted or backgrounded; its own kill
	// paths own cancellation from here on.
	runctx, cancel := context.WithCancel(context.Background())
	sc, err := makeToolCommand(runctx, tc, mode, "/bin/bash", "-c", a.Command)
	if err != nil {
		cancel()
		return nil, err
	}
	sc.cmd.Env = append(filteredEnv(sc.cmd.Env, "NO_COLOR", "TERM", "PAGER", "GIT_PAGER"), "NO_COLOR=1", "TERM=dumb", "PAGER=cat", "GIT_PAGER=cat")
	sc.cmd.Env, err = freshShellEnvironment(sc.cmd.Env, tc)
	if err != nil {
		cancel()
		sc.cleanup()
		return nil, err
	}
	st := &standardBashState{description: a.Description, cwd: tc.CWD, mode: mode, backend: sc.backend, status: "running", stdout: &streamCapture{dir: dir}, stderr: &streamCapture{dir: dir}}
	combined := &boundedBuffer{limit: maxOutputBytes}
	sc.cmd.Stdout = io.MultiWriter(st.stdout, combined)
	sc.cmd.Stderr = io.MultiWriter(st.stderr, combined)
	if err := sc.cmd.Start(); err != nil {
		cancel()
		sc.cleanup()
		return nil, fmt.Errorf("bash start failed: %w", err)
	}
	j := &job{standard: st, id: "bash-" + newID()[:16], owner: tc.SessionID, command: a.Command, cancel: cancel, done: make(chan struct{}), noticed: make(chan struct{}), output: combined, started: time.Now(), backend: sc.backend, mode: mode, pid: sc.cmd.Process.Pid}
	if foreground {
		j.waiters = 1
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		cancel()
		_ = sc.cmd.Wait()
		sc.cleanup()
		return nil, errors.New("registry closed")
	}
	r.jobs[j.id] = j
	r.mu.Unlock()
	go func() {
		err := sc.cmd.Wait()
		terminateProcess(sc.cmd)
		sc.cleanup()
		cancel()
		st.stdout.close()
		st.stderr.close()
		st.settle(sc.cmd.ProcessState, err)
		j.mu.Lock()
		j.finished = time.Now()
		j.exitCode = sc.cmd.ProcessState.ExitCode()
		if err != nil {
			j.err = err.Error()
		}
		// Waits register under mu, so exactly the waits this settlement
		// releases are counted; a later wait finds the job already settled.
		awaited := j.waiters > 0
		close(j.done)
		j.mu.Unlock()
		r.settled(j, awaited)
	}()
	return j, nil
}

func (r *Registry) removeJob(id string) {
	r.mu.Lock()
	delete(r.jobs, id)
	r.mu.Unlock()
}

// JobNotice is an owned job's completion notice in the official tool-jobs wording.
type JobNotice struct {
	Owner, Text, Summary string
}

// SetJobNotifier receives completion notices for owned bash jobs. Settlements
// that released a live wait, kills the model requested through job_kill, and
// registry teardown produce none.
func (r *Registry) SetJobNotifier(notify func(JobNotice)) {
	r.mu.Lock()
	r.notifier = notify
	r.mu.Unlock()
}

// CompletionNotice renders the official settlement notice and its context summary.
func CompletionNotice(id, kind, label, status, detail string, outputLimitBytes ...int) JobNotice {
	line := statusLine(publicJob{Status: status, Detail: detail})
	limit := 0
	if len(outputLimitBytes) > 0 {
		limit = outputLimitBytes[0]
	}
	return JobNotice{Text: fitCompletionNotice(id, kind, label, line, limit), Summary: core.BoundContextSummary(kind + " " + label + " " + line)}
}

func (r *Registry) settled(j *job, awaited bool) {
	defer close(j.noticed)
	r.mu.RLock()
	notify, closed := r.notifier, r.closed
	r.mu.RUnlock()
	j.mu.Lock()
	killed, limit := j.killedByModel, j.outputLimitBytes
	j.mu.Unlock()
	if notify == nil || closed || awaited || killed || j.owner == "" {
		return
	}
	p := j.public()
	n := CompletionNotice(p.ID, p.Kind, p.Label, p.Status, p.Detail, limit)
	n.Owner = j.owner
	notify(n)
}

// HasPendingJobs reports an owned bash job that is still running or whose
// completion notice has not been handed over yet.
func (r *Registry) HasPendingJobs(owners ...string) bool {
	owned := map[string]bool{}
	for _, owner := range owners {
		owned[owner] = true
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, j := range r.jobs {
		if j.noticed == nil || !owned[j.owner] {
			continue
		}
		select {
		case <-j.noticed:
		default:
			return true
		}
	}
	return false
}

// wait blocks until the job settles or d passes; a settlement during the
// wait is the caller's delivery and sends no notice.
func (j *job) wait(ctx context.Context, d time.Duration) error {
	j.mu.Lock()
	j.waiters++
	j.mu.Unlock()
	defer func() {
		j.mu.Lock()
		j.waiters--
		j.mu.Unlock()
	}()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-j.done:
	case <-timer.C:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// requestKill asks a live job to stop. A settled job reports false.
func (j *job) requestKill(reason string) bool {
	select {
	case <-j.done:
		return false
	default:
	}
	if j.standard != nil && !j.standard.requestStop(reason) {
		return false
	}
	j.cancel()
	return true
}

type publicJob struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Label      string `json:"label"`
	Status     string `json:"status"`
	Detail     string `json:"detail,omitempty"`
	StartedAt  int64  `json:"startedAt"`
	FinishedAt int64  `json:"finishedAt,omitempty"`
}

// public is the model-safe projection the job_* tools share across job kinds.
func (j *job) public() publicJob {
	j.mu.Lock()
	p := publicJob{ID: j.id, Kind: "shell", Label: j.command, StartedAt: j.started.UnixMilli()}
	finished, exitCode, errText, terminal := j.finished, j.exitCode, j.err, j.terminal != nil
	j.mu.Unlock()
	if !finished.IsZero() {
		p.FinishedAt = finished.UnixMilli()
	}
	if terminal {
		p.Kind = "pty"
	}
	switch {
	case j.standard != nil:
		s := j.standard
		s.mu.Lock()
		p.Kind, p.Status, p.Detail = "bash", s.status, s.detail
		s.mu.Unlock()
	case finished.IsZero():
		p.Status = "running"
	case exitCode >= 0:
		p.Status, p.Detail = "completed", fmt.Sprintf("exit code: %d", exitCode)
	default:
		p.Status, p.Detail = "killed", errText
	}
	return p
}

func (j *job) modelRead() string {
	if j.standard != nil {
		return j.standard.consume()
	}
	text, _ := j.legacyView(0, true)["output"].(string)
	return text
}

func (r *Registry) ownedJob(tc core.ToolContext, id string) (*job, error) {
	if id == "" {
		return nil, errors.New(`invalid job_id: expected a non-empty string, got ""`)
	}
	r.mu.RLock()
	j, ok := r.jobs[id]
	r.mu.RUnlock()
	if !ok || j.owner != tc.SessionID {
		return nil, fmt.Errorf("job %s does not exist in this session", id)
	}
	return j, nil
}

// JobWait resolves job_output's wait from its timeout_ms argument.
func JobWait(timeoutMS *float64) (time.Duration, error) {
	return resolveJobWait(timeoutMS, jobWaitMS, jobMaxWaitMS)
}

func (r *Registry) jobOutput(ctx context.Context, tc core.ToolContext, raw json.RawMessage) (core.ToolResult, error) {
	var a struct {
		JobID     string   `json:"job_id"`
		Wait      bool     `json:"wait"`
		TimeoutMS *float64 `json:"timeout_ms"`
	}
	if err := decode(raw, &a); err != nil {
		return core.ToolResult{}, err
	}
	j, err := r.ownedJob(tc, a.JobID)
	if err != nil {
		return core.ToolResult{}, err
	}
	if a.Wait {
		wait, err := r.JobWait(a.TimeoutMS)
		if err != nil {
			return core.ToolResult{}, err
		}
		if err = j.wait(ctx, wait); err != nil {
			return core.ToolResult{}, err
		}
	}
	// A settled projection implies every byte was written before the read.
	snapshot := j.public()
	return jobResult(map[string]any{"text": j.modelRead(), "job": snapshot}), nil
}

func (r *Registry) jobList(_ context.Context, tc core.ToolContext, raw json.RawMessage) (core.ToolResult, error) {
	var a struct{}
	if err := decode(raw, &a); err != nil {
		return core.ToolResult{}, err
	}
	r.mu.RLock()
	var owned []*job
	for _, j := range r.jobs {
		if j.owner == tc.SessionID {
			owned = append(owned, j)
		}
	}
	r.mu.RUnlock()
	out := make([]publicJob, 0, len(owned))
	for _, j := range owned {
		out = append(out, j.public())
	}
	sort.Slice(out, func(i, k int) bool {
		if out[i].StartedAt != out[k].StartedAt {
			return out[i].StartedAt < out[k].StartedAt
		}
		return out[i].ID < out[k].ID
	})
	return result(out), nil
}

func (r *Registry) jobKill(_ context.Context, tc core.ToolContext, raw json.RawMessage) (core.ToolResult, error) {
	var a struct {
		JobID  string `json:"job_id"`
		Reason string `json:"reason"`
	}
	if err := decode(raw, &a); err != nil {
		return core.ToolResult{}, err
	}
	j, err := r.ownedJob(tc, a.JobID)
	if err != nil {
		return core.ToolResult{}, err
	}
	// Marked before the kill lands so its settlement cannot notify first.
	j.mu.Lock()
	j.killedByModel = true
	j.mu.Unlock()
	outcome := "cancellation-requested"
	if !j.requestKill(strings.TrimSpace(a.Reason)) {
		outcome = "already-finished"
		j.mu.Lock()
		j.killedByModel = false
		j.mu.Unlock()
	}
	return jobResult(map[string]any{"outcome": outcome, "job": j.public()}), nil
}

// RenderShellResult turns canonical bash and job_* values into model text.
// PTC receives the canonical JSON; only top-level presentation renders it.
func RenderShellResult(name string, raw string) (string, bool) {
	switch name {
	case "bash":
		var v struct {
			Kind      string  `json:"kind"`
			JobID     string  `json:"jobId"`
			TimeoutMS int     `json:"timeoutMs"`
			Output    string  `json:"output"`
			ExitCode  *int    `json:"exitCode"`
			Signal    string  `json:"signal"`
			Stopped   *string `json:"stopped"`
			Stdout    struct {
				Text      string `json:"text"`
				Truncated bool   `json:"truncated"`
				SpillPath string `json:"spillPath"`
			} `json:"stdout"`
			Stderr struct {
				Text      string `json:"text"`
				Truncated bool   `json:"truncated"`
				SpillPath string `json:"spillPath"`
			} `json:"stderr"`
			Sandbox struct {
				Mode   string `json:"mode"`
				Denied bool   `json:"denied"`
			} `json:"sandbox"`
		}
		if json.Unmarshal([]byte(raw), &v) != nil {
			return "", false
		}
		switch v.Kind {
		case "background":
			return "started background job " + v.JobID, true
		case "promoted":
			body := v.Output
			if body != "" && !strings.HasSuffix(body, "\n") {
				body += "\n"
			}
			return fmt.Sprintf("%s[still running after %dms; moved to background job %s]\nThe command keeps running in the background. You will be notified when it finishes; read newer output with job_output, stop it with job_kill.", body, v.TimeoutMS, v.JobID), true
		case "foreground":
		default:
			return "", false
		}
		stream := func(text string, truncated bool, path string) string {
			if !truncated {
				return text
			}
			if path == "" {
				path = "(unavailable)"
			}
			return text + "\n[output truncated; full output: " + path + "]"
		}
		body := joinStreams(stream(v.Stdout.Text, v.Stdout.Truncated, v.Stdout.SpillPath), stream(v.Stderr.Text, v.Stderr.Truncated, v.Stderr.SpillPath))
		if body == "" {
			body = "(no output)"
		}
		var markers []string
		if v.Sandbox.Denied {
			markers = append(markers, "[sandbox: file access denied under "+v.Sandbox.Mode+" mode]", escalationHint)
		}
		if v.Stopped != nil {
			markers = append(markers, "[stopped: "+*v.Stopped+"]")
		}
		// The exit marker stays last; replay parses it as the exit status.
		if v.Signal != "" {
			markers = append(markers, "[killed by signal: "+v.Signal+"]")
		} else if v.ExitCode != nil && *v.ExitCode != 0 {
			markers = append(markers, fmt.Sprintf("[exit code: %d]", *v.ExitCode))
		}
		if len(markers) == 0 {
			return body, true
		}
		return appendLine(body, strings.Join(markers, "\n")), true
	case "job_output":
		var v struct {
			Text *string   `json:"text"`
			Job  publicJob `json:"job"`
		}
		if json.Unmarshal([]byte(raw), &v) != nil || v.Text == nil {
			return "", false
		}
		body := *v.Text
		if body == "" {
			body = "(no new output)"
		}
		return appendLine(body, statusLine(v.Job)), true
	case "job_list":
		var v []publicJob
		if json.Unmarshal([]byte(raw), &v) != nil {
			return "", false
		}
		if len(v) == 0 {
			return "(no background jobs)", true
		}
		lines := make([]string, len(v))
		for i, j := range v {
			lines[i] = fmt.Sprintf("%s [%s] %s — %s", j.ID, j.Kind, j.Status, j.Label)
		}
		return strings.Join(lines, "\n"), true
	case "job_kill":
		var v struct {
			Outcome string    `json:"outcome"`
			Job     publicJob `json:"job"`
		}
		if json.Unmarshal([]byte(raw), &v) != nil || v.Outcome == "" {
			return "", false
		}
		if v.Outcome == "already-finished" {
			return "job " + v.Job.ID + " had already finished " + statusLine(v.Job), true
		}
		return "requested cancellation of job " + v.Job.ID, true
	}
	return "", false
}

func statusLine(j publicJob) string {
	if j.Detail != "" {
		return "[status: " + j.Status + ", " + j.Detail + "]"
	}
	return "[status: " + j.Status + "]"
}
