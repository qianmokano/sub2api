//go:build unit

package kanoagent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/kanoupdate"
	"github.com/stretchr/testify/require"
)

func reserve(t *testing.T, store *Store, key string) *Job {
	t.Helper()
	request := startRequest()
	request.Schedule, request.IdempotencyKey = true, key
	job, _, err := store.Create(context.Background(), request, "v0.2.16-1", validManifest())
	require.NoError(t, err)
	return job
}

func TestReservationIsDurableExclusiveAndHiddenFromLegacy(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir()
	store, err := OpenStore(path)
	require.NoError(t, err)
	job := reserve(t, store, "reservation")
	require.Equal(t, kanoupdate.Scheduled, job.Status)
	require.False(t, job.Status.Active())
	_, err = store.LegacyCurrent(ctx)
	require.ErrorIs(t, err, ErrNotFound)
	request := startRequest()
	request.Schedule, request.IdempotencyKey = true, "reservation"
	replayed, _, err := store.Create(ctx, request, "v0.2.16-1", validManifest())
	require.NoError(t, err)
	require.Equal(t, job.ScheduledAt, replayed.ScheduledAt)
	request.Schedule = false
	_, err = store.FindRequest(ctx, request)
	require.ErrorIs(t, err, ErrIdempotencyConflict)
	request.IdempotencyKey = "immediate"
	_, _, err = store.Create(ctx, request, "v0.2.16-1", validManifest())
	require.ErrorIs(t, err, ErrBusy)
	token, err := store.Grant(ctx, job.ID)
	require.NoError(t, err)
	var expires int64
	require.NoError(t, store.db.QueryRow("SELECT expires_at FROM capabilities").Scan(&expires))
	require.Equal(t, job.ScheduledAt.Add(24*time.Hour).Unix(), expires)
	require.True(t, store.Authorized(ctx, job.ID, token))
	require.NoError(t, store.Close())
	store, err = OpenStore(path)
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	replayed, err = store.Schedule(ctx)
	require.NoError(t, err)
	require.Equal(t, job.ID, replayed.ID)
	require.Equal(t, job.ScheduledAt, replayed.ScheduledAt)
	require.NoError(t, store.CancelSchedule(ctx, job.ID))
	require.NoError(t, store.CancelSchedule(ctx, job.ID))
	require.ErrorIs(t, store.CancelSchedule(ctx, "missing"), ErrNotFound)
	_, err = store.LegacyCurrent(ctx)
	require.ErrorIs(t, err, ErrNotFound)
	_, _, err = store.Create(ctx, request, "v0.2.16-1", validManifest())
	require.NoError(t, err)
}

func TestReservationAdminCompetitionAndCancelStartRace(t *testing.T) {
	server, _, _ := testServer(t)
	ctx := context.Background()
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			request := startRequest()
			request.Schedule, request.Actor = true, time.Unix(int64(i), 0).String()
			_, _, err := server.Store.Create(ctx, request, "v0.2.16-1", validManifest())
			if err == nil {
				accepted.Add(1)
			} else {
				require.ErrorIs(t, err, ErrBusy)
			}
		}(i)
	}
	wg.Wait()
	require.EqualValues(t, 1, accepted.Load())
	job, err := server.Store.Current(ctx)
	require.NoError(t, err)
	var winners atomic.Int32
	for _, action := range []func() error{
		func() error { return server.Store.CancelSchedule(ctx, job.ID) },
		func() error { return server.Store.transitionSchedule(ctx, job.ID, Queued, "ready", "Starting") },
	} {
		wg.Add(1)
		go func(action func() error) {
			defer wg.Done()
			if action() == nil {
				winners.Add(1)
			}
		}(action)
	}
	wg.Wait()
	require.EqualValues(t, 1, winners.Load())
	require.ErrorIs(t, server.Store.transitionSchedule(ctx, job.ID, kanoupdate.Scheduled, "ready", "late preparation"), ErrBusy)
}

