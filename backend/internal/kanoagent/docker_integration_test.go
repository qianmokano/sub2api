//go:build integration

package kanoagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/kanoupdate"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

const integrationTarget = "v0.2.16-2"
const integrationRevision = "1111111111111111111111111111111111111111"
const integrationAdminEmail = "updater-fixture@example.test"
const integrationAdminPassword = "Fixture-Only-Password-2026"

type integrationSource struct{ release *Release }

func (s integrationSource) Latest(context.Context) (*Release, error)          { return s.release, nil }
func (s integrationSource) Resolve(context.Context, string) (*Release, error) { return s.release, nil }

func integrationManifest(t *testing.T) Manifest {
	t.Helper()
	policy := ValidationPolicy{MinimumSourceVersion: "v0.2.16-1"}
	if path := os.Getenv("KANO_TEST_POLICY"); path != "" {
		body, err := os.ReadFile(path)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(body, &policy))
	}
	m, err := kanoupdate.GenerateManifest(migrations.FS, policy, integrationTarget, "sha256:"+strings.Repeat("a", 64), integrationRevision)
	require.NoError(t, err)
	return m
}

// A local image resolver replaces only this fixture's unpublished target digest.
// Release/index/tag validation is exercised by unit tests and the production
// bootstrap. Everything after this boundary is real Docker, PostgreSQL and Redis.
type integrationRunner struct {
	config          Config
	fault           string
	newHealth       atomic.Bool
	cancelOperation context.CancelFunc
	command         CommandRunner
}

func (r *integrationRunner) Prepare(ctx context.Context, manifest Manifest) error {
	body, err := r.command.Run(ctx, []string{"docker", "image", "inspect", "kano-sub2api-test:target", "--format", "{{json .Config.Labels}}"}, nil, nil)
	if err != nil {
		return err
	}
	var labels map[string]string
	if json.Unmarshal(body, &labels) != nil || labels["org.opencontainers.image.version"] != manifest.Version || labels["org.opencontainers.image.revision"] != manifest.Revision {
		return errors.New("local fixture image differs from its manifest")
	}
	return nil
}

func (r *integrationRunner) Run(ctx context.Context, args []string, input io.Reader, output io.Writer) ([]byte, error) {
	if r.fault == "disk" && strings.Contains(strings.Join(args, " "), "pg_database_size") {
		return []byte("9223372036854775807"), nil
	}
	if r.fault == "restore" && strings.Contains(strings.Join(args, " "), " pg_restore ") {
		return nil, errors.New("injected logical restore failure")
	}
	resolved := append([]string(nil), args...)
	private := false
	project := ""
	if len(args) > 2 && args[0] == "docker" && args[1] == "compose" {
		for index := 2; index < len(args)-1; index++ {
			if args[index] == "-p" {
				private = true
				project = args[index+1]
			}
			if args[index] != "-f" {
				continue
			}
			body, err := os.ReadFile(args[index+1])
			if err != nil {
				return nil, err
			}
			from := ImageRepository + ":" + integrationTarget + "@sha256:" + strings.Repeat("a", 64)
			if !bytes.Contains(body, []byte(from)) {
				continue
			}
			file, err := os.CreateTemp(filepath.Dir(args[index+1]), "test-image-*.json")
			if err != nil {
				return nil, err
			}
			defer func() { _ = os.Remove(file.Name()) }()
			if _, err = file.Write(bytes.ReplaceAll(body, []byte(from), []byte("kano-sub2api-test:target"))); err != nil {
				_ = file.Close()
				return nil, err
			}
			if err = file.Close(); err != nil {
				return nil, err
			}
			resolved[index+1] = file.Name()
		}
	}
	harness := os.Getenv("KANO_TEST_HARNESS_CONTAINER")
	if private && strings.Contains(strings.Join(args, " "), " down ") && harness != "" {
		_, _ = r.command.Run(ctx, []string{"docker", "network", "disconnect", project + "_isolated", harness}, nil, nil)
	}
	body, err := r.command.Run(ctx, resolved, input, output)
	if err != nil {
		return body, err
	}
	if private && strings.Contains(strings.Join(args, " "), " up -d postgres redis") && harness != "" {
		if _, connectErr := r.command.Run(ctx, []string{"docker", "network", "connect", project + "_isolated", harness}, nil, nil); connectErr != nil {
			return nil, connectErr
		}
	}
	if args[0] == "cp" && r.fault == "backup_corrupt" && strings.Contains(args[len(args)-1], "/original") {
		_ = os.WriteFile(filepath.Join(args[len(args)-1], ".env"), []byte("corrupted backup"), 0o600)
	}
	joined := strings.Join(args, " ")
	if private && strings.Contains(joined, " stop --timeout 60 sub2api") && r.fault == "migration" {
		_, err = r.command.Run(ctx, []string{"docker", "exec", project + "-postgres", "psql", "-U", "sub2api", "-d", "sub2api", "-c", "UPDATE users SET balance=999 WHERE id=1"}, nil, nil)
		if err != nil {
			return nil, err
		}
	}
	if !private && strings.Contains(joined, " up -d --no-deps sub2api") {
		override, _ := os.ReadFile(r.config.ImageOverride)
		isTarget := bytes.Contains(override, []byte(integrationTarget))
		r.newHealth.Store(isTarget && r.fault == "health")
		if isTarget && r.fault == "service_restart" {
			os.Exit(87)
		}
		if isTarget && r.fault == "health" {
			time.AfterFunc(time.Second, r.cancelOperation)
		}
		if isTarget && r.fault == "background_startup" {
			// Give an ungated metrics collector time to write before the frozen
			// production comparison. The corrected application must stay paused.
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(5 * time.Second):
			}
		}
	}
	if !private && strings.Contains(joined, " up -d") && !strings.Contains(joined, "--no-deps") {
		r.newHealth.Store(false)
	}
	return body, nil
}

