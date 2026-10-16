package kanoagent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/kanoupdate"
	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
)

var (
	ErrBusy                = errors.New("another container upgrade needs to finish first")
	ErrIdempotencyConflict = errors.New("idempotency key was already used with a different update request")
	ErrNotFound            = errors.New("update job not found")
)

type Store struct {
	db *sql.DB
	mu sync.Mutex
}

// OpenStore owns only the updater's private database, never the app database.
func OpenStore(directory string) (*Store, error) {
	if !filepath.IsAbs(directory) {
		return nil, errors.New("updater state directory must be absolute")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, err
	}
	filename := filepath.Join(directory, "jobs.sqlite")
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err = file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err = file.Close(); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: filename}
	db, err := sql.Open("sqlite", u.String()+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`BEGIN IMMEDIATE;
	CREATE TABLE IF NOT EXISTS jobs (
		id TEXT PRIMARY KEY, request_key TEXT UNIQUE NOT NULL, fingerprint TEXT NOT NULL,
		source_version TEXT NOT NULL, target_version TEXT NOT NULL, image_digest TEXT NOT NULL,
		status TEXT NOT NULL, message TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
		backup_id TEXT NOT NULL DEFAULT '', manifest TEXT NOT NULL, state TEXT NOT NULL DEFAULT '{}', actor TEXT NOT NULL
	);
	DROP INDEX IF EXISTS jobs_one_active;
	CREATE UNIQUE INDEX jobs_one_active ON jobs((1))
		WHERE status NOT IN ('succeeded','failed','failed_rolled_back','cancelled','missed');
	CREATE TABLE IF NOT EXISTS schedules (
		job_id TEXT PRIMARY KEY REFERENCES jobs(id), scheduled_at TEXT NOT NULL, preparation TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS capabilities (
		token_hash TEXT PRIMARY KEY, job_id TEXT NOT NULL REFERENCES jobs(id), expires_at INTEGER NOT NULL
	);
	CREATE TABLE IF NOT EXISTS job_steps (
		id INTEGER PRIMARY KEY, job_id TEXT NOT NULL REFERENCES jobs(id), status TEXT NOT NULL, started_at TEXT NOT NULL
	);
	COMMIT;`)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func randomID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}