type preparingExecutor struct {
	fakeExecutor
	prepare func(context.Context, *Job) error
}

func (e *preparingExecutor) PrepareSchedule(ctx context.Context, job *Job) error {
	return e.prepare(ctx, job)
}

func TestSchedulePreparationWindowAndRestartRules(t *testing.T) {
	for _, scenario := range []string{"ready", "unready", "late", "restart", "future_restart", "prepare_failure", "prepare_late", "shutdown", "unsupported"} {
		t.Run(scenario, func(t *testing.T) {
			server, _, _ := testServer(t)
			job := reserve(t, server.Store, scenario)
			now := job.ScheduledAt.Add(-time.Hour)
			server.Now = func() time.Time { return now }
			server.Executor = &preparingExecutor{prepare: func(context.Context, *Job) error {
				if scenario == "prepare_failure" {
					return errors.New("private registry credential")
				}
				if scenario == "prepare_late" {
					now = *job.ScheduledAt
				}
				return nil
			}}
			ctx := context.Background()
			switch scenario {
			case "restart":
				now = *job.ScheduledAt
				server.expireOnStartup(ctx)
			case "future_restart":
				server.expireOnStartup(ctx)
			case "unready", "late":
				now = *job.ScheduledAt
				if scenario == "late" {
					now = now.Add(time.Minute)
					job.Preparation = "ready"
				}
				server.advanceSchedule(ctx, job)
			case "unsupported":
				server.Executor = &fakeExecutor{}
				server.advanceSchedule(ctx, job)
			case "shutdown":
				cancelCtx, cancel := context.WithCancel(ctx)
				cancel()
				server.advanceSchedule(cancelCtx, job)
			default:
				server.advanceSchedule(ctx, job)
				if scenario == "ready" {
					job, _ = server.Store.Get(ctx, job.ID)
					require.Equal(t, "ready", job.Preparation)
					server.advanceSchedule(ctx, job) // Already ready stays online.
					now = *job.ScheduledAt
					server.advanceSchedule(ctx, job)
				}
			}
			current, err := server.Store.Get(ctx, job.ID)
			require.NoError(t, err)
			want := kanoupdate.Missed
			if scenario == "ready" {
				want = Queued
			}
			if scenario == "future_restart" || scenario == "shutdown" {
				want = kanoupdate.Scheduled
			}
			require.Equal(t, want, current.Status)
			require.NotContains(t, current.Message, "credential")
		})
	}
}

