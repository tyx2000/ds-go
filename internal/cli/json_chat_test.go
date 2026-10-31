package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"dsgo/internal/agent"
	"dsgo/internal/core"
)

func TestJSONChatResumeAndSlashOutputIsNDJSON(t *testing.T) {
	a, s := lifecycleApp(t, lifecycleModel(func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
		return core.Response{}, nil
	}))
	var output, errors bytes.Buffer
	a.out, a.err = &output, &errors
	a.flags.json, a.flags.resume, a.session = true, s.ID, s
	a.in = bufio.NewReader(strings.NewReader("/help\n/history\n/sessions\n/cwd\n/export markdown\n/quit\n"))
	if _, err := a.store.Append(s.ID, "turn1", "user/message", core.TextMessage("user", "A historical line\nand another line.")); err != nil {
		t.Fatal(err)
	}
	if err := a.chat(); err != nil {
		t.Fatal(err)
	}
	if errors.Len() != 0 {
		t.Fatalf("chat errors: %s", errors.String())
	}
	var history, session, outputs int
	scanner := bufio.NewScanner(&output)
	for scanner.Scan() {
		var value any
		if err := json.Unmarshal(scanner.Bytes(), &value); err != nil {
			t.Fatalf("stdout contains a non-NDJSON line %q: %v", scanner.Text(), err)
		}
		if object, ok := value.(map[string]any); ok {
			switch object["type"] {
			case "session":
				session++
			case "history":
				history++
			case "output":
				outputs++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if session != 1 || history != 2 || outputs < 3 {
		t.Fatalf("missing JSON records: session=%d history=%d output=%d", session, history, outputs)
	}
}

func TestJSONOutputAndNoticeEachUseOneRecord(t *testing.T) {
	var output bytes.Buffer
	a := &App{out: &output, flags: flags{json: true}}
	a.line("slash response\nwith multiple lines\n")
	a.print(map[string]any{"nested": map[string]any{"value": "two\nlines"}})
	a.notice(agent.Notice{Type: "delta", Data: core.Delta{Type: "text", Text: "model response"}})
	lines := bytes.Split(bytes.TrimSpace(output.Bytes()), []byte("\n"))
	if len(lines) != 3 {
		t.Fatalf("expected one line per record, got %q", output.String())
	}
	for _, line := range lines {
		if !json.Valid(line) {
			t.Fatalf("invalid record: %q", line)
		}
	}
}

func TestCompactionWarningIsVisibleAndTerminalSafe(t *testing.T) {
	var output, errors bytes.Buffer
	a := &App{out: &output, err: &errors}
	a.notice(agent.Notice{Type: "warning", Data: map[string]any{"phase": "context-pressure", "message": "Compaction failed; continuing.", "error": "server \x1b[31mfailed"}})
	if output.Len() != 0 || !strings.Contains(errors.String(), "context-pressure") || !strings.Contains(errors.String(), "Compaction failed; continuing.") || !strings.Contains(errors.String(), "server [31mfailed") || strings.ContainsRune(errors.String(), '\x1b') {
		t.Fatalf("warning not safely displayed: stdout=%q stderr=%q", output.String(), errors.String())
	}
}
