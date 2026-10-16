package kanoagent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/kanoupdate"
)

// LegacyCurrent keeps unknown reservation states away from older app startup
// coordinators, which conservatively pause background writes on unknown states.
func (s *Store) LegacyCurrent(ctx context.Context) (*Job, error) {
	job, err := readJob(s.db.QueryRowContext(ctx, "SELECT "+jobColumns+" FROM jobs WHERE status NOT IN ('scheduled','cancelled','missed') ORDER BY CASE WHEN status NOT IN ('succeeded','failed','failed_rolled_back') THEN 0 ELSE 1 END,rowid DESC LIMIT 1"))
	if err != nil {
		return nil, err
	}
	return s.withSteps(ctx, job)
}

func (s *Store) Schedule(ctx context.Context) (*Job, error) {
	job, err := readJob(s.db.QueryRowContext(ctx, "SELECT "+jobColumns+" FROM jobs WHERE id IN (SELECT job_id FROM schedules) ORDER BY rowid DESC LIMIT 1"))
	if err != nil {
		return nil, err
	}
	return s.withSteps(ctx, job)
}

// transitionSchedule is a compare-and-swap. Cancellation and the timer cannot
// both win, and a late preparation result cannot resurrect a cancelled job.
func (s *Store) transitionSchedule(ctx context.Context, id string, status Status, preparation, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := tx.ExecContext(ctx, "UPDATE jobs SET status=?,message=?,updated_at=? WHERE id=? AND status='scheduled'", status, message, now, id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrBusy
	}
	if _, err = tx.ExecContext(ctx, "UPDATE schedules SET preparation=? WHERE job_id=?", preparation, id); err != nil {
		return err
	}
	if status != kanoupdate.Scheduled {
		if _, err = tx.ExecContext(ctx, "INSERT INTO job_steps(job_id,status,started_at) VALUES(?,?,?)", id, status, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) CancelSchedule(ctx context.Context, id string) error {
	job, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if job.Status == kanoupdate.Cancelled {
		return nil
	}
	return s.transitionSchedule(ctx, id, kanoupdate.Cancelled, job.Preparation, "Reservation cancelled; gateway remains available")
}

type schedulePreparer interface {
	PrepareSchedule(context.Context, *Job) error
}

// The durable gate distinguishes an intent checkpoint from an entered window.
// A crash or I/O failure before the gate must not start maintenance later.
func safeBeforeMaintenance(path string, state *UpgradeState) bool {
	if state.ProductionTouched || state.PublicCommitted || state.BackupComplete {
		return false
	}
	if !state.Maintenance {
		return true
	}
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return os.IsNotExist(err)
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Server) missSchedule(ctx context.Context, job *Job, message string) {
	_ = s.Store.transitionSchedule(ctx, job.ID, kanoupdate.Missed, job.Preparation, message+"; reserve another date manually")
}

// On a daemon restart an already due reservation is never caught up, including
// within its original minute. Only a continuously running worker may claim it.
func (s *Server) expireOnStartup(ctx context.Context) {
	job, err := s.Store.Schedule(ctx)
	if err != nil || job.ScheduledAt == nil || s.now().Before(*job.ScheduledAt) {
		return
	}
	if job.Status == kanoupdate.Scheduled {
		s.missSchedule(ctx, job, "Updater restarted after the reservation time")
	} else if job.Status.Active() {
		var state UpgradeState
		// An interrupted maintenance must still recover through the existing path.
		if json.Unmarshal(job.State, &state) != nil {
			return
		}
		if safeBeforeMaintenance(s.MaintenanceFile, &state) {
			_ = s.Store.Update(ctx, job.ID, kanoupdate.Missed, "Updater restarted before maintenance; reserve another date manually")
		}
	}
}

func (s *Server) advanceSchedule(ctx context.Context, job *Job) {
	now := s.now()
	if !now.Before(*job.ScheduledAt) {
		if !job.InScheduleWindow(now) || job.Preparation != "ready" {
			s.missSchedule(ctx, job, "Reservation window missed or preparation incomplete")
			return
		}
		_ = s.Store.transitionSchedule(ctx, job.ID, Queued, job.Preparation, "Reservation due; running final preflight before maintenance")
		return
	}
	if job.Preparation == "ready" {
		return
	}
	preparer, ok := s.Executor.(schedulePreparer)
	if !ok {
		s.missSchedule(ctx, job, "Host updater does not support online preparation")
		return
	}
	prepareCtx, cancel := context.WithDeadline(ctx, *job.ScheduledAt)
	defer cancel()
	// Cancellation frees the reservation immediately and also stops a long pull.
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-watchDone:
				return
			case <-prepareCtx.Done():
				return
			case <-ticker.C:
				current, err := s.Store.Get(prepareCtx, job.ID)
				if err != nil || current.Status != kanoupdate.Scheduled {
					cancel()
					return
				}
			}
		}
	}()
	err := preparer.PrepareSchedule(prepareCtx, job)
	if ctx.Err() != nil {
		return // A future reservation survives an orderly daemon restart.
	}
	if err != nil || !s.now().Before(*job.ScheduledAt) {
		job.Preparation = "failed"
		message := "Online image preparation failed"
		if errors.Is(err, context.DeadlineExceeded) || !s.now().Before(*job.ScheduledAt) {
			message = "Image preparation did not finish before the reservation"
		}
		s.missSchedule(ctx, job, message)
		return
	}
	_ = s.Store.transitionSchedule(ctx, job.ID, kanoupdate.Scheduled, "ready", "Image verified; waiting for Beijing 04:00")
}
