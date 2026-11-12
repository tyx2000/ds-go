package cli

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"
)

type foregroundTurn struct{ cancel context.CancelFunc }

// installTerminalSignals restores the terminal before job-control or fatal
// signals. Without it, SIGQUIT exits with ECHO/ICANON disabled and Ctrl+Z
// followed by fg leaves the editor in canonical mode.
func (a *App) installTerminalSignals() func() {
	if a.editor == nil {
		return func() {}
	}
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, syscall.SIGQUIT, syscall.SIGTSTP, syscall.SIGHUP, syscall.SIGCONT)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case sig := <-ch:
				if sig == syscall.SIGCONT {
					a.editor.ReapplyTerminal()
					continue
				}
				a.editor.RestoreTerminal()
				signal.Reset(sig)
				if unixSig, ok := sig.(syscall.Signal); ok {
					_ = syscall.Kill(os.Getpid(), unixSig)
				}
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
	}
}

// Signals belong to the chat lifecycle, including idle input while an
// automatically woken Engine turn runs. Nested foreground helpers share it.
func (a *App) acquireSignals() func() {
	a.signalMu.Lock()
	if a.signalRefs == 0 {
		interrupts := make(chan os.Signal, 4)
		done := make(chan struct{})
		signal.Notify(interrupts, os.Interrupt, syscall.SIGTERM)
		a.signalStop = func() { signal.Stop(interrupts); close(done) }
		go func() {
			for {
				select {
				case <-done:
					return
				case sig := <-interrupts:
					a.interruptCurrent(sig)
				case <-a.ctx.Done():
					return
				}
			}
		}()
	}
	a.signalRefs++
	a.signalMu.Unlock()
	return func() {
		a.signalMu.Lock()
		a.signalRefs--
		if a.signalRefs == 0 {
			a.signalStop()
			a.signalStop = nil
		}
		a.signalMu.Unlock()
	}
}

func (a *App) interruptCurrent(sig os.Signal) {
	if a.editor != nil {
		// Ctrl+C at the prompt discards the typed line, as in a shell.
		a.editor.Interrupt()
	}
	// A background session may hold an approval/question lease while the
	// foreground session composes input. Ctrl+C must end that interaction,
	// not just clear the visible line.
	a.interactionMu.Lock()
	stopInteraction := a.interactionStop
	a.interactionStop = nil
	a.interactionMu.Unlock()
	if stopInteraction != nil {
		stopInteraction()
		a.line("\n[Pending approval cancelled]\n")
	}
	id, _ := a.current.Load().(string)
	if sig == syscall.SIGTERM && a.cancel != nil {
		a.cancel()
	}
	if id == "" || a.engine == nil {
		return
	}
	if a.manager != nil {
		a.manager.pauseWakes(id)
	}
	if owner, ok := a.foreground.Load(id); ok {
		owner.(*foregroundTurn).cancel()
	}
	busy := a.engine.Cancel(id)
	if busy {
		a.line("\nCancelling…\n")
	}
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelDrain()
	if a.manager != nil {
		_ = a.manager.cancelAndDrain(drainCtx, id)
	}
	_ = a.engine.WaitIdle(drainCtx, id)
	if busy {
		a.line("[Current session cancelled]\n")
	}
}
