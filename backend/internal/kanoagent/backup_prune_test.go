//go:build unit

package kanoagent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// pruneFixture builds a store with one finished previous job and one current
// job, an empty backup directory, and an engine whose docker runner reports a
// single container without bind mounts.
func pruneFixture(t *testing.T) (*Engine, *Store, *Job, *Job, string) {
	t.Helper()
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	previous, _, err := store.Create(ctx, startRequest(), "v0.2.16-1", validManifest())
	require.NoError(t, err)
	require.NoError(t, store.Update(ctx, previous.ID, Succeeded, "Upgrade complete"))
	request := startRequest()
	request.IdempotencyKey = "next-request"
	current, _, err := store.Create(ctx, request, "v0.2.16-1", validManifest())
	require.NoError(t, err)
	backupDir := t.TempDir()
	engine := &Engine{
		Config: Config{BackupDir: backupDir, DeploymentDir: t.TempDir()},
		Runner: mountedRunner(t, nil),
	}
	return engine, store, previous, current, backupDir
}

// mountedRunner answers the prune probe with one container mounting sources.
func mountedRunner(t *testing.T, mountSources []string) Runner {
	t.Helper()
	mounts := make([]Mount, 0, len(mountSources))
	for _, source := range mountSources {
		mounts = append(mounts, Mount{Source: source})
	}
	return runnerFunc(func(_ context.Context, args []string, _ io.Reader, _ io.Writer) ([]byte, error) {
		if strings.Join(args, " ") == "docker ps -aq" {
			return []byte("container1\n"), nil
		}
		if len(args) == 3 && args[0] == "docker" && args[1] == "inspect" && args[2] == "container1" {
			value, err := json.Marshal([]Container{{Mounts: mounts}})
			require.NoError(t, err)
			return value, nil
		}
		return nil, errors.New("unexpected docker call " + strings.Join(args, " "))
	})
}

func backupDirName(jobID string) string {
	return "online-" + time.Now().UTC().Format("20060102T150405Z") + "-" + strings.TrimPrefix(jobID, "up-")
}

func TestPruneSupersededBackupsRemovesOnlyFinishedManagedBackups(t *testing.T) {
	ctx := context.Background()
	engine, store, previous, current, backupDir := pruneFixture(t)
	managed := backupDirName(previous.ID)
	require.NoError(t, os.MkdirAll(filepath.Join(backupDir, managed, "postgres"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(backupDir, managed, "postgres", "base"), []byte("x"), 0o600))
	own := backupDirName(current.ID)
	require.NoError(t, os.MkdirAll(filepath.Join(backupDir, own), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(backupDir, "online-20261001T160131Z-0000000000000000000000000000dead"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(backupDir, "upstream-v0214-20261007T093645Z"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(backupDir, "20261001T160131Z"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(backupDir, "online-not-a-managed-name"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(backupDir, "online-20261016T200022Z.tar.gz"), []byte("archive"), 0o600))

	removed, err := engine.pruneSupersededBackups(ctx, store, current)
	require.NoError(t, err)
	require.Equal(t, []string{managed}, removed)
	require.NoDirExists(t, filepath.Join(backupDir, managed))
	require.DirExists(t, filepath.Join(backupDir, own), "current job backup must survive")
	require.DirExists(t, filepath.Join(backupDir, "online-20261001T160131Z-0000000000000000000000000000dead"), "unknown jobs must survive")
	require.DirExists(t, filepath.Join(backupDir, "upstream-v0214-20261007T093645Z"))
	require.DirExists(t, filepath.Join(backupDir, "20261001T160131Z"))
	require.DirExists(t, filepath.Join(backupDir, "online-not-a-managed-name"))
	require.FileExists(t, filepath.Join(backupDir, "online-20261016T200022Z.tar.gz"))
}

func TestPruneSupersededBackupsKeepsActiveJobBackups(t *testing.T) {
	ctx := context.Background()
	engine, store, previous, current, backupDir := pruneFixture(t)
	// The jobs_one_active partial index makes two active jobs unreachable
	// through the public API; reach it directly to cover the defensive guard.
	require.NoError(t, store.Update(ctx, current.ID, Failed, "slot released"))
	_, err := store.db.Exec("UPDATE jobs SET status='preflight' WHERE id=?", previous.ID)
	require.NoError(t, err)
	previousDir := backupDirName(previous.ID)
	require.NoError(t, os.MkdirAll(filepath.Join(backupDir, previousDir), 0o700))

	removed, err := engine.pruneSupersededBackups(ctx, store, current)
	require.NoError(t, err)
	require.Empty(t, removed)
	require.DirExists(t, filepath.Join(backupDir, previousDir), "active job backups must survive")
}

func TestPruneSupersededBackupsKeepsMountedBackups(t *testing.T) {
	ctx := context.Background()
	engine, store, previous, current, backupDir := pruneFixture(t)
	previousDir := backupDirName(previous.ID)
	require.NoError(t, os.MkdirAll(filepath.Join(backupDir, previousDir), 0o700))
	require.NoError(t, store.Update(ctx, current.ID, FailedRolledBack, "rolled back"))
	mountedName := backupDirName(current.ID)
	require.NoError(t, os.MkdirAll(filepath.Join(backupDir, mountedName, "private"), 0o700))
	engine.Runner = mountedRunner(t, []string{filepath.Join(backupDir, mountedName, "private", "compose.yml")})
	request := startRequest()
	request.IdempotencyKey = "third-request"
	fresh, _, err := store.Create(ctx, request, "v0.2.16-1", validManifest())
	require.NoError(t, err)

	removed, err := engine.pruneSupersededBackups(ctx, store, fresh)
	require.NoError(t, err)
	require.Equal(t, []string{previousDir}, removed)
	require.DirExists(t, filepath.Join(backupDir, mountedName), "mounted backups must survive")
}

func TestPruneSupersededBackupsWithoutBackupDirOrDocker(t *testing.T) {
	ctx := context.Background()
	engine, store, _, current, _ := pruneFixture(t)
	engine.Config.BackupDir = filepath.Join(engine.Config.BackupDir, "missing")
	removed, err := engine.pruneSupersededBackups(ctx, store, current)
	require.NoError(t, err)
	require.Empty(t, removed)

	engine.Config.BackupDir = t.TempDir()
	engine.Runner = runnerFunc(func(context.Context, []string, io.Reader, io.Writer) ([]byte, error) {
		return nil, errors.New("docker unavailable")
	})
	_, err = engine.pruneSupersededBackups(ctx, store, current)
	require.Error(t, err)
}
