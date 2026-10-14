//go:build unit

package kanoupdate

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type startupSource struct {
	mu      sync.Mutex
	session *Session
	err     error
	calls   atomic.Int32
}

func (s *startupSource) Current(context.Context) (*Session, error) {
	s.calls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.session, s.err
}

func (s *startupSource) set(session *Session, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.session, s.err = session, err
}

func withStartupGuard(t *testing.T, ctx context.Context, source currentJobSource) func() {
	t.Helper()
	stop := guardBackgroundStartup(ctx, source, time.Millisecond)
	t.Cleanup(func() { stop(); startupGuard.Store(nil) })
	return stop
}

func TestStartBackgroundWithoutGuard(t *testing.T) {
	startupGuard.Store(nil)
	called := false
	StartBackground(nil)
	StartBackground(func() { called = true })
	require.True(t, called, "ordinary providers must keep synchronous startup")
}

func TestGuardBackgroundStartupReady(t *testing.T) {
	for _, status := range []Status{"", Succeeded, Failed, FailedRolledBack} {
		t.Run(string(status), func(t *testing.T) {
			source := &startupSource{session: &Session{}}
			if status != "" {
				source.session.Job = &Job{Status: status}
			}
			stop := GuardBackgroundStartup(context.Background(), source)
			t.Cleanup(func() { stop(); startupGuard.Store(nil) })
			called := false
			StartBackground(func() { called = true })
			require.True(t, called)
			require.EqualValues(t, 1, source.calls.Load())
		})
	}
}

func TestGuardInitialUnavailablePreservesOrdinaryStartup(t *testing.T) {
	for _, source := range []currentJobSource{nil, &startupSource{}, &startupSource{err: errors.New("updater offline")}} {
		stop := guardBackgroundStartup(context.Background(), source, time.Millisecond)
		called := false
		StartBackground(func() { called = true })
		require.True(t, called, "an optional updater outage must not disable ordinary workers")
		stop()
		startupGuard.Store(nil)
	}
}

func TestGuardWaitsForEveryActiveStatusAndUpdaterRecovery(t *testing.T) {
	source := &startupSource{session: &Session{Job: &Job{Status: Queued}}}
	withStartupGuard(t, context.Background(), source)
	var started atomic.Int32
	StartBackground(func() { started.Add(1) })
	for _, status := range []Status{Pulling, Preflight, BackingUp, Restoring, Verifying, Switching, Checking, RollingBack, NeedsAttention, "invalid", ""} {
		before := source.calls.Load()
		source.set(&Session{Job: &Job{Status: status}}, nil)
		require.Eventually(t, func() bool { return source.calls.Load() > before }, time.Second, time.Millisecond)
		require.Zero(t, started.Load())
	}
	for _, value := range []struct {
		session *Session
		err     error
	}{{&Session{}, errors.New("private local transport detail")}, {nil, nil}} {
		before := source.calls.Load()
		source.set(value.session, value.err)
		require.Eventually(t, func() bool { return source.calls.Load() > before }, time.Second, time.Millisecond)
		require.Zero(t, started.Load())
	}
	source.set(&Session{Job: &Job{Status: Succeeded}}, nil)
	require.Eventually(t, func() bool { return started.Load() == 1 }, time.Second, time.Millisecond)
	StartBackground(func() { started.Add(1) })
	require.EqualValues(t, 2, started.Load())
}

func TestGuardPreservesOrderIncludingWorkersRegisteredDuringRelease(t *testing.T) {
	source := &startupSource{session: &Session{Job: &Job{Status: Checking}}}
	withStartupGuard(t, context.Background(), source)
	order := make(chan int, 4)
	StartBackground(func() {
		order <- 1
		StartBackground(func() { order <- 3 })
	})
	StartBackground(func() { order <- 2 })
	source.set(&Session{}, nil)
	for _, expected := range []int{1, 2, 3} {
		select {
		case actual := <-order:
			require.Equal(t, expected, actual)
		case <-time.After(time.Second):
			t.Fatal("background startup did not resume")
		}
	}
}

func TestGuardShutdownDropsQueuedAndFutureWorkers(t *testing.T) {
	source := &startupSource{session: &Session{Job: &Job{Status: Checking}}}
	stop := withStartupGuard(t, context.Background(), source)
	var started atomic.Int32
	StartBackground(func() { started.Add(1) })
	stop()
	source.set(&Session{}, nil)
	StartBackground(func() { started.Add(1) })
	require.Zero(t, started.Load())
}

func TestGuardShutdownJoinsInProgressStartupBeforeCleanup(t *testing.T) {
	source := &startupSource{session: &Session{Job: &Job{Status: Checking}}}
	stop := withStartupGuard(t, context.Background(), source)
	entered, release := make(chan struct{}), make(chan struct{})
	StartBackground(func() { close(entered); <-release })
	source.set(&Session{}, nil)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker was not started")
	}
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	guard := startupGuard.Load()
	require.Eventually(t, func() bool { guard.mu.Lock(); defer guard.mu.Unlock(); return guard.closed }, time.Second, time.Millisecond)
	select {
	case <-stopped:
		t.Fatal("cleanup must wait for the worker startup call")
	default:
	}
	StartBackground(func() { t.Error("worker started after shutdown") })
	close(release)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish")
	}
}

func TestGuardCancellationAndMissingSourceStayPaused(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	withStartupGuard(t, ctx, &startupSource{session: &Session{}})
	StartBackground(func() { t.Error("canceled startup ran a worker") })
	ready, known := backgroundReady(context.Background(), nil)
	require.False(t, ready)
	require.False(t, known)
	ready, known = backgroundReady(ctx, &startupSource{session: &Session{}})
	require.False(t, ready)
	require.True(t, known)
}
