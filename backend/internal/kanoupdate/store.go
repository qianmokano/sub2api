package kanoupdate

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

	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
)

type Status string

const (
	Queued           Status = "queued"
	Pulling          Status = "pulling"
	Preflight        Status = "preflight"
	BackingUp        Status = "backing_up"
	Restoring        Status = "restoring"
	Verifying        Status = "verifying"
	Switching        Status = "switching"
	Checking         Status = "checking"
	RollingBack      Status = "rolling_back"
	Succeeded        Status = "succeeded"
	Failed           Status = "failed"
	FailedRolledBack Status = "failed_rolled_back"
	NeedsAttention   Status = "needs_attention"
)

func (s Status) Active() bool { return s != Succeeded && s != Failed && s != FailedRolledBack }

var (
	ErrBusy                = errors.New("another container upgrade needs to finish first")
	ErrIdempotencyConflict = errors.New("idempotency key was already used with a different update request")
	ErrNotFound            = errors.New("update job not found")
)

type StartRequest struct {
	Version        string `json:"version"`
	ExpectedDigest string `json:"expected_digest"`
	IdempotencyKey string `json:"idempotency_key"`
	Actor          string `json:"actor"`
}

type Job struct {
	ID            string          `json:"operation_id"`
	SourceVersion string          `json:"source_version"`
	TargetVersion string          `json:"target_version"`
	ImageDigest   string          `json:"image_digest"`
	Status        Status          `json:"status"`
	Message       string          `json:"message"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
	BackupID      string          `json:"backup_id,omitempty"`
	Manifest      Manifest        `json:"-"`
	State         json.RawMessage `json:"-"`
}

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
	db, err := sql.Open("sqlite", u.String()+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS jobs (
		id TEXT PRIMARY KEY, request_key TEXT UNIQUE NOT NULL, fingerprint TEXT NOT NULL,
		source_version TEXT NOT NULL, target_version TEXT NOT NULL, image_digest TEXT NOT NULL,
		status TEXT NOT NULL, message TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
		backup_id TEXT NOT NULL DEFAULT '', manifest TEXT NOT NULL, state TEXT NOT NULL DEFAULT '{}'
	);
	CREATE UNIQUE INDEX IF NOT EXISTS jobs_one_active ON jobs((1))
		WHERE status NOT IN ('succeeded','failed','failed_rolled_back');
	CREATE TABLE IF NOT EXISTS capabilities (
		token_hash TEXT PRIMARY KEY, job_id TEXT NOT NULL REFERENCES jobs(id), expires_at INTEGER NOT NULL
	);`)
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
	value, _ := json.Marshal([]string{request.Actor, request.Version, request.ExpectedDigest})
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
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM jobs WHERE status NOT IN ('succeeded','failed','failed_rolled_back')").Scan(&active); err != nil {
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
	now := time.Now().UTC()
	job := &Job{ID: "up-" + random, SourceVersion: source, TargetVersion: manifest.Version, ImageDigest: manifest.ImageDigest, Status: Queued, Message: "Upgrade queued", CreatedAt: now, UpdatedAt: now, Manifest: manifest, State: json.RawMessage(`{}`)}
	body, err := json.Marshal(manifest)
	if err != nil {
		return nil, false, err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO jobs(id,request_key,fingerprint,source_version,target_version,image_digest,status,message,created_at,updated_at,manifest) VALUES(?,?,?,?,?,?,?,?,?,?,?)", job.ID, key, fp, source, job.TargetVersion, job.ImageDigest, job.Status, job.Message, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), string(body))
	if err != nil {
		return nil, false, err
	}
	if err = tx.Commit(); err != nil {
		return nil, false, err
	}
	return job, true, nil
}

const jobColumns = "id,source_version,target_version,image_digest,status,message,created_at,updated_at,backup_id,manifest,state"

func readJob(row *sql.Row) (*Job, error) {
	var job Job
	var created, updated, manifest, state string
	err := row.Scan(&job.ID, &job.SourceVersion, &job.TargetVersion, &job.ImageDigest, &job.Status, &job.Message, &created, &updated, &job.BackupID, &manifest, &state)
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
	return &job, nil
}

func (s *Store) Get(ctx context.Context, id string) (*Job, error) {
	return readJob(s.db.QueryRowContext(ctx, "SELECT "+jobColumns+" FROM jobs WHERE id=?", id))
}

func (s *Store) Current(ctx context.Context) (*Job, error) {
	return readJob(s.db.QueryRowContext(ctx, "SELECT "+jobColumns+" FROM jobs ORDER BY CASE WHEN status NOT IN ('succeeded','failed','failed_rolled_back') THEN 0 ELSE 1 END,created_at DESC LIMIT 1"))
}

func (s *Store) Update(ctx context.Context, id string, status Status, message string) error {
	result, err := s.db.ExecContext(ctx, "UPDATE jobs SET status=?,message=?,updated_at=? WHERE id=?", status, message, time.Now().UTC().Format(time.RFC3339Nano), id)
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
	if _, err := s.Get(ctx, id); err != nil {
		return "", err
	}
	var bytes [32]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(bytes[:])
	sum := sha256.Sum256([]byte(token))
	_, err := s.db.ExecContext(ctx, "INSERT INTO capabilities(token_hash,job_id,expires_at) VALUES(?,?,?)", hex.EncodeToString(sum[:]), id, time.Now().Add(24*time.Hour).Unix())
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
