//go:build unit

package kanoupdate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func startRequest() StartRequest {
	m := validManifest()
	return StartRequest{Version: m.Version, ExpectedDigest: m.ImageDigest, Actor: "admin:1", IdempotencyKey: "same-request"}
}

func TestStoreIdempotencyConcurrencyAndPersistence(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	store, err := OpenStore(directory)
	require.NoError(t, err)
	job, created, err := store.Create(ctx, startRequest(), "v0.2.16-1", validManifest())
	require.NoError(t, err)
	require.True(t, created)
	duplicate, created, err := store.Create(ctx, startRequest(), "v0.2.16-1", validManifest())
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, job.ID, duplicate.ID)
	request := startRequest()
	request.ExpectedDigest = "sha256:" + strings.Repeat("d", 64)
	manifest := validManifest()
	manifest.ImageDigest = request.ExpectedDigest
	_, _, err = store.Create(ctx, request, "v0.2.16-1", manifest)
	require.ErrorIs(t, err, ErrIdempotencyConflict)
	request = startRequest()
	request.IdempotencyKey = "second"
	_, _, err = store.Create(ctx, request, "v0.2.16-1", validManifest())
	require.ErrorIs(t, err, ErrBusy)
	require.NoError(t, store.Checkpoint(ctx, job.ID, map[string]string{"private_secret": "must never be serialized"}, "backup-test"))
	require.NoError(t, store.Update(ctx, job.ID, Restoring, "Restoring backup"))
	token, err := store.Grant(ctx, job.ID)
	require.NoError(t, err)
	require.True(t, store.Authorized(ctx, job.ID, token))
	require.False(t, store.Authorized(ctx, "other", token))
	require.False(t, store.Authorized(ctx, job.ID, "bad"))
	_, err = store.db.Exec("UPDATE capabilities SET expires_at=?", time.Now().Add(-time.Second).Unix())
	require.NoError(t, err)
	require.False(t, store.Authorized(ctx, job.ID, token))
	require.NoError(t, store.Close())
	store, err = OpenStore(directory)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	current, err := store.Current(ctx)
	require.NoError(t, err)
	require.Equal(t, Restoring, current.Status)
	require.Equal(t, job.ID, current.ID)
	require.Equal(t, "backup-test", current.BackupID)
	body, err := json.Marshal(current)
	require.NoError(t, err)
	require.NotContains(t, string(body), "private_secret")
	require.NotContains(t, string(body), "migrations")
	require.NoError(t, store.Update(ctx, job.ID, Succeeded, "Upgrade complete"))
	duplicate, created, err = store.Create(ctx, startRequest(), "v0.2.16-2", validManifest())
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, job.ID, duplicate.ID)
	request = startRequest()
	request.IdempotencyKey = "new-request"
	_, created, err = store.Create(ctx, request, "v0.2.16-1", validManifest())
	require.NoError(t, err)
	require.True(t, created)
	info, err := os.Stat(filepath.Join(directory, "jobs.sqlite"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestStoreValidationAndAttentionStaysLocked(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	_, err = store.Current(ctx)
	require.ErrorIs(t, err, ErrNotFound)
	for _, mutate := range []func(*StartRequest){func(r *StartRequest) { r.Actor = "" }, func(r *StartRequest) { r.IdempotencyKey = "" }, func(r *StartRequest) { r.Version = "v0.2.16-3" }, func(r *StartRequest) { r.ExpectedDigest = "bad" }} {
		request := startRequest()
		mutate(&request)
		_, _, err = store.Create(ctx, request, "v0.2.16-1", validManifest())
		require.Error(t, err)
	}
	for _, source := range []string{"bad", "v0.2.16-2", "v0.2.17-1", "v0.2.14-kano.1"} {
		_, _, err = store.Create(ctx, startRequest(), source, validManifest())
		require.Error(t, err)
	}
	job, _, err := store.Create(ctx, startRequest(), "v0.2.16-1", validManifest())
	require.NoError(t, err)
	require.NoError(t, store.Update(ctx, job.ID, NeedsAttention, "Recovery needs attention"))
	request := startRequest()
	request.IdempotencyKey = "different"
	_, _, err = store.Create(ctx, request, "v0.2.16-1", validManifest())
	require.ErrorIs(t, err, ErrBusy)
	require.ErrorIs(t, store.Update(ctx, "missing", Failed, "Failure"), ErrNotFound)
	require.ErrorIs(t, store.Checkpoint(ctx, "missing", map[string]string{}, ""), ErrNotFound)
	_, err = store.Grant(ctx, "missing")
	require.ErrorIs(t, err, ErrNotFound)
	_, err = OpenStore("relative")
	require.Error(t, err)
	for _, status := range []Status{Queued, RollingBack, NeedsAttention} {
		require.True(t, status.Active())
	}
	for _, status := range []Status{Succeeded, Failed, FailedRolledBack} {
		require.False(t, status.Active())
	}
}

func TestHostLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.lock")
	lock, err := AcquireHostLock(path)
	require.NoError(t, err)
	_, err = AcquireHostLock(path)
	require.Error(t, err)
	require.NoError(t, lock.Close())
	lock, err = AcquireHostLock(path)
	require.NoError(t, err)
	require.NoError(t, lock.Close())
	_, err = AcquireHostLock("relative")
	require.Error(t, err)
}
