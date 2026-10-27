package cli

import (
	"bufio"
	"os"
	"regexp"
	"strings"
	"testing"

	"dsgo/internal/tui"
)

// The help text lists every slash command on its own line, so a command
// added to the dispatcher without documentation fails here.
func TestSlashHelpListsEveryCommandOnItsOwnLine(t *testing.T) {
	source, err := os.ReadFile("commands.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(source)
	start := strings.Index(body, "func (a *App) slash(")
	end := strings.Index(body[start:], "\n}\n")
	if start < 0 || end < 0 {
		t.Fatal("slash dispatcher not found")
	}
	cases := regexp.MustCompile(`(?m)^\tcase ((?:"[a-z-]+",? ?)+):`).FindAllStringSubmatch(body[start:start+end], -1)
	names := regexp.MustCompile(`"([a-z-]+)"`)
	count := 0
	for _, c := range cases {
		for _, name := range names.FindAllStringSubmatch(c[1], -1) {
			count++
			if !regexp.MustCompile(`(?m)(^  /` + name[1] + `\b|\(also /` + name[1] + `\))`).MatchString(slashHelp) {
				t.Errorf("/%s is not documented in /help", name[1])
			}
		}
	}
	if count < 30 {
		t.Fatalf("found only %d commands; the dispatcher format changed", count)
	}
	for _, line := range strings.Split(slashHelp, "\n") {
		if len([]rune(line)) > 80 {
			t.Errorf("help line wider than 80 columns: %q", line)
		}
		if strings.HasPrefix(line, "  /") && strings.Count(line, "  /") > 1 {
			t.Errorf("more than one command on a line: %q", line)
		}
		if strings.HasPrefix(line, "  /") && len(strings.Fields(line)) < 3 {
			t.Errorf("command without a description: %q", line)
		}
	}
}

func TestPromptIsHighlightedOnlyOnInteractiveTerminals(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	a := &App{}
	if got := a.promptLabel("dsgo>"); got != "dsgo>" {
		t.Fatalf("piped prompt carried escapes: %q", got)
	}
	a.editor = tui.NewLineEditor(bufio.NewReader(strings.NewReader("")), nil, nil, func(string) {})
	if got := a.promptLabel("dsgo>"); got != "\x1b[1;36mdsgo>\x1b[0m" {
		t.Fatalf("terminal prompt not highlighted: %q", got)
	}
	t.Setenv("NO_COLOR", "1")
	if got := a.promptLabel("dsgo>"); got != "dsgo>" {
		t.Fatalf("NO_COLOR ignored: %q", got)
	}
}
