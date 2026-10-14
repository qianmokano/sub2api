//go:build unit

package kanoagent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestImageVerifierRequiresPublishedDigestSourceAndBothArchitectures(t *testing.T) {
	manifest := validManifest()
	for _, fault := range []string{"", "pull", "invalid_image", "revision", "architecture", "digest", "invalid_index", "single_arch"} {
		t.Run(fault, func(t *testing.T) {
			engine := &Engine{Runner: runnerFunc(func(_ context.Context, args []string, _ io.Reader, _ io.Writer) ([]byte, error) {
				switch args[1] {
				case "pull":
					if fault == "pull" {
						return nil, errors.New("registry unavailable")
					}
					return nil, nil
				case "image":
					if fault == "invalid_image" {
						return []byte(`[]`), nil
					}
					labels := map[string]string{"org.opencontainers.image.source": "https://github.com/" + Repository, "org.opencontainers.image.version": manifest.Version, "org.opencontainers.image.revision": manifest.Revision}
					arch := runtime.GOARCH
					digest := manifest.ImageDigest
					if fault == "revision" {
						labels["org.opencontainers.image.revision"] = "wrong"
					}
					if fault == "architecture" {
						arch = "wrong"
					}
					if fault == "digest" {
						digest = "wrong"
					}
					body, _ := json.Marshal([]any{map[string]any{"Architecture": arch, "RepoDigests": []string{ImageRepository + "@" + digest}, "Config": map[string]any{"Labels": labels}}})
					return body, nil
				default:
					if fault == "invalid_index" {
						return []byte("invalid"), nil
					}
					arches := []string{"amd64", "arm64"}
					if fault == "single_arch" {
						arches = arches[:1]
					}
					entries := []any{}
					for _, arch := range arches {
						entries = append(entries, map[string]any{"Digest": manifest.ImageDigest, "Platform": map[string]any{"OS": "linux", "Architecture": arch}})
					}
					body, _ := json.Marshal(map[string]any{"manifests": entries})
					return body, nil
				}
			})}
			if fault == "" {
				require.NoError(t, engine.Prepare(context.Background(), manifest))
			} else {
				require.Error(t, engine.Prepare(context.Background(), manifest))
			}
		})
	}
}

func TestRuntimeRejectsSymlinkAndGuardsRecoveryPaths(t *testing.T) {
	e, _, job, f := newDeploymentFixture(t, "")
	old := f.containers["sub2api"]
	mount := old.Mounts[0]
	link := filepath.Join(f.config.DeploymentDir, "linked")
	require.NoError(t, os.Symlink(mount.Source, link))
	old.Mounts = []Mount{{Source: link, Destination: "/data", RW: true}}
	f.containers["sub2api"] = old
	_, err := e.snapshotRuntime(context.Background())
	require.ErrorContains(t, err, "symlinked")
	state := UpgradeState{Snapshot: &RuntimeSnapshot{}, BackupPath: filepath.Join(e.Config.BackupDir, "test"), PrivateProject: "kano-verify-" + strings.TrimPrefix(job.ID, "up-")}
	state.PrivateCompose = filepath.Join(state.BackupPath, "private.json")
	require.NoError(t, e.validateCheckpoint(job, &state))
	state.Snapshot.Roots = []SavedRoot{{Saved: "/outside"}}
	require.Error(t, e.validateCheckpoint(job, &state))
	require.Error(t, e.cleanup(context.Background(), &UpgradeState{BackupPath: e.Config.BackupDir}))
	require.Error(t, e.verifyBackup(&UpgradeState{}))
	require.Error(t, syncTree(filepath.Join(t.TempDir(), "missing")))
	require.Error(t, writePrivateJSON(filepath.Join(t.TempDir(), "invalid"), make(chan int)))
	before := originalDatabase()
	after := copyDatabase(before)
	after.Migrations["241_sso_issuer_independence.sql"] = "known-historical-checksum"
	before.Migrations["241_sso_issuer_independence.sql"] = "known-historical-checksum"
	require.NoError(t, CompareDatabase(before, after, validManifest(), false))
}

func TestRuntimeVerificationRejectsChangedCredentialsMountsAndDependencies(t *testing.T) {
	for _, fault := range []string{"environment", "command", "mount", "port", "dependency", "stopped", "version"} {
		t.Run(fault, func(t *testing.T) {
			e, _, _, f := newDeploymentFixture(t, "")
			snapshot, err := e.snapshotRuntime(context.Background())
			require.NoError(t, err)
			body, err := json.Marshal(f.containers["sub2api"])
			require.NoError(t, err)
			var app Container
			require.NoError(t, json.Unmarshal(body, &app))
			manifest := validManifest()
			app.Config.Labels = map[string]string{"org.opencontainers.image.version": manifest.Version, "org.opencontainers.image.revision": manifest.Revision}
			switch fault {
			case "environment":
				app.Config.Env = append(app.Config.Env, "JWT_SECRET=changed")
			case "command":
				app.Config.Cmd = []string{"different-command"}
			case "mount":
				app.Mounts[0].Source = "/another-data-directory"
			case "port":
				app.HostConfig["PortBindings"] = map[string]any{"8080/tcp": "0.0.0.0"}
			case "dependency":
				postgres := f.containers["postgres"]
				postgres.ID = "replaced-dependency"
				f.containers["postgres"] = postgres
			case "stopped":
				app.State.Running = false
			case "version":
				app.Config.Labels["org.opencontainers.image.version"] = "v0.2.16-1"
			}
			f.containers["sub2api"] = app
			require.Error(t, e.verifyRuntime(context.Background(), &UpgradeState{Snapshot: snapshot}, manifest))
		})
	}
}
