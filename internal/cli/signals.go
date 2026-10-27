package cli

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

type foregroundTurn struct{ cancel context.CancelFunc }

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
	if a.manager != nil {
		_ = a.manager.cancelAndDrain(context.Background(), id)
	}
	_ = a.engine.WaitIdle(context.Background(), id)
	if busy {
		a.line("[Current session cancelled]\n")
	}
}
