package service

import (
	"context"
	"slices"

	"github.com/Wei-Shaw/sub2api/internal/kanoupdate"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// ContainerUpdater is a restricted client. Docker and backup operations run in
// the independent host service; this application has no privileged executor.
type ContainerUpdater interface {
	Release(context.Context, bool) (*kanoupdate.Availability, error)
	Start(context.Context, kanoupdate.StartRequest) (*kanoupdate.Session, error)
	Current(context.Context) (*kanoupdate.Session, error)
}

func (s *UpdateService) WithContainerUpdater(updater ContainerUpdater) *UpdateService {
	s.containerUpdater = updater
	return s
}
func (s *UpdateService) ContainerMode() bool { return s.isManagedFork() }

func (s *UpdateService) checkContainerUpdate(ctx context.Context, force bool) *UpdateInfo {
	info := &UpdateInfo{CurrentVersion: s.currentVersion, LatestVersion: s.currentVersion, BuildType: "kano-container", UpdateMode: "container", UnavailableReason: "Install and connect the host updater to enable online upgrades"}
	if s.containerUpdater == nil {
		return info
	}
	available, err := s.containerUpdater.Release(ctx, force)
	if err != nil || available == nil {
		info.UnavailableReason = "Host updater is unavailable; inspect its systemd service"
		return info
	}
	info.UpdaterReady = available.Ready && available.ProtocolVersion == kanoupdate.ProtocolVersion
	info.UpdaterVersion = available.AgentVersion
	info.ScheduleReady = info.UpdaterReady && slices.Contains(available.Capabilities, kanoupdate.ScheduleCapability)
	info.NextScheduleAt = available.NextScheduleAt
	info.UpdateBusy = available.Busy
	info.Warning = available.Warning
	if !info.UpdaterReady {
		info.UnavailableReason = "Host updater protocol is incompatible"
		return info
	}
	if available.CurrentVersion != "" {
		info.CurrentVersion = available.CurrentVersion
	}
	info.UnavailableReason = ""
	if available.Release == nil {
		info.UnavailableReason = "No verified fork release is available"
		return info
	}
	release := available.Release
	info.LatestVersion = release.Tag
	info.ReleaseInfo = &ReleaseInfo{Name: release.Name, Body: release.Body, PublishedAt: release.PublishedAt, HTMLURL: release.URL}
	comparison, err := kanoupdate.CompareVersions(info.LatestVersion, info.CurrentVersion)
	info.HasUpdate = err == nil && comparison > 0
	if available.Busy {
		info.UnavailableReason = "An upgrade or recovery is already in progress"
		return info
	}
	if release.Manifest == nil {
		info.UnavailableReason = release.UnavailableReason
		if info.UnavailableReason == "" {
			info.UnavailableReason = "This release requires a manual upgrade"
		}
		return info
	}
	if err = release.Manifest.Validate(); err != nil {
		info.UnavailableReason = "This release requires a newer updater or manual validation"
		return info
	}
	comparison, err = kanoupdate.CompareVersions(info.CurrentVersion, release.Manifest.Validation.MinimumSourceVersion)
	if err != nil || comparison < 0 {
		info.UnavailableReason = "Current deployment requires a manual upgrade before this release"
		return info
	}
	info.TargetImageDigest = release.Manifest.ImageDigest
	return info
}

func (s *UpdateService) ContainerStart(ctx context.Context, request kanoupdate.StartRequest) (*kanoupdate.Session, error) {
	if !s.ContainerMode() {
		return nil, ErrForkUpdateManaged
	}
	if s.containerUpdater == nil {
		return nil, infraerrors.ServiceUnavailable("UPDATER_UNAVAILABLE", "Host updater is not connected")
	}
	return s.containerUpdater.Start(ctx, request)
}
func (s *UpdateService) ContainerCurrent(ctx context.Context) (*kanoupdate.Session, error) {
	if !s.ContainerMode() {
		return nil, ErrForkUpdateManaged
	}
	if s.containerUpdater == nil {
		return nil, infraerrors.ServiceUnavailable("UPDATER_UNAVAILABLE", "Host updater is not connected")
	}
	return s.containerUpdater.Current(ctx)
}

type ContainerScheduler interface {
	Schedule(context.Context) (*kanoupdate.Session, error)
	CreateSchedule(context.Context, kanoupdate.StartRequest) (*kanoupdate.Session, error)
	CancelSchedule(context.Context, string) error
}

func (s *UpdateService) containerScheduler(ctx context.Context) (ContainerScheduler, error) {
	if !s.ContainerMode() {
		return nil, ErrForkUpdateManaged
	}
	scheduler, ok := s.containerUpdater.(ContainerScheduler)
	if !ok {
		return nil, infraerrors.ServiceUnavailable("UPDATER_UNAVAILABLE", "Update the host service to version 1.1.0 or newer to enable reservations")
	}
	available, err := s.containerUpdater.Release(ctx, false)
	if err != nil || available == nil || !available.Ready || available.ProtocolVersion != kanoupdate.ProtocolVersion || !slices.Contains(available.Capabilities, kanoupdate.ScheduleCapability) {
		return nil, infraerrors.ServiceUnavailable("UPDATER_UNAVAILABLE", "Update and connect the host service with reservation support (1.1.0 or newer)")
	}
	return scheduler, nil
}

func (s *UpdateService) ContainerSchedule(ctx context.Context) (*kanoupdate.Session, error) {
	scheduler, err := s.containerScheduler(ctx)
	if err != nil {
		return nil, err
	}
	return scheduler.Schedule(ctx)
}

func (s *UpdateService) ContainerCreateSchedule(ctx context.Context, request kanoupdate.StartRequest) (*kanoupdate.Session, error) {
	scheduler, err := s.containerScheduler(ctx)
	if err != nil {
		return nil, err
	}
	return scheduler.CreateSchedule(ctx, request)
}

func (s *UpdateService) ContainerCancelSchedule(ctx context.Context, id string) error {
	scheduler, err := s.containerScheduler(ctx)
	if err != nil {
		return err
	}
	return scheduler.CancelSchedule(ctx, id)
}