func fingerprint(request StartRequest) string {
	parts := []string{request.Actor, request.Version, request.ExpectedDigest}
	if request.Schedule {
		parts = append(parts, "schedule")
		if request.ExpectedScheduleAt != nil {
			parts = append(parts, request.ExpectedScheduleAt.UTC().Format(time.RFC3339Nano))
		}
	}
	value, _ := json.Marshal(parts)
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
func scopedKey(request StartRequest) string {
	sum := sha256.Sum256([]byte(request.Actor + "\x00" + request.IdempotencyKey))
	return hex.EncodeToString(sum[:])
}

// FindRequest checks persisted deduplication before contacting release services.
// A newer release or daemon restart must not break retrying an accepted request.
func (s *Store) FindRequest(ctx context.Context, request StartRequest) (*Job, error) {
	var id, existing string
	err := s.db.QueryRowContext(ctx, "SELECT id,fingerprint FROM jobs WHERE request_key=?", scopedKey(request)).Scan(&id, &existing)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if existing != fingerprint(request) {
		return nil, ErrIdempotencyConflict
	}
	return s.Get(ctx, id)
}

// Create performs both deduplication and the global active-job check atomically.
// Replaying the same request after completion still returns its original job.
func (s *Store) Create(ctx context.Context, request StartRequest, source string, manifest Manifest) (*Job, bool, error) {
	return s.create(ctx, request, source, manifest, time.Now().UTC())
}

func (s *Store) create(ctx context.Context, request StartRequest, source string, manifest Manifest, now time.Time) (*Job, bool, error) {
	if request.Actor == "" || len(request.Actor) > 128 || request.IdempotencyKey == "" || len(request.IdempotencyKey) > 256 {
		return nil, false, errors.New("invalid update request identity")
	}
	if err := manifest.Validate(); err != nil {
		return nil, false, err
	}
	if request.Version != manifest.Version || request.ExpectedDigest != manifest.ImageDigest {
		return nil, false, errors.New("update target changed; check releases again")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback() }()
	key, fp := scopedKey(request), fingerprint(request)
	var id, existingFingerprint string
	err = tx.QueryRowContext(ctx, "SELECT id,fingerprint FROM jobs WHERE request_key=?", key).Scan(&id, &existingFingerprint)
	if err == nil {
		if fp != existingFingerprint {
			return nil, false, ErrIdempotencyConflict
		}
		if err = tx.Commit(); err != nil {
			return nil, false, err
		}
		job, getErr := s.Get(ctx, id)
		return job, false, getErr
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	var active int
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM jobs WHERE status NOT IN ('succeeded','failed','failed_rolled_back','cancelled','missed')").Scan(&active); err != nil {
		return nil, false, err
	}
	if active != 0 {
		return nil, false, ErrBusy
	}
	comparison, err := CompareVersions(manifest.Version, source)
	if err != nil || comparison <= 0 {
		return nil, false, errors.New("target is not newer than the running fork")
	}
	comparison, err = CompareVersions(source, manifest.Validation.MinimumSourceVersion)
	if err != nil || comparison < 0 {
		return nil, false, errors.New("this source version requires a manual upgrade")
	}
	random, err := randomID()
	if err != nil {
		return nil, false, err
	}
	job := &Job{ID: "up-" + random, SourceVersion: source, TargetVersion: manifest.Version, ImageDigest: manifest.ImageDigest, Status: Queued, Message: "Upgrade queued", CreatedAt: now, UpdatedAt: now, Manifest: manifest, State: json.RawMessage(`{}`), Actor: request.Actor}
	if request.Schedule {
		at := kanoupdate.NextSchedule(now)
		if request.ExpectedScheduleAt != nil && !at.Equal(*request.ExpectedScheduleAt) {
			return nil, false, errors.New("reservation date changed; refresh the version menu before reserving again")
		}
		job.ScheduledAt, job.Preparation, job.Status, job.Message = &at, "pending", kanoupdate.Scheduled, "Reserved for the next Beijing 04:00; preparing online"
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		return nil, false, err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO jobs(id,request_key,fingerprint,source_version,target_version,image_digest,status,message,created_at,updated_at,manifest,actor) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)", job.ID, key, fp, source, job.TargetVersion, job.ImageDigest, job.Status, job.Message, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), string(body), request.Actor)
	if err != nil {
		return nil, false, err
	}
	if request.Schedule {
		if _, err = tx.ExecContext(ctx, "INSERT INTO schedules(job_id,scheduled_at,preparation) VALUES(?,?,?)", job.ID, job.ScheduledAt.Format(time.RFC3339Nano), job.Preparation); err != nil {
			return nil, false, err
		}
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO job_steps(job_id,status,started_at) VALUES(?,?,?)", job.ID, job.Status, now.Format(time.RFC3339Nano)); err != nil {
		return nil, false, err
	}
	if err = tx.Commit(); err != nil {
		return nil, false, err
	}
	return job, true, nil
}

const jobColumns = "id,source_version,target_version,image_digest,status,message,created_at,updated_at,backup_id,manifest,state,actor,COALESCE((SELECT scheduled_at FROM schedules WHERE job_id=jobs.id),''),COALESCE((SELECT preparation FROM schedules WHERE job_id=jobs.id),'')"

func readJob(row *sql.Row) (*Job, error) {
	var job Job
	var created, updated, manifest, state, scheduled string
	err := row.Scan(&job.ID, &job.SourceVersion, &job.TargetVersion, &job.ImageDigest, &job.Status, &job.Message, &created, &updated, &job.BackupID, &manifest, &state, &job.Actor, &scheduled, &job.Preparation)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if job.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return nil, err
	}
	if job.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated); err != nil {
		return nil, err
	}
	if err = json.Unmarshal([]byte(manifest), &job.Manifest); err != nil {
		return nil, err
	}
	job.State = json.RawMessage(state)
	if scheduled != "" {
		at, parseErr := time.Parse(time.RFC3339Nano, scheduled)
		if parseErr != nil {
			return nil, parseErr
		}
		job.ScheduledAt = &at
	}
	return &job, nil
}

