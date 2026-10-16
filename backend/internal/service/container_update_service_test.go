//go:build unit

package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/kanoupdate"
	"github.com/stretchr/testify/require"
)

type containerUpdaterStub struct {
	availability *kanoupdate.Availability
	session      *kanoupdate.Session
	request      kanoupdate.StartRequest
	err          error
}

func (s *containerUpdaterStub) Release(context.Context, bool) (*kanoupdate.Availability, error) {
	return s.availability, s.err
}
func (s *containerUpdaterStub) Start(_ context.Context, request kanoupdate.StartRequest) (*kanoupdate.Session, error) {
	s.request = request
	return s.session, s.err
}
func (s *containerUpdaterStub) Current(context.Context) (*kanoupdate.Session, error) {
	return s.session, s.err
}
func (s *containerUpdaterStub) Schedule(context.Context) (*kanoupdate.Session, error) {
	return s.session, s.err
}
func (s *containerUpdaterStub) CreateSchedule(ctx context.Context, request kanoupdate.StartRequest) (*kanoupdate.Session, error) {
	return s.Start(ctx, request)
}
func (s *containerUpdaterStub) CancelSchedule(context.Context, string) error { return s.err }

func TestContainerReservationsRequireDeclaredHostCapability(t *testing.T) {
	ctx := context.Background()
	svc := NewUpdateService(nil, nil, "v0.2.16-1", "kano-container")
	_, err := svc.ContainerSchedule(ctx)
	require.Error(t, err)
	updater := &containerUpdaterStub{availability: &kanoupdate.Availability{Ready: true, ProtocolVersion: 1}}
	svc.WithContainerUpdater(updater)
	_, err = svc.ContainerCreateSchedule(ctx, kanoupdate.StartRequest{})
	require.Error(t, err)
	require.Error(t, svc.ContainerCancelSchedule(ctx, "up-test"))
	updater.availability.Capabilities = []string{kanoupdate.ScheduleCapability}
	at := kanoupdate.NextSchedule(time.Now())
	updater.availability.NextScheduleAt = &at
	updater.session = &kanoupdate.Session{Job: &kanoupdate.Job{Status: kanoupdate.Scheduled, ScheduledAt: &at}}
	request := kanoupdate.StartRequest{Version: "v0.2.16-2", ExpectedScheduleAt: &at}
	session, err := svc.ContainerCreateSchedule(ctx, request)
	require.NoError(t, err)
	require.Equal(t, updater.session, session)
	require.Equal(t, request, updater.request)
	session, err = svc.ContainerSchedule(ctx)
	require.NoError(t, err)
	require.Equal(t, updater.session, session)
	require.NoError(t, svc.ContainerCancelSchedule(ctx, "up-test"))
	info, err := svc.CheckUpdate(ctx, false)
	require.NoError(t, err)
	require.True(t, info.ScheduleReady)
	require.Equal(t, at, *info.NextScheduleAt)
	updater.err = errors.New("private socket details")
	_, err = svc.ContainerSchedule(ctx)
	require.Error(t, err)
	svc = NewUpdateService(nil, nil, "v0.2.16", "release")
	_, err = svc.ContainerSchedule(ctx)
	require.ErrorIs(t, err, ErrForkUpdateManaged)
}

func containerTestManifest() kanoupdate.Manifest {
	return kanoupdate.Manifest{SchemaVersion: 1, Version: "v0.2.16-2", Image: kanoupdate.ImageRepository + ":v0.2.16-2", ImageDigest: "sha256:" + strings.Repeat("a", 64), Revision: strings.Repeat("b", 40), MinimumAgentVersion: kanoupdate.AgentVersion, Migrations: map[string]string{"241.sql": strings.Repeat("c", 64)}, Validation: kanoupdate.ValidationPolicy{MinimumSourceVersion: "v0.2.16-1"}}
}

func TestContainerUpdateAvailabilityAndDelegation(t *testing.T) {
	ctx := context.Background()
	svc := NewUpdateService(nil, nil, "v0.2.16-1", "kano-container")
	info, err := svc.CheckUpdate(ctx, false)
	require.NoError(t, err)
	require.False(t, info.UpdaterReady)
	require.NotEmpty(t, info.UnavailableReason)
	_, err = svc.ContainerStart(ctx, kanoupdate.StartRequest{})
	require.Error(t, err)
	_, err = svc.ContainerCurrent(ctx)
	require.Error(t, err)
	manifest := containerTestManifest()
	updater := &containerUpdaterStub{availability: &kanoupdate.Availability{Ready: true, ProtocolVersion: 1, AgentVersion: kanoupdate.AgentVersion, CurrentVersion: "v0.2.16-1", Release: &kanoupdate.Release{Tag: manifest.Version, Manifest: &manifest}}, session: &kanoupdate.Session{Job: &kanoupdate.Job{ID: "up-" + strings.Repeat("a", 32)}}}
	svc.WithContainerUpdater(updater)
	info, err = svc.CheckUpdate(ctx, true)
	require.NoError(t, err)
	require.True(t, info.UpdaterReady)
	require.True(t, info.HasUpdate)
	require.Equal(t, manifest.ImageDigest, info.TargetImageDigest)
	require.Empty(t, info.UnavailableReason)
	request := kanoupdate.StartRequest{Version: manifest.Version, ExpectedDigest: manifest.ImageDigest, IdempotencyKey: "retry", Actor: "admin:1"}
	session, err := svc.ContainerStart(ctx, request)
	require.NoError(t, err)
	require.Equal(t, updater.session, session)
	require.Equal(t, request, updater.request)
	session, err = svc.ContainerCurrent(ctx)
	require.NoError(t, err)
	require.Equal(t, updater.session, session)
	require.ErrorIs(t, svc.PerformUpdate(ctx), ErrForkUpdateManaged)
	require.ErrorIs(t, svc.Rollback(), ErrForkUpdateManaged)
	for _, mutate := range []func(){func() { updater.availability.Busy = true }, func() { updater.availability.Ready = false }, func() { updater.availability.ProtocolVersion = 2 }, func() { updater.availability.Release = nil }, func() { updater.availability.Release.Manifest = nil }, func() { updater.availability.Release.Manifest.MinimumAgentVersion = "9.0.0" }, func() { updater.availability.CurrentVersion = "v0.2.15-1" }} {
		copyManifest := containerTestManifest()
		updater.availability = &kanoupdate.Availability{Ready: true, ProtocolVersion: 1, CurrentVersion: "v0.2.16-1", Release: &kanoupdate.Release{Tag: copyManifest.Version, Manifest: &copyManifest}}
		mutate()
		info, err = svc.CheckUpdate(ctx, true)
		require.NoError(t, err)
		require.NotEmpty(t, info.UnavailableReason)
		require.Empty(t, info.TargetImageDigest)
	}
	updater.err = errors.New("transport unavailable")
	info, err = svc.CheckUpdate(ctx, true)
	require.NoError(t, err)
	require.False(t, info.UpdaterReady)
	svc = NewUpdateService(nil, nil, "v0.2.16", "release")
	require.False(t, svc.ContainerMode())
	_, err = svc.ContainerStart(ctx, request)
	require.ErrorIs(t, err, ErrForkUpdateManaged)
	_, err = svc.ContainerCurrent(ctx)
	require.ErrorIs(t, err, ErrForkUpdateManaged)
}
