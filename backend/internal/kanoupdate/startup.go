package kanoupdate

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

type currentJobSource interface {
	Current(context.Context) (*Session, error)
}

// The guard is installed before dependency wiring, when service providers
// normally start their workers. HTTP, security initialization and migrations
// remain available; only scheduled/background work uses StartBackground.
var startupGuard atomic.Pointer[backgroundStartup]

type backgroundStartup struct {
	ctx      context.Context
	mu       sync.Mutex
	pending  []func()
	ready    bool
	closed   bool
	cancel   context.CancelFunc
	monitor  sync.WaitGroup
	starting sync.WaitGroup
}

// GuardBackgroundStartup delays background workers until the host upgrade has
// finished. Call it before wiring services, and stop it before service cleanup.
// Ordinary startup remains available if the optional updater cannot be reached.
// Once maintenance is confirmed, a transient disconnect keeps workers paused.
func GuardBackgroundStartup(ctx context.Context, source currentJobSource) func() {
	return guardBackgroundStartup(ctx, source, 2*time.Second)
}

func guardBackgroundStartup(ctx context.Context, source currentJobSource, interval time.Duration) func() {
	ctx, cancel := context.WithCancel(ctx)
	guard := &backgroundStartup{ctx: ctx, cancel: cancel}
	startupGuard.Store(guard)
	ready, known := backgroundReady(ctx, source)
	if ready || !known {
		guard.release()
	} else {
		guard.monitor.Add(1)
		go func() {
			defer guard.monitor.Done()
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					ready, known := backgroundReady(ctx, source)
					if known && ready {
						guard.release()
						return
					}
				}
			}
		}()
	}
	return guard.close
}

func backgroundReady(ctx context.Context, source currentJobSource) (ready, known bool) {
	if ctx.Err() != nil {
		return false, true
	}
	if source == nil {
		return false, false
	}
	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	session, err := source.Current(probe)
	if ctx.Err() != nil {
		return false, true
	}
	if err != nil || session == nil || probe.Err() != nil {
		return false, false
	}
	return session.Job == nil || (session.Job.Status.Valid() && !session.Job.Status.Active()), true
}

// StartBackground preserves provider startup order while an upgrade is active.
// Without a configured guard it has the same synchronous behavior as start().
func StartBackground(start func()) {
	if start == nil {
		return
	}
	guard := startupGuard.Load()
	if guard == nil {
		start()
		return
	}
	guard.mu.Lock()
	if guard.closed || guard.ctx.Err() != nil {
		guard.mu.Unlock()
		return
	}
	if !guard.ready {
		guard.pending = append(guard.pending, start)
		guard.mu.Unlock()
		return
	}
	guard.starting.Add(1)
	guard.mu.Unlock()
	defer guard.starting.Done()
	start()
}

func (g *backgroundStartup) release() {
	for {
		g.mu.Lock()
		if g.closed || g.ctx.Err() != nil {
			g.mu.Unlock()
			return
		}
		if len(g.pending) == 0 {
			g.ready = true
			g.mu.Unlock()
			return
		}
		start := g.pending[0]
		g.pending = g.pending[1:]
		g.starting.Add(1)
		g.mu.Unlock()
		start()
		g.starting.Done()
	}
}

func (g *backgroundStartup) close() {
	g.mu.Lock()
	g.closed = true
	g.pending = nil
	g.cancel()
	g.mu.Unlock()
	g.monitor.Wait()
	g.starting.Wait()
}
