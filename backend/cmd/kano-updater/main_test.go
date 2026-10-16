//go:build unit

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/kanoagent"
	"github.com/Wei-Shaw/sub2api/internal/kanoupdate"
	"github.com/stretchr/testify/require"
)

func TestCLIManifestAndVersion(t *testing.T) {
	var output bytes.Buffer
	require.NoError(t, run(context.Background(), []string{"version"}, &output))
	require.Equal(t, kanoupdate.AgentVersion+"\n", output.String())
	root := t.TempDir()
	policy := filepath.Join(root, "policy.json")
	require.NoError(t, os.WriteFile(policy, []byte(`{"minimum_source_version":"v0.2.16-1"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "001.sql"), []byte("SELECT 1;"), 0o600))
	output.Reset()
	args := []string{"manifest", "--policy", policy, "--migrations", root, "--version", "v0.2.16-2", "--digest", "sha256:" + strings.Repeat("a", 64), "--revision", strings.Repeat("b", 40)}
	require.NoError(t, run(context.Background(), args, &output))
	var manifest kanoupdate.Manifest
	require.NoError(t, json.Unmarshal(output.Bytes(), &manifest))
	require.NoError(t, manifest.Validate())
	require.NoError(t, os.WriteFile(policy, []byte(`{"unexpected":true}`), 0o600))
	require.Error(t, run(context.Background(), args, &output))
	for _, arguments := range [][]string{nil, {"invalid"}, {"manifest", "--unknown"}, {"manifest", "positional"}, {"manifest", "--policy", "/missing-policy"}} {
		require.Error(t, run(context.Background(), arguments, &output))
	}
}

func TestCLIRejectsUnprivilegedHostCommands(t *testing.T) {
	if runtime.GOOS == "linux" && os.Geteuid() == 0 {
		t.Skip("non-root permission boundary is exercised by the ordinary unit run")
	}
	for _, command := range []string{"serve", "start", "schedule", "cancel-schedule", "recover", "install"} {
		require.ErrorContains(t, run(context.Background(), []string{command}, io.Discard), "Linux root")
	}
}

func TestCLIHostReservationAndCancellation(t *testing.T) {
	config, path := cliHostConfig(t)
	listener, err := kanoagent.ListenUnix(config.ControlSocket, config.ControlGID)
	require.NoError(t, err)
	id := "up-" + strings.Repeat("a", 32)
	at := kanoupdate.NextSchedule(time.Now())
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			require.Equal(t, "/schedule/"+id, r.URL.Path)
			_, _ = io.WriteString(w, `{"cancelled":true}`)
			return
		}
		require.Equal(t, "/schedule", r.URL.Path)
		var request kanoupdate.StartRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		require.Equal(t, "specific-reservation", request.IdempotencyKey)
		require.Equal(t, "host-bootstrap", request.Actor)
		_ = json.NewEncoder(w).Encode(kanoupdate.Session{Job: &kanoupdate.Job{ID: id, ScheduledAt: &at}, Capability: "private-capability", ProgressURL: "/kano-updates/" + id})
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	var output bytes.Buffer
	require.ErrorContains(t, run(context.Background(), []string{"schedule", "--config", path}, &output), "request-key")
	require.NoError(t, run(context.Background(), []string{"schedule", "--config", path, "--version", "v0.2.16-2", "--digest", "sha256:" + strings.Repeat("b", 64), "--request-key", "specific-reservation"}, &output))
	require.Contains(t, output.String(), at.Format(time.RFC3339))
	require.NotContains(t, output.String(), "private-capability")
	require.NoError(t, run(context.Background(), []string{"cancel-schedule", "--config", path, "--id", id}, io.Discard))
}

func cliHostConfig(t *testing.T) (kanoagent.Config, string) {
	t.Helper()
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("run native host lifecycle unit tests as Linux root")
	}
	root, err := os.MkdirTemp("/tmp", "ku-cli-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })
	deployment := filepath.Join(root, "app")
	config := kanoagent.Config{
		DeploymentDir: deployment,
		ComposeFiles:  []string{filepath.Join(deployment, "compose.json"), filepath.Join(deployment, "image.json")},
		ImageOverride: filepath.Join(deployment, "image.json"), BackupDir: filepath.Join(root, "backups"),
		StateDir: filepath.Join(root, "state"), MaintenanceFile: filepath.Join(root, "public", "maintenance"),
		ControlSocket: filepath.Join(root, "control.sock"), ProgressSocket: filepath.Join(root, "progress.sock"),
		ControlGID: os.Getgid(), ProgressGID: os.Getgid(), LiveURL: "http://127.0.0.1:1",
	}
	require.NoError(t, config.Validate())
	body, err := json.Marshal(config)
	require.NoError(t, err)
	path := filepath.Join(root, "config.json")
	require.NoError(t, os.WriteFile(path, body, 0o600))
	return config, path
}

func TestCLIHostServeAndCleanShutdown(t *testing.T) {
	config, path := cliHostConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"serve", "--config", path}, io.Discard) }()
	client := kanoupdate.NewClient(config.ControlSocket)
	require.Eventually(t, func() bool {
		availability, err := client.Health(context.Background())
		return err == nil && availability.Ready && !availability.Busy
	}, 5*time.Second, 20*time.Millisecond)
	require.ErrorIs(t, run(context.Background(), []string{"recover", "--config", path}, io.Discard), syscall.EWOULDBLOCK)
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("native updater did not shut down after cancellation")
	}
	_, err := os.Stat(config.ControlSocket)
	require.ErrorIs(t, err, os.ErrNotExist)
	lock, err := kanoagent.AcquireHostLock(filepath.Join(config.StateDir, "host.lock"))
	require.NoError(t, err)
	require.NoError(t, lock.Close())
}

func TestCLIHostStartDoesNotPrintProgressCredential(t *testing.T) {
	config, path := cliHostConfig(t)
	listener, err := kanoagent.ListenUnix(config.ControlSocket, config.ControlGID)
	require.NoError(t, err)
	requests := make(chan kanoupdate.StartRequest, 1)
	secret := strings.Repeat("c", 64)
	id := "up-" + strings.Repeat("a", 32)
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request kanoupdate.StartRequest
		if r.Method != "POST" || r.URL.Path != "/jobs" || json.NewDecoder(r.Body).Decode(&request) != nil {
			w.WriteHeader(400)
			return
		}
		requests <- request
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(kanoupdate.Session{Job: &kanoupdate.Job{ID: id}, ProgressURL: "/kano-updates/" + id, Capability: secret})
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	var output bytes.Buffer
	digest := "sha256:" + strings.Repeat("b", 64)
	require.NoError(t, run(context.Background(), []string{"start", "--config", path, "--version", "v0.2.16-2", "--digest", digest}, &output))
	request := <-requests
	require.Equal(t, "host-bootstrap", request.Actor)
	require.Equal(t, "bootstrap:v0.2.16-2", request.IdempotencyKey)
	require.Equal(t, digest, request.ExpectedDigest)
	require.NotContains(t, output.String(), secret)
	require.JSONEq(t, `{"operation_id":"`+id+`","progress_url":"/kano-updates/`+id+`"}`, output.String())
}

func TestCLIHostRejectsInvalidStateAndOccupiedSocketPaths(t *testing.T) {
	for _, socket := range []string{"control", "progress"} {
		t.Run(socket, func(t *testing.T) {
			config, path := cliHostConfig(t)
			target := config.ControlSocket
			if socket == "progress" {
				target = config.ProgressSocket
			}
			require.NoError(t, os.WriteFile(target, []byte("preserve-existing-file"), 0o600))
			require.ErrorContains(t, run(context.Background(), []string{"serve", "--config", path}, io.Discard), "non-socket")
			body, err := os.ReadFile(target)
			require.NoError(t, err)
			require.Equal(t, "preserve-existing-file", string(body))
		})
	}
	config, path := cliHostConfig(t)
	require.Error(t, run(context.Background(), []string{"serve", "--config", path + ".missing"}, io.Discard))
	require.NoError(t, os.MkdirAll(config.StateDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(config.StateDir, "jobs.sqlite"), []byte("invalid-sqlite-state"), 0o600))
	require.Error(t, run(context.Background(), []string{"serve", "--config", path}, io.Discard))
}

func TestCLIHostRecoveryAndInstallRespectActiveJob(t *testing.T) {
	config, path := cliHostConfig(t)
	require.ErrorIs(t, run(context.Background(), []string{"recover", "--config", path}, io.Discard), kanoagent.ErrNotFound)
	manifest, err := kanoupdate.GenerateManifest(fstest.MapFS{"001.sql": {Data: []byte("SELECT 1;")}}, kanoupdate.ValidationPolicy{MinimumSourceVersion: "v0.2.16-1"}, "v0.2.16-2", "sha256:"+strings.Repeat("a", 64), strings.Repeat("b", 40))
	require.NoError(t, err)
	store, err := kanoagent.OpenStore(config.StateDir)
	require.NoError(t, err)
	job, _, err := store.Create(context.Background(), kanoupdate.StartRequest{Actor: "admin:1", IdempotencyKey: "existing-job", Version: manifest.Version, ExpectedDigest: manifest.ImageDigest}, "v0.2.16-1", manifest)
	require.NoError(t, err)
	require.NoError(t, store.Checkpoint(context.Background(), job.ID, "invalid-recovery-state", ""))
	require.NoError(t, store.Close())
	require.ErrorIs(t, run(context.Background(), []string{"install", "--config", path}, io.Discard), kanoagent.ErrBusy)
	require.ErrorContains(t, run(context.Background(), []string{"recover", "--config", path}, io.Discard), "recovery checkpoint is invalid")
	store, err = kanoagent.OpenStore(config.StateDir)
	require.NoError(t, err)
	require.NoError(t, store.Update(context.Background(), job.ID, kanoagent.Succeeded, "Done"))
	require.NoError(t, store.Close())
	require.ErrorContains(t, run(context.Background(), []string{"recover", "--config", path}, io.Discard), "no incomplete upgrade")
}
