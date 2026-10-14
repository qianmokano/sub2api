//go:build unit

package kanoagent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The fixture substitutes Docker and the external databases; file copies,
// permissions, durable SQLite checkpoints and recovery use the real code.
type deploymentFixture struct {
	config         Config
	manifest       Manifest
	containers     map[string]Container
	fault          string
	privatePaused  bool
	privateRunning bool
	restored       bool
	productionNew  bool
	requests       int
}

func newDeploymentFixture(t *testing.T, fault string) (*Engine, *Store, *Job, *deploymentFixture) {
	t.Helper()
	f := &deploymentFixture{config: testConfig(t), manifest: validManifest(), containers: map[string]Container{}, fault: fault}
	for _, service := range []string{"sub2api", "postgres", "redis"} {
		data := filepath.Join(f.config.DeploymentDir, service)
		require.NoError(t, os.MkdirAll(data, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(data, "original"), []byte(service+"-original-data"), 0o600))
		c := Container{ID: service, Image: "sha256:" + strings.Repeat("d", 64), Mounts: []Mount{{Type: "bind", Source: data, Destination: "/data", RW: true}}, HostConfig: map[string]any{"NetworkMode": "original-network", "PortBindings": map[string]any{"8080/tcp": "127.0.0.1"}}}
		c.Config.Image = "fixture/" + service
		c.Config.Labels = map[string]string{"org.opencontainers.image.version": "v0.2.16-1", "org.opencontainers.image.revision": strings.Repeat("e", 40)}
		c.Config.Env = []string{"SECRET=original-secret", "POSTGRES_USER=postgres", "POSTGRES_DB=sub2api", "PG_MAJOR=18", "PGDATA=/data/18/docker"}
		c.State.Running = true
		c.State.Health.Status = "healthy"
		if service == "postgres" {
			require.NoError(t, os.MkdirAll(filepath.Join(data, "18", "docker"), 0o700))
		}
		f.containers[service] = c
	}
	for _, path := range f.config.ComposeFiles {
		require.NoError(t, os.WriteFile(path, []byte(`{"services":{}}`), 0o600))
	}
	require.NoError(t, os.WriteFile(filepath.Join(f.config.DeploymentDir, ".env"), []byte("JWT_SECRET=original-secret\n"), 0o600))
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests++
		if f.productionNew && f.fault == "production_health" {
			w.WriteHeader(503)
			return
		}
		if r.URL.Path == "/v1/sub2api/billing" {
			require.Equal(t, "Bearer original-key", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(live.Close)
	f.config.LiveURL = live.URL
	store, err := OpenStore(f.config.StateDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	job, _, err := store.Create(context.Background(), startRequest(), "v0.2.16-1", f.manifest)
	require.NoError(t, err)
	engine := &Engine{Config: f.config, Runner: f, Source: &fakeSource{release: &Release{Tag: f.manifest.Version, Manifest: &f.manifest}}, Verifier: f, Redis: f}
	return engine, store, job, f
}

func (f *deploymentFixture) Prepare(context.Context, Manifest) error {
	if f.fault == "image" {
		return errors.New("image unavailable")
	}
	return nil
}
func (f *deploymentFixture) Snapshot(_ context.Context, c, identity Container) (RedisSnapshot, error) {
	value := RedisSnapshot{{"session": {Type: "string", Digest: "original-session", Expires: -1}}}
	if strings.Contains(c.ID, "kano-verify-") && f.fault == "redis" {
		value[0]["session"] = RedisEntry{Type: "string", Digest: "corrupt", Expires: -1}
	}
	return value, nil
}
func (f *deploymentFixture) Save(context.Context, Container) error { return nil }

func (f *deploymentFixture) Run(ctx context.Context, args []string, input io.Reader, output io.Writer) ([]byte, error) {
	if args[0] != "docker" {
		return (CommandRunner{}).Run(ctx, args, input, output)
	}
	if args[1] == "inspect" {
		name := args[2]
		c, ok := f.containers[name]
		if !ok {
			service := name[strings.LastIndex(name, "-")+1:]
			c = f.containers[service]
			c.ID = name
			c.State.Running = f.privateRunning
		}
		body, _ := json.Marshal([]Container{c})
		return body, nil
	}
	if args[1] == "ps" {
		if f.privateRunning {
			return []byte("private-container"), nil
		}
		return nil, nil
	}
	if args[1] == "compose" {
		private := false
		project := ""
		composeFile := ""
		index := 2
		for index < len(args) {
			switch args[index] {
			case "--project-directory":
				index += 2
			case "-p":
				private = true
				project = args[index+1]
				index += 2
			case "-f":
				composeFile = args[index+1]
				index += 2
			default:
				goto action
			}
		}
	action:
		parts := args[index:]
		action := parts[0]
		if action == "config" {
			body, _ := json.Marshal(map[string]any{"services": map[string]any{"sub2api": map[string]any{}, "postgres": map[string]any{}, "redis": map[string]any{}}})
			return body, nil
		}
		if action == "ps" {
			return []byte(parts[len(parts)-1]), nil
		}
		if private {
			if action == "up" {
				f.privateRunning = true
				if parts[len(parts)-1] == "sub2api" && f.fault == "migration" {
					return nil, errors.New("migration failed")
				}
			}
			if action == "down" {
				f.privateRunning = false
			}
			_ = project
			if action == "up" && f.fault == "corrupt_backup" {
				path := filepath.Join(filepath.Dir(composeFile), "postgres.dump")
				_ = os.WriteFile(path, []byte("damaged"), 0o600)
				f.fault = ""
			}
			return nil, nil
		}
		selected := []string{"sub2api", "postgres", "redis"}
		if parts[len(parts)-1] == "sub2api" {
			selected = []string{"sub2api"}
		} else if parts[len(parts)-1] == "redis" && len(parts) > 2 {
			selected = []string{"postgres", "redis"}
		}
		for _, service := range selected {
			c := f.containers[service]
			c.State.Running = action != "stop"
			f.containers[service] = c
		}
		if action == "up" {
			body, _ := os.ReadFile(f.config.ImageOverride)
			f.productionNew = strings.Contains(string(body), f.manifest.Version)
			c := f.containers["sub2api"]
			if f.productionNew {
				c.Config.Labels = map[string]string{"org.opencontainers.image.version": f.manifest.Version, "org.opencontainers.image.revision": f.manifest.Revision}
			} else {
				c.Config.Labels = map[string]string{"org.opencontainers.image.version": "v0.2.16-1", "org.opencontainers.image.revision": strings.Repeat("e", 40)}
			}
			f.containers["sub2api"] = c
			if f.productionNew {
				_ = os.WriteFile(filepath.Join(f.config.DeploymentDir, "sub2api", "original"), []byte("new-version-data"), 0o600)
				if f.fault == "interrupt" {
					f.fault = ""
					panic("simulated process termination")
				}
				if f.fault == "switch" {
					f.fault = ""
					return nil, errors.New("switch failed")
				}
				if f.fault == "rollback_corrupt" {
					entries, _ := os.ReadDir(f.config.BackupDir)
					_ = os.WriteFile(filepath.Join(f.config.BackupDir, entries[0].Name(), "postgres.dump"), []byte("damaged"), 0o600)
					f.fault = "production_data"
				}
			}
		}
		return nil, nil
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, " pg_dump ") || strings.Contains(joined, " pg_dumpall ") {
		if f.fault == "dump" {
			return nil, errors.New("dump failed")
		}
		_, err := io.WriteString(output, "complete logical backup")
		return nil, err
	}
	if strings.Contains(joined, " pg_restore ") {
		if f.fault == "restore" {
			return nil, errors.New("restore failed")
		}
		body, err := io.ReadAll(input)
		if err != nil || string(body) != "complete logical backup" {
			return nil, errors.New("invalid dump")
		}
		f.restored = true
		return nil, nil
	}
	if strings.Contains(joined, " wget ") {
		return []byte(`{"status":"ok"}`), nil
	}
	query := args[len(args)-1]
	private := strings.Contains(joined, "kano-verify-")
	switch {
	case strings.Contains(query, "pg_database_size"):
		if f.fault == "disk" {
			return []byte("9223372036854775807"), nil
		}
		return []byte("1024"), nil
	case strings.Contains(query, "pg_table_size"):
		return []byte("512"), nil
	case strings.Contains(query, "FROM pg_tables"):
		return []byte(`["users","settings","schema_migrations"]`), nil
	case strings.Contains(query, "information_schema.columns"):
		return []byte(`[{"name":"id","type":"int8","nullable":false,"default":""}]`), nil
	case strings.Contains(query, "md5(row_to_json"):
		if strings.Contains(query, `public."users"`) && ((private && f.fault == "private_data") || (!private && f.productionNew && f.fault == "production_data")) {
			return []byte("1|5|6"), nil
		}
		return []byte("1|2|3"), nil
	case strings.Contains(query, "jsonb_object_agg(filename"):
		body, _ := json.Marshal(f.manifest.Migrations)
		return body, nil
	case strings.Contains(query, "jsonb_object_agg(key"):
		settings := map[string]string{"sso_enabled": "true", "default_balance": "0"}
		if private && f.privatePaused {
			settings, _ = pausedSettings(settings)
		}
		body, _ := json.Marshal(settings)
		return body, nil
	case strings.HasPrefix(query, "INSERT INTO settings"):
		f.privatePaused = true
		return nil, nil
	case strings.Contains(query, "FROM api_keys"):
		return []byte("original-key"), nil
	default:
		return []byte("1"), nil
	}
}

func TestExecuteFullBackupRestoreAndMigration(t *testing.T) {
	e, store, job, f := newDeploymentFixture(t, "")
	require.NoError(t, e.Execute(context.Background(), store, job))
	latest, err := store.Get(context.Background(), job.ID)
	require.NoError(t, err)
	require.Equal(t, Succeeded, latest.Status)
	var state UpgradeState
	require.NoError(t, json.Unmarshal(latest.State, &state))
	require.True(t, state.BackupComplete)
	require.True(t, state.PublicCommitted)
	require.True(t, f.restored)
	require.True(t, f.productionNew)
	require.NoError(t, e.verifyBackup(&state))
	require.NoFileExists(t, e.Config.MaintenanceFile)
	require.FileExists(t, filepath.Join(state.BackupPath, "final-evidence.json"))
	require.NoDirExists(t, filepath.Join(state.BackupPath, "private-postgres"))
	body, err := os.ReadFile(filepath.Join(e.Config.DeploymentDir, ".env"))
	require.NoError(t, err)
	require.Equal(t, "JWT_SECRET=original-secret\n", string(body))
}

func TestExecuteFaultsKeepOrRestoreOriginalDeployment(t *testing.T) {
	for _, fault := range []string{"image", "disk", "dump", "restore", "redis", "private_data", "migration", "corrupt_backup", "switch", "production_data", "rollback_corrupt"} {
		t.Run(fault, func(t *testing.T) {
			e, store, job, f := newDeploymentFixture(t, fault)
			require.Error(t, e.Execute(context.Background(), store, job))
			latest, err := store.Get(context.Background(), job.ID)
			require.NoError(t, err)
			if fault == "image" || fault == "disk" {
				require.Equal(t, Failed, latest.Status)
			} else if fault == "rollback_corrupt" {
				require.Equal(t, NeedsAttention, latest.Status)
				require.FileExists(t, e.Config.MaintenanceFile)
				return
			} else {
				require.Equal(t, FailedRolledBack, latest.Status)
			}
			require.NoFileExists(t, e.Config.MaintenanceFile)
			require.True(t, f.containers["sub2api"].State.Running)
			require.False(t, f.productionNew)
			body, err := os.ReadFile(filepath.Join(e.Config.DeploymentDir, "sub2api", "original"))
			require.NoError(t, err)
			require.Equal(t, "sub2api-original-data", string(body))
		})
	}
}

func TestExecuteResumesDurableInterruptedRecovery(t *testing.T) {
	e, store, job, f := newDeploymentFixture(t, "interrupt")
	require.Panics(t, func() { _ = e.Execute(context.Background(), store, job) })
	require.True(t, f.productionNew)
	require.FileExists(t, e.Config.MaintenanceFile)
	persisted, err := store.Get(context.Background(), job.ID)
	require.NoError(t, err)
	var state UpgradeState
	require.NoError(t, json.Unmarshal(persisted.State, &state))
	require.True(t, state.ProductionTouched)
	require.False(t, state.PublicCommitted)
	require.NoError(t, e.Execute(context.Background(), store, persisted))
	latest, err := store.Get(context.Background(), job.ID)
	require.NoError(t, err)
	require.Equal(t, FailedRolledBack, latest.Status)
	body, err := os.ReadFile(filepath.Join(e.Config.DeploymentDir, "sub2api", "original"))
	require.NoError(t, err)
	require.Equal(t, "sub2api-original-data", string(body))
}

func TestCommittedRecoveryNeverRestoresOlderDatabase(t *testing.T) {
	e, store, job, _ := newDeploymentFixture(t, "")
	require.NoError(t, e.Execute(context.Background(), store, job))
	path := filepath.Join(e.Config.DeploymentDir, "sub2api", "original")
	require.NoError(t, os.WriteFile(path, []byte("new customer writes"), 0o600))
	require.NoError(t, store.Update(context.Background(), job.ID, Checking, "interrupted finalization"))
	persisted, err := store.Get(context.Background(), job.ID)
	require.NoError(t, err)
	require.NoError(t, e.Execute(context.Background(), store, persisted))
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "new customer writes", string(body))
}

func TestHealthRejectsRedirectAndCancelledReadiness(t *testing.T) {
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "https://untrusted.test", 302) }))
	defer redirect.Close()
	e := &Engine{Config: Config{LiveURL: redirect.URL}}
	require.Error(t, e.billing(context.Background(), "private-key"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.Error(t, e.waitHealth(ctx))
	require.Error(t, e.waitPostgres(ctx, Container{}))
	require.NoError(t, e.billing(ctx, ""))
	require.False(t, validHealth([]byte(`{"status":"error"}`)))
	require.False(t, validHealth([]byte(`invalid`)))
}