func (s *Store) Get(ctx context.Context, id string) (*Job, error) {
	job, err := readJob(s.db.QueryRowContext(ctx, "SELECT "+jobColumns+" FROM jobs WHERE id=?", id))
	if err != nil {
		return nil, err
	}
	return s.withSteps(ctx, job)
}

func (s *Store) Current(ctx context.Context) (*Job, error) {
	job, err := readJob(s.db.QueryRowContext(ctx, "SELECT "+jobColumns+" FROM jobs ORDER BY CASE WHEN status NOT IN ('succeeded','failed','failed_rolled_back','cancelled','missed') THEN 0 ELSE 1 END,rowid DESC LIMIT 1"))
	if err != nil {
		return nil, err
	}
	return s.withSteps(ctx, job)
}

func (s *Store) withSteps(ctx context.Context, job *Job) (*Job, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT status,started_at FROM job_steps WHERE job_id=? ORDER BY id", job.ID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var step kanoupdate.Step
		var started string
		if err = rows.Scan(&step.Status, &started); err != nil {
			return nil, err
		}
		if step.StartedAt, err = time.Parse(time.RFC3339Nano, started); err != nil {
			return nil, err
		}
		job.Steps = append(job.Steps, step)
	}
	return job, rows.Err()
}

func (s *Store) Update(ctx context.Context, id string, status Status, message string) error {
	if !status.Valid() {
		return errors.New("invalid upgrade step")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := tx.ExecContext(ctx, "UPDATE jobs SET status=?,message=?,updated_at=? WHERE id=?", status, message, now, id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrNotFound
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO job_steps(job_id,status,started_at) VALUES(?,?,?)", id, status, now); err != nil {
		return err
	}
	return tx.Commit()
}

// Checkpoint is committed before a destructive step. Its private payload may
// contain runtime configuration; it is excluded from every public JSON response.
func (s *Store) Checkpoint(ctx context.Context, id string, state any, backupID string) error {
	body, err := json.Marshal(state)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, "UPDATE jobs SET state=?,backup_id=?,updated_at=? WHERE id=?", string(body), backupID, time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) Grant(ctx context.Context, id string) (string, error) {
	job, err := s.Get(ctx, id)
	if err != nil {
		return "", err
	}
	var bytes [32]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(bytes[:])
	sum := sha256.Sum256([]byte(token))
	_, err = s.db.ExecContext(ctx, "INSERT INTO capabilities(token_hash,job_id,expires_at) VALUES(?,?,?)", hex.EncodeToString(sum[:]), id, job.SessionExpiresAt(time.Now()).Unix())
	if err != nil {
		return "", err
	}
	_, _ = s.db.ExecContext(ctx, "DELETE FROM capabilities WHERE expires_at<?", time.Now().Unix())
	return token, nil
}

func (s *Store) Authorized(ctx context.Context, id, token string) bool {
	if len(token) != 64 {
		return false
	}
	sum := sha256.Sum256([]byte(token))
	var count int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM capabilities WHERE token_hash=? AND job_id=? AND expires_at>?", hex.EncodeToString(sum[:]), id, time.Now().Unix()).Scan(&count)
	return err == nil && count == 1
}

// AcquireHostLock prevents a second daemon, CLI invocation or recovery worker
// from controlling the same deployment. The kernel releases it after a crash.
func AcquireHostLock(path string) (*os.File, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("updater lock path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("updater deployment lock unavailable: %w", err)
	}
	return file, nil
}