func TestScheduleHTTPAndPublicStatusDoNotDisclosePrivateProgress(t *testing.T) {
	server, source, _ := testServer(t)
	control := server.ControlHandler()
	response := controlRequest(control, "GET", "/schedule", "")
	require.Contains(t, response.Body.String(), `"job":null`)
	body, _ := json.Marshal(startRequest())
	response = controlRequest(control, "POST", "/schedule", string(body))
	require.Equal(t, 202, response.Code)
	var session Session
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &session))
	require.Equal(t, session.Job.ScheduledAt.Add(24*time.Hour).Unix(), session.ExpiresAt.Unix())
	source.err = errors.New("offline")
	response = controlRequest(control, "POST", "/schedule", string(body))
	require.Equal(t, 202, response.Code)
	response = controlRequest(control, "GET", "/jobs/current", "")
	require.Contains(t, response.Body.String(), `"job":null`)
	response = controlRequest(control, "GET", "/health", "")
	require.Contains(t, response.Body.String(), `"busy":true`)
	require.Contains(t, response.Body.String(), kanoupdate.ScheduleCapability)
	response = controlRequest(control, "GET", "/schedule", "")
	require.Contains(t, response.Body.String(), session.Job.ID)
	public := server.ProgressHandler()
	response = controlRequest(public, "GET", "/kano-maintenance/status", "")
	require.Equal(t, 200, response.Code)
	require.Contains(t, response.Body.String(), "scheduled_at")
	for _, private := range []string{session.Job.ID, session.Job.TargetVersion, session.Job.ImageDigest, "migrations", "capability", "actor"} {
		require.NotContains(t, response.Body.String(), private)
	}
	response = controlRequest(control, "DELETE", "/schedule/invalid", "")
	require.Equal(t, 400, response.Code)
	response = controlRequest(control, "DELETE", "/schedule/"+session.Job.ID, "")
	require.Equal(t, 200, response.Code)
	response = controlRequest(public, "GET", "/kano-maintenance/status", "")
	require.NotContains(t, response.Body.String(), "scheduled_at")
	response = controlRequest(public, "GET", session.ProgressURL, "")
	require.Equal(t, 403, response.Code)
	server.MaintenanceFile = filepath.Join(t.TempDir(), "maintenance")
	require.NoError(t, os.WriteFile(server.MaintenanceFile, []byte(session.Job.ID), 0o644))
	require.NoError(t, server.Store.Update(context.Background(), session.Job.ID, BackingUp, "private"))
	for _, phase := range []Status{BackingUp, Restoring, Switching, RollingBack, NeedsAttention} {
		require.NoError(t, server.Store.Update(context.Background(), session.Job.ID, phase, "private"))
		response = controlRequest(public, "GET", "/kano-maintenance/status", "")
		require.Contains(t, response.Body.String(), `"maintenance":true`)
		require.Contains(t, response.Body.String(), "started_at")
		require.NotContains(t, response.Body.String(), session.Job.ID)
	}
	response = httptest.NewRecorder()
	public.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/kano-maintenance/status", nil))
	require.Equal(t, 405, response.Code)
}

func TestScheduledFinalPreflightNeverStartsLateOrOnChangedDeployment(t *testing.T) {
	for _, scenario := range []string{"late_before", "late_after_preflight", "late_after_checkpoint", "version_changed", "existing_gate", "disk", "image"} {
		t.Run(scenario, func(t *testing.T) {
			fault := ""
			if scenario == "disk" || scenario == "image" {
				fault = scenario
			}
			engine, store, job, fixture := newDeploymentFixture(t, fault)
			at := kanoupdate.NextSchedule(time.Now())
			job.ScheduledAt = &at
			calls := 0
			engine.Now = func() time.Time {
				calls++
				if scenario == "late_before" || (scenario == "late_after_preflight" && calls > 1) || (scenario == "late_after_checkpoint" && calls > 2) {
					return at.Add(time.Minute)
				}
				return at
			}
			if scenario == "version_changed" {
				c := fixture.containers["sub2api"]
				c.Config.Labels["org.opencontainers.image.version"] = "v0.2.16-0"
				fixture.containers["sub2api"] = c
			}
			if scenario == "existing_gate" {
				require.NoError(t, os.MkdirAll(filepath.Dir(fixture.config.MaintenanceFile), 0o750))
				require.NoError(t, os.WriteFile(fixture.config.MaintenanceFile, []byte("other-maintenance"), 0o644))
			}
			require.Error(t, engine.Execute(context.Background(), store, job))
			current, err := store.Get(context.Background(), job.ID)
			require.NoError(t, err)
			require.Equal(t, kanoupdate.Missed, current.Status)
			if scenario == "existing_gate" {
				body, err := os.ReadFile(fixture.config.MaintenanceFile)
				require.NoError(t, err)
				require.Equal(t, "other-maintenance", string(body))
			} else {
				require.NoFileExists(t, fixture.config.MaintenanceFile)
			}
			for _, c := range fixture.containers {
				require.True(t, c.State.Running)
			}
			require.False(t, fixture.productionNew)
		})
	}
}

