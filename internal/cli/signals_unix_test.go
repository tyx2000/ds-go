//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dsgo/internal/core"
)

func TestSCOR005SignalHelper(t *testing.T) {
	if os.Getenv("DSGO_SCOR005_SIGNAL_HELPER") != "1" {
		t.Skip("isolated subprocess helper")
	}
	ready, survived := os.Getenv("DSGO_SCOR005_READY"), os.Getenv("DSGO_SCOR005_SURVIVED")
	var a *App
	a, parent := lifecycleApp(t, lifecycleModel(func(ctx context.Context, request core.Request, _ func(core.Delta)) (core.Response, error) {
		_, err := a.registry.Execute(ctx, core.ToolContext{SessionID: request.SessionID, CWD: filepath.Dir(ready), Permission: "full-access"}, "standard_bash", core.JSON(map[string]any{"description": "Signal fixture", "command": fmt.Sprintf("printf ready > %q; sleep 0.7; printf survived > %q", ready, survived)}))
		return core.Response{Content: []core.Block{{Type: "text", Text: "done"}}}, err
	}))
	a.session = parent
	a.in = bufio.NewReader(os.Stdin)
	a.out = os.Stdout
	if _, err := a.manager.deliver(a.ctx, parent.ID, core.TextMessage("user", "automatic notice"), true); err != nil {
		t.Fatal(err)
	}
	if err := a.chat(); err != nil {
		t.Fatal(err)
	}
}

func TestSCOR005CtrlCAtPromptCancelsAutomaticShellAndKeepsChat(t *testing.T) {
	dir := t.TempDir()
	ready, survived := filepath.Join(dir, "ready"), filepath.Join(dir, "survived")
	logfile, err := os.Create(filepath.Join(dir, "output"))
	if err != nil {
		t.Fatal(err)
	}
	defer logfile.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSCOR005SignalHelper$", "-test.v")
	cmd.Env = append(os.Environ(), "DSGO_SCOR005_SIGNAL_HELPER=1", "DSGO_SCOR005_READY="+ready, "DSGO_SCOR005_SURVIVED="+survived)
	cmd.Stdout = logfile
	cmd.Stderr = logfile
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, readyErr := os.Stat(ready)
		output, _ := os.ReadFile(logfile.Name())
		if readyErr == nil && strings.Contains(string(output), "dsgo> ") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("automatic shell/chat did not start: %s", output)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err = cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("Ctrl+C exited the chat instead of canceling its automatic owner: %v", err)
	case <-time.After(time.Second):
	}
	if _, err = os.Stat(survived); !os.IsNotExist(err) {
		t.Fatalf("automatic shell continued writing after cancellation: %v", err)
	}
	if _, err = stdin.Write([]byte("/quit\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			output, _ := os.ReadFile(logfile.Name())
			t.Fatalf("chat did not exit cleanly: %v %s", err, output)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("chat did not exit after /quit")
	}
}