type integrationExecutor struct {
	engine *Engine
	runner *integrationRunner
}

func (e *integrationExecutor) CurrentVersion(ctx context.Context) (string, error) {
	return e.engine.CurrentVersion(ctx)
}
func (e *integrationExecutor) Execute(ctx context.Context, store *Store, job *Job) error {
	operation, cancel := context.WithCancel(ctx)
	defer cancel()
	e.runner.cancelOperation = cancel
	return e.engine.Execute(operation, store, job)
}

// Run in a child process so the restart scenario really exits without defers,
// releases the kernel lock and resumes from the same protected SQLite file.
func TestKanoUpdaterDockerWorker(t *testing.T) {
	if os.Getenv("KANO_UPDATER_WORKER") != "1" {
		t.Skip("worker process only")
	}
	config, err := LoadConfig(os.Getenv("KANO_UPDATER_CONFIG"))
	require.NoError(t, err)
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	lock, err := AcquireHostLock(filepath.Join(config.StateDir, "host.lock"))
	require.NoError(t, err)
	defer func() { _ = lock.Close() }()
	store, err := OpenStore(config.StateDir)
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	runner := &integrationRunner{config: config, fault: os.Getenv("KANO_UPDATER_FAULT"), command: CommandRunner{DiagnosticDir: filepath.Join(config.StateDir, "diagnostics")}}
	manifest := integrationManifest(t)
	source := integrationSource{release: &Release{Tag: manifest.Version, Name: "Isolated fixture release", Manifest: &manifest}}
	engine := &Engine{Config: config, Runner: runner, Source: source, Verifier: runner}
	upstream, err := url.Parse("http://127.0.0.1:19081")
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	var addressMu sync.Mutex
	address := "127.0.0.1:19081"
	var addressAt time.Time
	proxy.Director = func(request *http.Request) {
		// Concurrent frontend chunks must not launch a Docker CLI per request.
		// Refresh the short-lived address after container recreation instead.
		addressMu.Lock()
		if time.Since(addressAt) >= time.Second {
			container, inspectErr := engine.container(request.Context(), "sub2api")
			if inspectErr == nil {
				for _, network := range sortedKeys(container.NetworkSettings.Networks) {
					if ip := container.NetworkSettings.Networks[network].IPAddress; ip != "" {
						address = net.JoinHostPort(ip, "8080")
						break
					}
				}
			}
			addressAt = time.Now()
		}
		request.URL.Scheme = "http"
		request.URL.Host = address
		addressMu.Unlock()
	}
	proxyServer := &http.Server{Addr: "127.0.0.1:19082", ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if runner.newHealth.Load() {
			w.WriteHeader(503)
			return
		}
		proxy.ServeHTTP(w, r)
	})}
	go func() { _ = proxyServer.ListenAndServe() }()
	defer func() { _ = proxyServer.Close() }()
	server := NewServer(store, source, &integrationExecutor{engine: engine, runner: runner})
	control, err := ListenUnix(config.ControlSocket, config.ControlGID)
	require.NoError(t, err)
	defer func() { _ = control.Close() }()
	progress, err := ListenUnix(config.ProgressSocket, config.ProgressGID)
	require.NoError(t, err)
	defer func() { _ = progress.Close() }()
	controlHTTP := &http.Server{Handler: server.ControlHandler(), ReadHeaderTimeout: 5 * time.Second}
	progressHTTP := &http.Server{Handler: server.ProgressHandler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = controlHTTP.Serve(control) }()
	go func() { _ = progressHTTP.Serve(progress) }()
	defer func() { _ = controlHTTP.Close(); _ = progressHTTP.Close() }()
	server.Run(ctx)
}