func TestRestartOnlyResumesAnEnteredMaintenance(t *testing.T) {
	for _, scenario := range []string{"claimed", "intent_before_gate", "entered", "complete_backup", "committed", "invalid_checkpoint", "terminal"} {
		t.Run(scenario, func(t *testing.T) {
			server, _, _ := testServer(t)
			job := reserve(t, server.Store, scenario)
			server.Now = func() time.Time { return job.ScheduledAt.Add(time.Second) }
			server.MaintenanceFile = filepath.Join(t.TempDir(), "maintenance")
			require.NoError(t, server.Store.transitionSchedule(context.Background(), job.ID, Queued, "ready", "Claimed"))
			state := UpgradeState{}
			if scenario != "claimed" {
				state.Maintenance = true
			}
			if scenario == "entered" {
				require.NoError(t, os.WriteFile(server.MaintenanceFile, []byte(job.ID), 0o644))
			}
			if scenario == "complete_backup" {
				state.BackupComplete = true
			}
			if scenario == "committed" {
				state.PublicCommitted = true
			}
			require.NoError(t, server.Store.Checkpoint(context.Background(), job.ID, state, ""))
			if scenario == "invalid_checkpoint" {
				_, err := server.Store.db.Exec("UPDATE jobs SET state='invalid'")
				require.NoError(t, err)
			}
			if scenario == "terminal" {
				require.NoError(t, server.Store.Update(context.Background(), job.ID, Failed, "Not started"))
			}
			server.expireOnStartup(context.Background())
			current, err := server.Store.Get(context.Background(), job.ID)
			require.NoError(t, err)
			want := Queued
			if scenario == "claimed" || scenario == "intent_before_gate" {
				want = kanoupdate.Missed
			}
			if scenario == "terminal" {
				want = Failed
			}
			require.Equal(t, want, current.Status)
		})
	}
}

func TestPreparationCancellationStopsPullWithoutRevivingReservation(t *testing.T) {
	server, _, _ := testServer(t)
	job := reserve(t, server.Store, "slow-pull")
	started, finished := make(chan struct{}), make(chan struct{})
	server.Executor = &preparingExecutor{prepare: func(ctx context.Context, _ *Job) error { close(started); <-ctx.Done(); return ctx.Err() }}
	go func() { server.advanceSchedule(context.Background(), job); close(finished) }()
	<-started
	require.NoError(t, server.Store.CancelSchedule(context.Background(), job.ID))
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled image pull did not stop")
	}
	current, err := server.Store.Get(context.Background(), job.ID)
	require.NoError(t, err)
	require.Equal(t, kanoupdate.Cancelled, current.Status)
}

func TestReservationConfirmationDateCannotSilentlyRollForward(t *testing.T) {
	server, _, _ := testServer(t)
	now := time.Now().UTC()
	at := kanoupdate.NextSchedule(now)
	request := startRequest()
	request.Schedule, request.ExpectedScheduleAt = true, &at
	_, _, err := server.Store.create(context.Background(), request, "v0.2.16-1", validManifest(), at)
	require.ErrorContains(t, err, "date changed")
	job, _, err := server.Store.create(context.Background(), request, "v0.2.16-1", validManifest(), now)
	require.NoError(t, err)
	replayed, _, err := server.Store.create(context.Background(), request, "v0.2.16-1", validManifest(), at.Add(24*time.Hour))
	require.NoError(t, err)
	require.Equal(t, job.ScheduledAt, replayed.ScheduledAt)
}

func TestOnlinePreparationKeepsOriginalDeploymentAndPinsManifest(t *testing.T) {
	engine, store, job, fixture := newDeploymentFixture(t, "")
	require.NoError(t, engine.PrepareSchedule(context.Background(), job))
	require.NoFileExists(t, fixture.config.MaintenanceFile)
	for _, c := range fixture.containers {
		require.True(t, c.State.Running)
	}
	current, err := store.Get(context.Background(), job.ID)
	require.NoError(t, err)
	require.Equal(t, Queued, current.Status)
	manifest := validManifest()
	manifest.Version = "v0.2.16-3"
	engine.Source = &fakeSource{release: &Release{Manifest: &manifest}}
	require.Error(t, engine.PrepareSchedule(context.Background(), job))
	require.False(t, fixture.productionNew)
}