func startDockerWorker(t *testing.T, configPath, fault string) *exec.Cmd {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestKanoUpdaterDockerWorker$", "-test.timeout=20m")
	command.Env = append(os.Environ(), "KANO_UPDATER_WORKER=1", "KANO_UPDATER_CONFIG="+configPath, "KANO_UPDATER_FAULT="+fault)
	log, err := os.OpenFile(filepath.Join(filepath.Dir(configPath), "worker-"+fault+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	command.Stdout = log
	command.Stderr = log
	require.NoError(t, command.Start())
	require.NoError(t, log.Close())
	t.Cleanup(func() {
		if t.Failed() {
			body, readErr := os.ReadFile(log.Name())
			if readErr == nil {
				t.Logf("fixture worker diagnostic: %s", body)
			}
		}
	})
	return command
}

func stopDockerWorker(command *exec.Cmd) {
	if command == nil || command.Process == nil {
		return
	}
	_ = command.Process.Signal(syscall.SIGTERM)
	_ = command.Wait()
}

func dockerFixture(t *testing.T) (Config, string, *Engine) {
	return dockerFixtureEnvironment(t, nil)
}

func dockerFixtureEnvironment(t *testing.T, overrides map[string]string) (Config, string, *Engine) {
	t.Helper()
	parent := os.Getenv("KANO_DOCKER_FIXTURE_ROOT")
	require.True(t, filepath.IsAbs(parent))
	root, err := os.MkdirTemp(parent, "kano-updater-it-")
	require.NoError(t, err)
	require.NoError(t, os.Chmod(root, 0o755))
	deployment := filepath.Join(root, "deployment")
	require.NoError(t, os.MkdirAll(deployment, 0o755))
	config := Config{DeploymentDir: deployment, ComposeFiles: []string{filepath.Join(deployment, "compose.json"), filepath.Join(deployment, "image.json")}, ImageOverride: filepath.Join(deployment, "image.json"), BackupDir: filepath.Join(root, "backups"), StateDir: filepath.Join(root, "state"), MaintenanceFile: filepath.Join(root, "public", "maintenance"), ControlSocket: filepath.Join(root, "control", "agent.sock"), ProgressSocket: filepath.Join(root, "progress", "agent.sock"), ControlGID: 1000, ProgressGID: 101, LiveURL: "http://127.0.0.1:19082"}
	require.NoError(t, config.Validate())
	require.NoError(t, writePrivateJSON(config.ImageOverride, map[string]any{"services": map[string]any{}}))
	configPath := filepath.Join(root, "config.json")
	require.NoError(t, writePrivateJSON(configPath, config))
	require.NoError(t, atomicFile(filepath.Join(deployment, ".env"), []byte("FIXTURE_ONLY=true\n"), 0o600))
	for _, directory := range []string{"app", "postgres", "postgres-unused", "redis"} {
		require.NoError(t, os.MkdirAll(filepath.Join(deployment, directory), 0o755))
	}
	appEnvironment := map[string]string{"AUTO_SETUP": "true", "ADMIN_EMAIL": integrationAdminEmail, "ADMIN_PASSWORD": integrationAdminPassword, "SERVER_PORT": "8080", "DATABASE_HOST": "postgres", "DATABASE_PORT": "5432", "DATABASE_USER": "sub2api", "DATABASE_PASSWORD": "fixture-postgres-password", "DATABASE_DBNAME": "sub2api", "DATABASE_SSLMODE": "disable", "REDIS_HOST": "redis", "REDIS_PORT": "6379", "REDIS_PASSWORD": "fixture-redis-password", "JWT_SECRET": strings.Repeat("b", 64), "TOTP_ENCRYPTION_KEY": strings.Repeat("c", 64), "OPS_ENABLED": "false", "DASHBOARD_AGGREGATION_ENABLED": "false", "USAGE_CLEANUP_ENABLED": "false", "TOKEN_REFRESH_ENABLED": "false", "GATEWAY_CN_PROVIDERS_BALANCE_CHECK_ENABLED": "false", "UPDATE_CONTAINER_ENABLED": "true", "UPDATE_SOCKET_PATH": "/run/kano-updater/control/agent.sock"}
	for key, value := range overrides {
		appEnvironment[key] = value
	}
	health := map[string]any{"test": []string{"CMD", "wget", "-q", "-T", "2", "-O", "/dev/null", "http://127.0.0.1:8080/health"}, "interval": "1s", "timeout": "3s", "retries": 120, "start_period": "5s"}
	compose := map[string]any{"name": filepath.Base(root), "services": map[string]any{
		"sub2api":  map[string]any{"image": "kano-sub2api-test:base", "environment": appEnvironment, "ports": []string{"127.0.0.1:19081:8080"}, "volumes": []string{filepath.Join(deployment, "app") + ":/app/data", filepath.Dir(config.ControlSocket) + ":/run/kano-updater/control:ro"}, "healthcheck": health, "networks": []string{"backend"}},
		"postgres": map[string]any{"image": "postgres:18-alpine", "environment": map[string]string{"POSTGRES_USER": "sub2api", "POSTGRES_PASSWORD": "fixture-postgres-password", "POSTGRES_DB": "sub2api", "PGDATA": "/var/lib/postgresql/data"}, "volumes": []string{filepath.Join(deployment, "postgres") + ":/var/lib/postgresql", filepath.Join(deployment, "postgres-unused") + ":/var/lib/postgresql/data"}, "networks": []string{"backend"}},
		"redis":    map[string]any{"image": "redis:8-alpine", "command": []string{"redis-server", "--requirepass", "fixture-redis-password", "--appendonly", "yes"}, "environment": map[string]string{"REDISCLI_AUTH": "fixture-redis-password"}, "volumes": []string{filepath.Join(deployment, "redis") + ":/data"}, "networks": []string{"backend"}},
	}, "networks": map[string]any{"backend": map[string]any{"internal": true}}}
	services, ok := compose["services"].(map[string]any)
	require.True(t, ok)
	app, ok := services["sub2api"].(map[string]any)
	require.True(t, ok)
	if os.Getenv("KANO_TEST_HARNESS_CONTAINER") != "" {
		// The harness proxy uses the application address on its private network.
		// A host publication is unnecessary and can collide with another fixture.
		delete(app, "ports")
	}
	app["depends_on"] = map[string]any{"postgres": map[string]string{"condition": "service_healthy"}, "redis": map[string]string{"condition": "service_healthy"}}
	pg, ok := services["postgres"].(map[string]any)
	require.True(t, ok)
	pg["healthcheck"] = map[string]any{"test": []string{"CMD", "pg_isready", "-U", "sub2api", "-d", "sub2api"}, "interval": "1s", "timeout": "3s", "retries": 90}
	redisService, ok := services["redis"].(map[string]any)
	require.True(t, ok)
	redisService["healthcheck"] = map[string]any{"test": []string{"CMD", "redis-cli", "ping"}, "interval": "1s", "timeout": "3s", "retries": 90}
	require.NoError(t, writePrivateJSON(config.ComposeFiles[0], compose))
	engine := &Engine{Config: config, Runner: CommandRunner{DiagnosticDir: filepath.Join(config.StateDir, "parent-diagnostics")}}
	t.Cleanup(func() {
		if t.Failed() {
			body, _ := engine.compose(context.Background(), "ps", "--all")
			t.Logf("fixture container state: %s", body)
			app, inspectErr := engine.container(context.Background(), "sub2api")
			if inspectErr == nil {
				body, _ = exec.Command("docker", "logs", "--tail", "80", app.ID).CombinedOutput()
				t.Logf("fixture application log: %s", body)
			} else {
				t.Logf("fixture application inspection: %v", inspectErr)
			}
		}
		_, _ = engine.compose(context.Background(), "down", "--volumes", "--remove-orphans")
	})
	return config, configPath, engine
}

func waitFixtureHealth(t *testing.T, engine *Engine) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	require.NoError(t, engine.waitHealth(ctx))
}

func seedDockerData(t *testing.T, engine *Engine) (DatabaseSnapshot, string) {
	t.Helper()
	postgres, err := engine.container(context.Background(), "postgres")
	require.NoError(t, err)
	key := "sk-" + strings.Repeat("u", 48)
	query := `UPDATE users SET balance=123.456,concurrency=7 WHERE id=1;
INSERT INTO groups(name,platform,subscription_type,status,rate_multiplier,i18n) VALUES ('fixture-group','openai','standard','active',1,'{"zh":{"name":"保留名称"}}');
INSERT INTO api_keys(user_id,key,name,group_id,status) SELECT 1,'` + key + `','existing-key',id,'active' FROM groups WHERE name='fixture-group';
INSERT INTO auth_identities(user_id,provider_type,provider_key,provider_subject,issuer,metadata) VALUES (1,'oidc','fixture-kano','existing-subject','https://auth.example.test','{"fixture":true}');
INSERT INTO user_subscriptions(user_id,group_id,starts_at,expires_at,status,assigned_at,daily_usage_usd,weekly_usage_usd,monthly_usage_usd) SELECT 1,id,now(),now()+interval '30 days','active',now(),1.25,2.50,5.00 FROM groups WHERE name='fixture-group';
INSERT INTO payment_orders(user_id,user_email,user_name,amount,pay_amount,bonus_amount,recharge_code,out_trade_no,payment_type,payment_trade_no,status,expires_at,client_ip,src_host) VALUES (1,'updater-fixture@example.test','fixture',25,20,5,'fixture-recharge','fixture-completed-order','fixture','fixture-trade','COMPLETED',now()+interval '1 day','127.0.0.1','fixture.example.test');
INSERT INTO settings(key,value) VALUES ('registration_enabled','false'),('default_balance','7.25'),('default_concurrency','4'),('sso_fixture_issuer','https://auth.example.test'),('sso_fixture_client_secret','fixture-private-value') ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value;`
	_, err = engine.pg(context.Background(), postgres, query)
	require.NoError(t, err)
	redisContainer, err := engine.container(context.Background(), "redis")
	require.NoError(t, err)
	client, err := redisClient(redisContainer, redisContainer, 0)
	require.NoError(t, err)
	defer func() { _ = client.Close() }()
	require.NoError(t, client.Set(context.Background(), "preserved-session", []byte{0, 255, 1}, time.Hour).Err())
	require.NoError(t, client.HSet(context.Background(), "preserved-hash", "a", "one", "b", "two").Err())
	client1, err := redisClient(redisContainer, redisContainer, 1)
	require.NoError(t, err)
	defer func() { _ = client1.Close() }()
	require.NoError(t, client1.RPush(context.Background(), "preserved-list", "first", "second").Err())
	before, err := engine.database(context.Background(), postgres, nil)
	require.NoError(t, err)
	return before, key
}

func fixtureAdminSession(t *testing.T, engine *Engine) string {
	t.Helper()
	body, err := json.Marshal(map[string]string{"email": integrationAdminEmail, "password": integrationAdminPassword})
	require.NoError(t, err)
	request, err := http.NewRequest(http.MethodPost, engine.Config.LiveURL+"/api/v1/auth/login", bytes.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 20 * time.Second}
	response, err := client.Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	require.Equal(t, 200, response.StatusCode)
	var envelope struct {
		Data struct {
			Token string `json:"access_token"`
		} `json:"data"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&envelope))
	require.NotEmpty(t, envelope.Data.Token)
	// Login audit writes are asynchronous. Seed/freeze only after the test's
	// initial login has been persisted, otherwise a normal delayed audit row
	// can appear as an unrelated database change in a later failure scenario.
	postgres, err := engine.container(context.Background(), "postgres")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		count, readErr := engine.pg(context.Background(), postgres, "SELECT COUNT(*) FROM audit_logs WHERE method='POST' AND path='/api/v1/auth/login' AND status_code=200")
		return readErr == nil && count == "1"
	}, 10*time.Second, 100*time.Millisecond)
	return envelope.Data.Token
}

func verifyFixtureAdminSession(t *testing.T, engine *Engine, token string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, engine.Config.LiveURL+"/api/v1/user/profile", nil)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 20 * time.Second}
	response, err := client.Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	require.Equal(t, 200, response.StatusCode)
}

func progressUnixClient(socket string) *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
}

func TestKanoUpdaterDockerBackgroundStartup(t *testing.T) {
	if os.Getenv("KANO_UPDATER_DOCKER_TEST") != "1" {
		t.Skip("enable dedicated real Docker fixture")
	}
	require.Equal(t, "linux", runtime.GOOS)
	require.Zero(t, os.Geteuid())
	config, configPath, engine := dockerFixtureEnvironment(t, map[string]string{"OPS_ENABLED": "true"})
	worker := startDockerWorker(t, configPath, "background_startup")
	defer stopDockerWorker(worker)
	client := kanoupdate.NewClient(config.ControlSocket)
	require.Eventually(t, func() bool { _, err := client.Health(context.Background()); return err == nil }, 20*time.Second, 100*time.Millisecond)
	_, err := engine.compose(context.Background(), "up", "-d")
	require.NoError(t, err)
	initialApp, err := engine.container(context.Background(), "sub2api")
	require.NoError(t, err)
	t.Logf("background fixture startup running=%v health=%s networks=%v", initialApp.State.Running, initialApp.State.Health.Status, sortedKeys(initialApp.NetworkSettings.Networks))
	if harness := os.Getenv("KANO_TEST_HARNESS_CONTAINER"); harness != "" {
		network := filepath.Base(filepath.Dir(config.DeploymentDir)) + "_backend"
		_, err = engine.run(context.Background(), "docker", "network", "connect", network, harness)
		require.NoError(t, err)
		t.Cleanup(func() { _, _ = engine.run(context.Background(), "docker", "network", "disconnect", network, harness) })
	}
	waitFixtureHealth(t, engine)
	postgres, err := engine.container(context.Background(), "postgres")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		value, queryErr := engine.pg(context.Background(), postgres, "SELECT count(*) FROM ops_system_metrics")
		return queryErr == nil && value != "0"
	}, 30*time.Second, 100*time.Millisecond)
	adminToken := fixtureAdminSession(t, engine)
	_, key := seedDockerData(t, engine)
	manifest := integrationManifest(t)
	session, err := client.Start(context.Background(), StartRequest{Version: manifest.Version, ExpectedDigest: manifest.ImageDigest, Actor: "fixture-admin:1", IdempotencyKey: "background-startup"})
	require.NoError(t, err)
	store, err := OpenStore(config.StateDir)
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	var final *Job
	require.Eventually(t, func() bool {
		job, queryErr := store.Get(context.Background(), session.Job.ID)
		if queryErr != nil {
			return false
		}
		final = job
		return !job.Status.Active() || job.Status == NeedsAttention
	}, 10*time.Minute, 200*time.Millisecond)
	require.Equal(t, Succeeded, final.Status, "enabled production workers must not mutate the maintenance baseline")
	var state UpgradeState
	require.NoError(t, json.Unmarshal(final.State, &state))
	require.True(t, state.PublicCommitted)
	evidence, err := os.ReadFile(filepath.Join(state.BackupPath, "final-evidence.json"))
	require.NoError(t, err)
	var proof struct {
		OldValues bool `json:"old_business_values_verified"`
	}
	require.NoError(t, json.Unmarshal(evidence, &proof))
	require.True(t, proof.OldValues)
	// Once the committed upgrade opens traffic, the original enabled collector
	// must resume without a settings change, another restart or a manual action.
	require.Eventually(t, func() bool {
		value, queryErr := engine.pg(context.Background(), postgres, "SELECT count(*) FROM ops_system_metrics")
		if queryErr != nil {
			return false
		}
		count, parseErr := strconv.ParseInt(value, 10, 64)
		return parseErr == nil && count > state.Snapshot.Database.Tables["ops_system_metrics"].Count
	}, 30*time.Second, 100*time.Millisecond)
	app, err := engine.container(context.Background(), "sub2api")
	require.NoError(t, err)
	require.Equal(t, "true", app.Environment()["OPS_ENABLED"])
	require.NoFileExists(t, config.MaintenanceFile)
	require.NoError(t, engine.billing(context.Background(), key))
	verifyFixtureAdminSession(t, engine, adminToken)
}

func TestKanoUpdaterDockerLifecycle(t *testing.T) {
	if os.Getenv("KANO_UPDATER_DOCKER_TEST") != "1" {
		t.Skip("enable dedicated real Docker fixture")
	}
	require.Equal(t, "linux", runtime.GOOS)
	require.Zero(t, os.Geteuid())
	for _, fault := range []string{"success", "disk", "backup_corrupt", "restore", "migration", "health", "service_restart"} {
		t.Run(fault, func(t *testing.T) {
			config, configPath, engine := dockerFixture(t)
			worker := startDockerWorker(t, configPath, fault)
			defer func() { stopDockerWorker(worker) }()
			client := kanoupdate.NewClient(config.ControlSocket)
			require.Eventually(t, func() bool { _, err := client.Health(context.Background()); return err == nil }, 20*time.Second, 100*time.Millisecond)
			_, err := engine.compose(context.Background(), "up", "-d")
			require.NoError(t, err)
			if harness := os.Getenv("KANO_TEST_HARNESS_CONTAINER"); harness != "" {
				network := filepath.Base(filepath.Dir(config.DeploymentDir)) + "_backend"
				_, err = engine.run(context.Background(), "docker", "network", "connect", network, harness)
				require.NoError(t, err)
				t.Cleanup(func() { _, _ = engine.run(context.Background(), "docker", "network", "disconnect", network, harness) })
			}
			waitFixtureHealth(t, engine)
			adminToken := fixtureAdminSession(t, engine)
			before, key := seedDockerData(t, engine)
			require.NoError(t, engine.billing(context.Background(), key))
			verifyFixtureAdminSession(t, engine, adminToken)
			manifest := integrationManifest(t)
			session, err := client.Start(context.Background(), StartRequest{Version: manifest.Version, ExpectedDigest: manifest.ImageDigest, Actor: "fixture-admin:1", IdempotencyKey: "fixture-retry"})
			require.NoError(t, err)
			duplicate, err := client.Start(context.Background(), StartRequest{Version: manifest.Version, ExpectedDigest: manifest.ImageDigest, Actor: "fixture-admin:1", IdempotencyKey: "fixture-retry"})
			require.NoError(t, err)
			require.Equal(t, session.Job.ID, duplicate.Job.ID)
			progress := progressUnixClient(config.ProgressSocket)
			request, _ := http.NewRequest(http.MethodGet, "http://progress"+session.ProgressURL+"/status", nil)
			response, err := progress.Do(request)
			require.NoError(t, err)
			require.Equal(t, 403, response.StatusCode)
			_ = response.Body.Close()
			request.AddCookie(&http.Cookie{Name: CookieName(session.Job.ID), Value: session.Capability})
			response, err = progress.Do(request)
			require.NoError(t, err)
			require.Equal(t, 200, response.StatusCode)
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			_ = response.Body.Close()
			require.NotContains(t, string(body), "fixture-private-value")
			require.NotContains(t, string(body), "fixture-postgres-password")
			if fault == "service_restart" {
				err = worker.Wait()
				var exit *exec.ExitError
				require.ErrorAs(t, err, &exit)
				require.Equal(t, 87, exit.ExitCode())
				worker = nil
				require.FileExists(t, config.MaintenanceFile)
				worker = startDockerWorker(t, configPath, "")
			}
			store, err := OpenStore(config.StateDir)
			require.NoError(t, err)
			defer func() { _ = store.Close() }()
			var final *Job
			require.Eventually(t, func() bool {
				job, getErr := store.Get(context.Background(), session.Job.ID)
				if getErr != nil {
					return false
				}
				final = job
				return !job.Status.Active() || job.Status == NeedsAttention
			}, 10*time.Minute, 200*time.Millisecond)
			if fault == "success" {
				require.Equal(t, Succeeded, final.Status)
			} else if fault == "disk" {
				require.Equal(t, Failed, final.Status)
			} else {
				require.Equal(t, FailedRolledBack, final.Status)
			}
			require.NoFileExists(t, config.MaintenanceFile)
			require.Equal(t, "fixture-admin:1", final.Actor)
			require.GreaterOrEqual(t, len(final.Steps), 2)
			version, err := engine.CurrentVersion(context.Background())
			require.NoError(t, err)
			if fault == "success" {
				require.Equal(t, integrationTarget, version)
			} else {
				require.Equal(t, "v0.2.16-1", version)
			}
			postgres, err := engine.container(context.Background(), "postgres")
			require.NoError(t, err)
			after, err := engine.database(context.Background(), postgres, &before)
			require.NoError(t, err)
			for table, original := range before.Tables {
				actual := after.Tables[table]
				if original.Count != actual.Count || original.Digest != actual.Digest {
					t.Logf("scenario=%s table=%s snapshot_rows=%d final_rows=%d", fault, table, original.Count, actual.Count)
				}
			}
			require.NoError(t, CompareDatabase(before, after, manifest, false))
			require.NoError(t, engine.billing(context.Background(), key))
			verifyFixtureAdminSession(t, engine, adminToken)
			var state UpgradeState
			require.NoError(t, json.Unmarshal(final.State, &state))
			if state.BackupComplete {
				require.NoError(t, engine.verifyBackup(&state))
				require.FileExists(t, filepath.Join(state.BackupPath, "postgres.dump"))
				require.DirExists(t, filepath.Join(state.BackupPath, "original"))
			}
			if fault == "success" {
				_, err = engine.pg(context.Background(), postgres, "UPDATE users SET balance=456.789 WHERE id=1")
				require.NoError(t, err)
				stopDockerWorker(worker)
				worker = nil
				require.NoError(t, store.Update(context.Background(), final.ID, Checking, "interrupted after public commit"))
				worker = startDockerWorker(t, configPath, "")
				require.Eventually(t, func() bool {
					job, getErr := store.Get(context.Background(), final.ID)
					return getErr == nil && job.Status == Succeeded
				}, time.Minute, 200*time.Millisecond)
				value, err := engine.pg(context.Background(), postgres, "SELECT balance FROM users WHERE id=1")
				require.NoError(t, err)
				require.True(t, strings.HasPrefix(value, "456.789"), "committed recovery must keep new business writes")
			}
			t.Logf("verified real Docker scenario=%s status=%s original_tables=%d original_rows=%d complete_backup=%t", fault, final.Status, len(before.Tables), databaseRowCount(before), state.BackupComplete)
		})
	}
}

type fixtureNginxRunner struct {
	Runner
	prefix string
}

func (r fixtureNginxRunner) Run(ctx context.Context, args []string, input io.Reader, output io.Writer) ([]byte, error) {
	if len(args) == 2 && args[0] == "nginx" && args[1] == "-t" {
		args = []string{"nginx", "-p", r.prefix, "-c", "nginx.conf", "-t"}
	}
	return r.Runner.Run(ctx, args, input, output)
}

// The external Chromium test uses the actual application, TLS nginx, scoped
// cookie and privileged worker. Only the unpublished image resolver is mocked.
func TestKanoUpdaterDockerBrowser(t *testing.T) {
	if os.Getenv("KANO_UPDATER_BROWSER_TEST") != "1" {
		t.Skip("enable dedicated HTTPS browser fixture")
	}
	require.Equal(t, "linux", runtime.GOOS)
	require.Zero(t, os.Geteuid())
	config, configPath, engine := dockerFixture(t)
	overlay := filepath.Join(config.DeploymentDir, "kano-updater.compose.json")
	config.ComposeFiles = []string{config.ComposeFiles[0], overlay, config.ImageOverride}
	engine.Config = config
	require.NoError(t, writePrivateJSON(configPath, config))
	prefix := filepath.Join(filepath.Dir(configPath), "nginx")
	require.NoError(t, os.MkdirAll(prefix, 0o755))
	_, err := engine.run(context.Background(), "openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-subj", "/CN=localhost", "-keyout", filepath.Join(prefix, "key.pem"), "-out", filepath.Join(prefix, "cert.pem"))
	require.NoError(t, err)
	site := filepath.Join(prefix, "site.conf")
	nginx := "user nginx;\npid " + prefix + "/nginx.pid;\nerror_log " + prefix + "/error.log;\nevents {}\nhttp { access_log off; include " + site + "; }\n"
	require.NoError(t, atomicFile(filepath.Join(prefix, "nginx.conf"), []byte(nginx), 0o644))
	siteBody := "server {\n listen 19443 ssl;\n ssl_certificate " + prefix + "/cert.pem;\n ssl_certificate_key " + prefix + "/key.pem;\n location /fixture/ { proxy_pass http://127.0.0.1:19085/; }\n location / {\n  proxy_pass " + config.LiveURL + ";\n }\n}\n"
	require.NoError(t, atomicFile(site, []byte(siteBody), 0o644))
	require.NoError(t, ConnectDeployment(context.Background(), config, site, fixtureNginxRunner{Runner: engine.Runner, prefix: prefix}))
	worker := startDockerWorker(t, configPath, "")
	defer func() { stopDockerWorker(worker) }()
	client := kanoupdate.NewClient(config.ControlSocket)
	require.Eventually(t, func() bool { _, checkErr := client.Health(context.Background()); return checkErr == nil }, 20*time.Second, 100*time.Millisecond)
	_, err = engine.compose(context.Background(), "up", "-d")
	require.NoError(t, err)
	if harness := os.Getenv("KANO_TEST_HARNESS_CONTAINER"); harness != "" {
		network := filepath.Base(filepath.Dir(config.DeploymentDir)) + "_backend"
		_, err = engine.run(context.Background(), "docker", "network", "connect", network, harness)
		require.NoError(t, err)
		t.Cleanup(func() { _, _ = engine.run(context.Background(), "docker", "network", "disconnect", network, harness) })
	}
	waitFixtureHealth(t, engine)
	adminToken := fixtureAdminSession(t, engine)
	before, key := seedDockerData(t, engine)
	finished := make(chan bool, 1)
	baseline := make(chan DatabaseSnapshot, 1)
	fixture := &http.Server{Addr: "127.0.0.1:19085", ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/ready":
			writeJSON(w, 200, map[string]bool{"ready": true})
		case r.Method == http.MethodGet && r.URL.Path == "/baseline":
			// Browser login updates users' login/activity timestamps and accepting
			// the existing declaration writes settings. Capture after those normal
			// setup actions, immediately before clicking the real upgrade button.
			postgres, captureErr := engine.container(r.Context(), "postgres")
			if captureErr != nil {
				w.WriteHeader(500)
				return
			}
			expectedAudits, parseErr := strconv.Atoi(r.URL.Query().Get("expected_audits"))
			if parseErr != nil || expectedAudits < 2 || expectedAudits > 3 {
				w.WriteHeader(400)
				return
			}
			waitCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			defer cancel()
			for {
				count, readErr := engine.pg(waitCtx, postgres, "SELECT COUNT(*) FROM audit_logs")
				if readErr == nil && count == strconv.Itoa(expectedAudits) {
					break
				}
				select {
				case <-waitCtx.Done():
					w.WriteHeader(500)
					return
				case <-time.After(100 * time.Millisecond):
				}
			}
			captured, captureErr := engine.database(r.Context(), postgres, nil)
			if captureErr != nil {
				w.WriteHeader(500)
				return
			}
			select {
			case baseline <- captured:
				writeJSON(w, 200, map[string]bool{"captured": true})
			default:
				w.WriteHeader(409)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/done":
			select {
			case finished <- r.URL.Query().Get("ok") == "true":
			default:
			}
			w.WriteHeader(204)
		default:
			w.WriteHeader(404)
		}
	})}
	go func() { _ = fixture.ListenAndServe() }()
	defer func() { _ = fixture.Close() }()
	_, err = engine.run(context.Background(), "nginx", "-p", prefix, "-c", "nginx.conf")
	require.NoError(t, err)
	defer func() {
		_, _ = engine.run(context.Background(), "nginx", "-p", prefix, "-c", "nginx.conf", "-s", "quit")
	}()
	t.Log("HTTPS fixture ready on port 19443; waiting for Chromium")
	select {
	case passed := <-finished:
		require.True(t, passed, "external HTTPS browser assertions failed")
	case <-time.After(10 * time.Minute):
		t.Fatal("external HTTPS browser test did not finish")
	}
	select {
	case before = <-baseline:
	default:
		t.Fatal("browser did not capture its pre-upgrade data baseline")
	}
	store, err := OpenStore(config.StateDir)
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	final, err := store.Current(context.Background())
	require.NoError(t, err)
	require.Equal(t, Succeeded, final.Status)
	require.Equal(t, "admin:1", final.Actor)
	var state UpgradeState
	require.NoError(t, json.Unmarshal(final.State, &state))
	require.NotNil(t, state.Snapshot)
	postgres, err := engine.container(context.Background(), "postgres")
	require.NoError(t, err)
	// The real POST itself creates one audit entry before traffic is frozen.
	// Account for that expected write, then independently compare every frozen
	// business value (including the complete audit history) after the UI returns.
	for table, original := range before.Tables {
		frozen := state.Snapshot.Database.Tables[table]
		if original.Count != frozen.Count || original.Digest != frozen.Digest {
			t.Logf("pre-click to stopped-write changes: %s (%d -> %d rows)", table, original.Count, frozen.Count)
		}
	}
	require.Equal(t, before.Tables["audit_logs"].Count+1, state.Snapshot.Database.Tables["audit_logs"].Count)
	before.Tables["audit_logs"] = state.Snapshot.Database.Tables["audit_logs"]
	require.NoError(t, CompareDatabase(before, state.Snapshot.Database, integrationManifest(t), false))
	audit, err := engine.pg(context.Background(), postgres, "SELECT COUNT(*) FROM audit_logs WHERE actor_user_id=1 AND method='POST' AND path='/api/v1/admin/system/update' AND status_code=202")
	require.NoError(t, err)
	require.Equal(t, "1", audit)
	after, err := engine.database(context.Background(), postgres, &state.Snapshot.Database)
	require.NoError(t, err)
	require.NoError(t, CompareDatabase(state.Snapshot.Database, after, integrationManifest(t), false))
	require.NoError(t, engine.billing(context.Background(), key))
	verifyFixtureAdminSession(t, engine, adminToken)
	require.NoError(t, engine.verifyBackup(&state))
	t.Logf("browser-clicked upgrade preserved %d original tables and %d rows", len(before.Tables), databaseRowCount(before))
}
