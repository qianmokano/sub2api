package kanoagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"
)

func (e *Engine) checkpoint(ctx context.Context, store *Store, job *Job, state *UpgradeState) error {
	return store.Checkpoint(ctx, job.ID, state, filepath.Base(state.BackupPath))
}

// Execute treats an interrupted maintenance window as recovery, never as an
// excuse to continue an unverified switch. After public commit it only finalizes;
// it can no longer replace the database with an older snapshot.
func (e *Engine) Execute(ctx context.Context, store *Store, job *Job) (err error) {
	state := &UpgradeState{}
	if len(job.State) > 0 && json.Unmarshal(job.State, state) != nil {
		return errors.New("updater recovery checkpoint is invalid")
	}
	if err = e.Config.Validate(); err != nil {
		return err
	}
	if state.Maintenance || state.ProductionTouched || state.PublicCommitted {
		return e.recover(ctx, store, job, state)
	}
	defer func() {
		if err != nil {
			_ = e.failure(store, job, state)
		}
	}()
	if err = store.Update(ctx, job.ID, Pulling, "Pulling and verifying the release image"); err != nil {
		return err
	}
	release, resolveErr := e.Source.Resolve(ctx, job.TargetVersion)
	if resolveErr != nil {
		return resolveErr
	}
	if release == nil || release.Manifest == nil || !reflect.DeepEqual(*release.Manifest, job.Manifest) {
		return errors.New("verified release changed before execution")
	}
	verifier := e.Verifier
	if verifier == nil {
		verifier = e
	}
	if err = verifier.Prepare(ctx, job.Manifest); err != nil {
		return err
	}
	if err = store.Update(ctx, job.ID, Preflight, "Checking deployment and free space"); err != nil {
		return err
	}
	snapshot, snapshotErr := e.snapshotRuntime(ctx)
	if snapshotErr != nil {
		return snapshotErr
	}
	if snapshot.Containers["sub2api"].Config.Labels["org.opencontainers.image.version"] != job.SourceVersion {
		return errors.New("deployment changed before upgrade")
	}
	if _, err = e.storageBudget(ctx, snapshot); err != nil {
		return err
	}
	if err = e.health(ctx); err != nil {
		return err
	}
	state.Snapshot = snapshot
	state.BackupPath = filepath.Join(e.Config.BackupDir, "online-"+time.Now().UTC().Format("20060102T150405Z")+"-"+strings.TrimPrefix(job.ID, "up-"))
	state.PrivateProject = "kano-verify-" + strings.TrimPrefix(job.ID, "up-")
	state.PrivateCompose = filepath.Join(state.BackupPath, "private.json")
	if err = os.MkdirAll(state.BackupPath, 0o700); err != nil {
		return err
	}
	if err = e.checkpoint(ctx, store, job, state); err != nil {
		return err
	}
	if err = store.Update(ctx, job.ID, BackingUp, "Maintenance: creating a complete backup"); err != nil {
		return err
	}
	state.Maintenance = true
	if err = e.checkpoint(ctx, store, job, state); err != nil {
		return err
	}
	if err = e.maintenance(job.ID, true); err != nil {
		return err
	}
	if _, err = e.compose(ctx, "stop", "--timeout", "60", "sub2api"); err != nil {
		return err
	}
	app, inspectErr := e.container(ctx, "sub2api")
	if inspectErr != nil {
		return inspectErr
	}
	if app.State.Running {
		return errors.New("application did not stop writing")
	}
	postgres := snapshot.Containers["postgres"]
	snapshot.Database, err = e.database(ctx, postgres, nil)
	if err != nil {
		return err
	}
	snapshot.ReadonlyKey, err = e.readonlyKey(ctx, postgres)
	if err != nil {
		return err
	}
	if err = e.dump(ctx, postgres, state); err != nil {
		return err
	}
	snapshot.Redis, err = e.redisBackend().Snapshot(ctx, snapshot.Containers["redis"], snapshot.Containers["redis"])
	if err != nil {
		return err
	}
	if err = e.redisBackend().Save(ctx, snapshot.Containers["redis"]); err != nil {
		return err
	}
	if _, err = e.compose(ctx, "stop", "--timeout", "60"); err != nil {
		return err
	}
	for _, service := range []string{"sub2api", "postgres", "redis"} {
		info, inspectErr := e.container(ctx, service)
		if inspectErr != nil {
			return inspectErr
		}
		if info.State.Running {
			return errors.New("deployment was not completely frozen")
		}
	}
	if err = e.saveFiles(ctx, state); err != nil {
		return err
	}
	if err = syncTree(state.BackupPath); err != nil {
		return err
	}
	state.BackupComplete = true
	if err = e.checkpoint(ctx, store, job, state); err != nil {
		return err
	}
	if err = writePrivateJSON(filepath.Join(state.BackupPath, "snapshot.json"), state); err != nil {
		return err
	}
	if err = store.Update(ctx, job.ID, Restoring, "Maintenance: restoring the complete backup in isolation"); err != nil {
		return err
	}
	if err = e.isolatedRestore(ctx, state, job.Manifest); err != nil {
		return err
	}
	if err = store.Update(ctx, job.ID, Verifying, "Maintenance: verifying migrations and original business data"); err != nil {
		return err
	}
	if err = e.verifyPrivate(ctx, state, job.Manifest); err != nil {
		return err
	}
	if _, err = e.private(ctx, state, "down", "--remove-orphans"); err != nil {
		return err
	}
	if err = store.Update(ctx, job.ID, Switching, "Maintenance: switching the application image"); err != nil {
		return err
	}
	// Recheck the immutable frozen backup before modifying any production data.
	if err = e.verifyBackup(state); err != nil {
		return err
	}
	for _, service := range []string{"sub2api", "postgres", "redis"} {
		info, inspectErr := e.container(ctx, service)
		if inspectErr != nil {
			return inspectErr
		}
		if info.State.Running {
			return errors.New("original deployment resumed during verification")
		}
	}
	state.ProductionTouched = true
	if err = e.checkpoint(ctx, store, job, state); err != nil {
		return err
	}
	override := map[string]any{"services": map[string]any{"sub2api": map[string]any{"image": job.Manifest.Image + "@" + job.Manifest.ImageDigest}}}
	if err = writePrivateJSON(e.Config.ImageOverride, override); err != nil {
		return err
	}
	if _, err = e.compose(ctx, "start", "postgres", "redis"); err != nil {
		return err
	}
	if err = e.waitDependencies(ctx); err != nil {
		return err
	}
	if _, err = e.compose(ctx, "up", "-d", "--no-deps", "sub2api"); err != nil {
		return err
	}
	if err = store.Update(ctx, job.ID, Checking, "Maintenance: checking the new deployment"); err != nil {
		return err
	}
	if err = e.waitHealth(ctx); err != nil {
		return err
	}
	if err = e.verifyRuntime(ctx, state, job.Manifest); err != nil {
		return err
	}
	currentPostgres, inspectErr := e.container(ctx, "postgres")
	if inspectErr != nil {
		return inspectErr
	}
	after, databaseErr := e.database(ctx, currentPostgres, &snapshot.Database)
	if databaseErr != nil {
		return databaseErr
	}
	if err = CompareDatabase(snapshot.Database, after, job.Manifest, false); err != nil {
		return err
	}
	if err = e.checkAddedValues(ctx, currentPostgres, snapshot.Database, job.Manifest.Validation); err != nil {
		return err
	}
	if err = e.billing(ctx, snapshot.ReadonlyKey); err != nil {
		return err
	}
	if err = e.verifyBackup(state); err != nil {
		return err
	}
	if err = writePrivateJSON(filepath.Join(state.BackupPath, "final-evidence.json"), map[string]any{"operation_id": job.ID, "version": job.TargetVersion, "image_digest": job.ImageDigest, "revision": job.Manifest.Revision, "database_tables": len(snapshot.Database.Tables), "database_rows": databaseRowCount(snapshot.Database), "backup_verified": true, "isolated_restore_verified": true, "old_business_values_verified": true, "runtime_verified": true, "readonly_key_checked": snapshot.ReadonlyKey != ""}); err != nil {
		return err
	}
	state.PublicCommitted = true
	if err = e.checkpoint(ctx, store, job, state); err != nil {
		return err
	}
	if err = e.maintenance(job.ID, false); err != nil {
		return err
	}
	if err = store.Update(ctx, job.ID, Succeeded, "Upgrade complete; gateway available"); err != nil {
		return err
	}
	_ = e.cleanup(ctx, state)
	return nil
}

func (e *Engine) maintenance(id string, enabled bool) error {
	if enabled {
		directory := filepath.Dir(e.Config.MaintenanceFile)
		if err := os.MkdirAll(directory, 0o750); err != nil {
			return err
		}
		if err := os.Chmod(directory, 0o750); err != nil {
			return err
		}
		if err := os.Chown(directory, -1, e.Config.ProgressGID); err != nil {
			return err
		}
		return atomicFile(e.Config.MaintenanceFile, []byte(id+"\n"), 0o644)
	}
	if err := os.Remove(e.Config.MaintenanceFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	directory, err := os.Open(filepath.Dir(e.Config.MaintenanceFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}

func (e *Engine) dump(ctx context.Context, postgres Container, state *UpgradeState) error {
	env := postgres.Environment()
	for _, entry := range []struct {
		name string
		args []string
	}{
		{"postgres.dump", []string{"docker", "exec", postgres.ID, "pg_dump", "-U", env["POSTGRES_USER"], "-d", env["POSTGRES_DB"], "-Fc"}},
		{"postgres-globals.sql", []string{"docker", "exec", postgres.ID, "pg_dumpall", "-U", env["POSTGRES_USER"], "--globals-only"}},
	} {
		path := filepath.Join(state.BackupPath, entry.name)
		file, err := os.OpenFile(path+".partial", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, runErr := e.Runner.Run(ctx, entry.args, nil, file)
		syncErr := file.Sync()
		closeErr := file.Close()
		if runErr != nil {
			return runErr
		}
		if syncErr != nil {
			return syncErr
		}
		if closeErr != nil {
			return closeErr
		}
		if err = os.Rename(path+".partial", path); err != nil {
			return err
		}
		digest, err := digestFile(path)
		if err != nil {
			return err
		}
		if entry.name == "postgres.dump" {
			state.DumpDigest = digest
		} else {
			state.GlobalsDigest = digest
		}
	}
	return nil
}

func digestFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err = io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (e *Engine) verifyBackup(state *UpgradeState) error {
	if !state.BackupComplete || state.Snapshot == nil {
		return errors.New("a complete verified backup is required")
	}
	for _, root := range state.Snapshot.Roots {
		if err := verifyRoot(root); err != nil {
			return err
		}
	}
	for name, expected := range map[string]string{"postgres.dump": state.DumpDigest, "postgres-globals.sql": state.GlobalsDigest} {
		actual, err := digestFile(filepath.Join(state.BackupPath, name))
		if err != nil {
			return err
		}
		if expected == "" || actual != expected {
			return errors.New("logical backup checksum differs")
		}
	}
	return nil
}

func (e *Engine) isolatedRestore(ctx context.Context, state *UpgradeState, manifest Manifest) error {
	if err := e.verifyBackup(state); err != nil {
		return err
	}
	restoredMounts := map[string]string{}
	excluded := postgresSources(state.Snapshot)
	for index, root := range state.Snapshot.Roots {
		whollyExcluded := false
		for _, path := range excluded {
			if inside(root.Source, path) {
				whollyExcluded = true
			}
		}
		if whollyExcluded {
			continue
		}
		target := filepath.Join(state.BackupPath, fmt.Sprintf("restored-%d", index))
		if err := e.cloneFiltered(ctx, root, excluded, target); err != nil {
			return err
		}
		for source := range state.Snapshot.MountBackups {
			if inside(source, root.Source) {
				relative, _ := filepath.Rel(root.Source, source)
				restoredMounts[source] = filepath.Join(target, relative)
			}
		}
	}
	config, err := cloneJSON(state.Snapshot.Compose)
	if err != nil {
		return err
	}
	delete(config, "name")
	delete(config, "volumes")
	delete(config, "secrets")
	delete(config, "configs")
	config["networks"] = map[string]any{"isolated": map[string]any{"internal": true}}
	services, servicesOK := config["services"].(map[string]any)
	if !servicesOK {
		return errors.New("invalid private compose services")
	}
	for service, entry := range services {
		definition, ok := entry.(map[string]any)
		if !ok {
			return errors.New("invalid private compose service")
		}
		original := state.Snapshot.Containers[service]
		definition["container_name"] = state.PrivateProject + "-" + service
		definition["image"] = original.Image
		if service == "sub2api" {
			definition["image"] = manifest.Image + "@" + manifest.ImageDigest
		}
		definition["restart"] = "no"
		for _, key := range []string{"depends_on", "ports", "network_mode", "pid", "ipc", "devices", "privileged", "cap_add", "build", "develop", "profiles", "env_file", "secrets", "configs", "extra_hosts"} {
			delete(definition, key)
		}
		definition["networks"] = map[string]any{"isolated": map[string]any{"aliases": []string{service}}}
		mounts := []any{}
		for _, mount := range original.Mounts {
			if e.managedMount(mount) {
				continue
			}
			if service == "postgres" {
				continue
			}
			source := restoredMounts[mount.Source]
			if source == "" || !inside(source, state.BackupPath) {
				return errors.New("private data mount is not an isolated restored copy")
			}
			mounts = append(mounts, map[string]any{"type": "bind", "source": source, "target": mount.Destination, "read_only": !mount.RW})
		}
		definition["volumes"] = mounts
		environment := map[string]any{}
		for name, value := range original.Environment() {
			environment[name] = value
		}
		if service == "sub2api" {
			environment["DATABASE_HOST"] = "postgres"
			environment["REDIS_HOST"] = "redis"
			for _, key := range []string{"OPS_ENABLED", "DASHBOARD_AGGREGATION_ENABLED", "USAGE_CLEANUP_ENABLED", "TOKEN_REFRESH_ENABLED", "GATEWAY_CN_PROVIDERS_BALANCE_CHECK_ENABLED", "UPDATE_CONTAINER_ENABLED"} {
				environment[key] = "false"
			}
			environment["GATEWAY_SCHEDULING_OUTBOX_POLL_INTERVAL_SECONDS"] = "86400"
			environment["GATEWAY_SCHEDULING_FULL_REBUILD_INTERVAL_SECONDS"] = "0"
		}
		definition["environment"] = environment
	}
	cluster := filepath.Join(state.BackupPath, "private-postgres")
	if err = os.Mkdir(cluster, 0o700); err != nil {
		return err
	}
	postgres := state.Snapshot.Containers["postgres"]
	pgdata := state.Snapshot.PostgresData
	if pgdata == "" {
		return errors.New("postgresql data location is missing")
	}
	var dataMount *Mount
	for _, mount := range postgres.Mounts {
		if inside(pgdata, mount.Destination) && (dataMount == nil || len(mount.Destination) > len(dataMount.Destination)) {
			copyMount := mount
			dataMount = &copyMount
		}
	}
	if dataMount == nil {
		return errors.New("postgresql data was not inside a captured mount")
	}
	relative, _ := filepath.Rel(dataMount.Destination, pgdata)
	info, err := os.Stat(filepath.Join(state.Snapshot.MountBackups[dataMount.Source], relative))
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("postgresql data ownership is unavailable")
	}
	if err = os.Chown(cluster, int(stat.Uid), int(stat.Gid)); err != nil {
		return err
	}
	pgDefinition, definitionOK := services["postgres"].(map[string]any)
	if !definitionOK {
		return errors.New("invalid private PostgreSQL definition")
	}
	pgDefinition["volumes"] = []any{map[string]any{"type": "bind", "source": cluster, "target": "/var/lib/postgresql"}}
	pgEnvironmentValues, environmentOK := pgDefinition["environment"].(map[string]any)
	if !environmentOK {
		return errors.New("invalid private PostgreSQL environment")
	}
	pgEnvironmentValues["PGDATA"] = "/var/lib/postgresql/18/docker"
	if err = writePrivateJSON(state.PrivateCompose, escapeCompose(config)); err != nil {
		return err
	}
	if _, err = e.private(ctx, state, "up", "-d", "postgres", "redis"); err != nil {
		return err
	}
	privatePostgres, err := e.inspect(ctx, state.PrivateProject+"-postgres")
	if err != nil {
		return err
	}
	if err = e.waitPostgres(ctx, privatePostgres); err != nil {
		return err
	}
	file, err := os.Open(filepath.Join(state.BackupPath, "postgres.dump"))
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	pgEnvironment := privatePostgres.Environment()
	args := []string{"docker", "exec", "-i", privatePostgres.ID, "pg_restore", "--exit-on-error", "--no-owner", "--no-privileges", "-U", pgEnvironment["POSTGRES_USER"], "-d", pgEnvironment["POSTGRES_DB"]}
	if _, err = e.Runner.Run(ctx, args, file, nil); err != nil {
		return err
	}
	restored, err := e.database(ctx, privatePostgres, nil)
	if err != nil {
		return err
	}
	if err = validateRestoredDatabase(state.Snapshot.Database, restored); err != nil {
		return err
	}
	privateRedis, err := e.inspect(ctx, state.PrivateProject+"-redis")
	if err != nil {
		return err
	}
	redisSnapshot, err := e.redisBackend().Snapshot(ctx, privateRedis, state.Snapshot.Containers["redis"])
	if err != nil {
		return err
	}
	if err = compareRedis(state.Snapshot.Redis, redisSnapshot, time.Now()); err != nil {
		return err
	}
	return e.pausePrivateSettings(ctx, privatePostgres, state.Snapshot.Database.Settings)
}

func (e *Engine) waitPostgres(ctx context.Context, container Container) error {
	for attempt := 0; attempt < 120; attempt++ {
		if _, err := e.pg(ctx, container, "SELECT 1"); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return errors.New("isolated PostgreSQL readiness timed out")
}

func (e *Engine) verifyPrivate(ctx context.Context, state *UpgradeState, manifest Manifest) error {
	if _, err := e.private(ctx, state, "up", "-d", "--no-deps", "sub2api"); err != nil {
		return err
	}
	var app Container
	for attempt := 0; attempt < 120; attempt++ {
		info, err := e.inspect(ctx, state.PrivateProject+"-sub2api")
		if err == nil {
			app = info
			value, checkErr := e.run(ctx, "docker", "exec", app.ID, "wget", "-q", "-T", "5", "-O", "-", "http://127.0.0.1:8080/health")
			if checkErr == nil && validHealth(value) {
				break
			}
		}
		if attempt == 119 {
			return errors.New("isolated application health check failed")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if _, err := e.private(ctx, state, "stop", "--timeout", "60", "sub2api"); err != nil {
		return err
	}
	postgres, err := e.inspect(ctx, state.PrivateProject+"-postgres")
	if err != nil {
		return err
	}
	after, err := e.database(ctx, postgres, &state.Snapshot.Database)
	if err != nil {
		return err
	}
	if err = CompareDatabase(state.Snapshot.Database, after, manifest, true); err != nil {
		return err
	}
	return e.checkAddedValues(ctx, postgres, state.Snapshot.Database, manifest.Validation)
}

func validHealth(body []byte) bool {
	var value struct {
		Status string `json:"status"`
	}
	return json.Unmarshal(body, &value) == nil && value.Status == "ok"
}

func (e *Engine) request(ctx context.Context, path, key string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, e.Config.LiveURL+path, nil)
	if err != nil {
		return nil, err
	}
	if key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("local application request failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != 200 {
		return nil, errors.New("local application returned an unhealthy response")
	}
	value, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(value) > 1<<20 {
		return nil, errors.New("invalid local application response")
	}
	return value, nil
}

func (e *Engine) health(ctx context.Context) error {
	body, err := e.request(ctx, "/health", "")
	if err != nil {
		return err
	}
	if !validHealth(body) {
		return errors.New("application health response is invalid")
	}
	return nil
}

func (e *Engine) waitHealth(ctx context.Context) error {
	for attempt := 0; attempt < 180; attempt++ {
		if e.health(ctx) == nil {
			app, err := e.container(ctx, "sub2api")
			if err == nil && app.State.Running && app.State.Health.Status == "healthy" {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return errors.New("application readiness timed out")
}

func (e *Engine) billing(ctx context.Context, key string) error {
	if key == "" {
		return nil
	}
	_, err := e.request(ctx, "/v1/sub2api/billing", key)
	return err
}

func (e *Engine) verifyRuntime(ctx context.Context, state *UpgradeState, manifest Manifest) error {
	for service, before := range state.Snapshot.Containers {
		after, err := e.container(ctx, service)
		if err != nil {
			return err
		}
		if !after.State.Running {
			return errors.New("deployment service did not resume")
		}
		if service != "sub2api" {
			if after.ID != before.ID || after.Image != before.Image {
				return errors.New("a dependency container or image changed")
			}
			continue
		}
		if after.Config.Labels["org.opencontainers.image.version"] != manifest.Version || after.Config.Labels["org.opencontainers.image.revision"] != manifest.Revision {
			return errors.New("running image does not match the verified release")
		}
		if !reflect.DeepEqual(before.Environment(), after.Environment()) || !reflect.DeepEqual(before.Config.Cmd, after.Config.Cmd) || !reflect.DeepEqual(before.Config.Entrypoint, after.Config.Entrypoint) {
			return errors.New("application runtime environment or command changed")
		}
		for _, key := range []string{"PortBindings", "NetworkMode", "RestartPolicy", "Privileged", "ReadonlyRootfs", "CapAdd", "CapDrop"} {
			if !reflect.DeepEqual(before.HostConfig[key], after.HostConfig[key]) {
				return errors.New("application ports, network or runtime policy changed")
			}
		}
		beforeMounts := map[string]Mount{}
		afterMounts := map[string]Mount{}
		for _, mount := range before.Mounts {
			beforeMounts[mount.Destination] = mount
		}
		for _, mount := range after.Mounts {
			afterMounts[mount.Destination] = mount
		}
		if !reflect.DeepEqual(beforeMounts, afterMounts) {
			return errors.New("application data mounts changed")
		}
	}
	return nil
}

func (e *Engine) failure(store *Store, job *Job, state *UpgradeState) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	if state.PublicCommitted {
		_ = store.Update(ctx, job.ID, NeedsAttention, "Upgrade is committed; inspect protected diagnostics without restoring old data")
		return nil
	}
	if !state.Maintenance {
		return store.Update(ctx, job.ID, Failed, "Upgrade did not proceed; previous version remains available")
	}
	return e.recover(ctx, store, job, state)
}

func (e *Engine) recover(ctx context.Context, store *Store, job *Job, state *UpgradeState) (err error) {
	if err = e.validateCheckpoint(job, state); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = store.Update(context.Background(), job.ID, NeedsAttention, "Recovery needs administrator attention; complete backups are retained")
		}
	}()
	if state.PublicCommitted {
		if err = e.waitHealth(ctx); err != nil {
			return err
		}
		if err = e.maintenance(job.ID, false); err != nil {
			return err
		}
		if err = store.Update(ctx, job.ID, Succeeded, "Upgrade complete; gateway available"); err != nil {
			return err
		}
		_ = e.cleanup(ctx, state)
		return nil
	}
	if err = e.maintenance(job.ID, true); err != nil {
		return err
	}
	if err = store.Update(ctx, job.ID, RollingBack, "Maintenance: restoring the previous version"); err != nil {
		return err
	}
	if state.PrivateCompose != "" {
		if _, statErr := os.Stat(state.PrivateCompose); statErr == nil {
			if _, err = e.private(ctx, state, "down", "--remove-orphans"); err != nil {
				return err
			}
		}
	}
	if err = e.cleanup(ctx, state); err != nil {
		return err
	}
	if state.ProductionTouched {
		if err = e.verifyBackup(state); err != nil {
			return err
		}
		if _, err = e.compose(ctx, "stop", "--timeout", "60"); err != nil {
			return err
		}
		for _, service := range []string{"sub2api", "postgres", "redis"} {
			info, inspectErr := e.container(ctx, service)
			if inspectErr != nil {
				return inspectErr
			}
			if info.State.Running {
				return errors.New("cannot restore data while containers are running")
			}
		}
		for _, root := range state.Snapshot.Roots {
			if err = validateDataPath(root.Source, e.Config); err != nil {
				return err
			}
			// The immutable, verified cold backup survives an interrupted copy.
			// Recovery can safely repeat this step until every root matches.
			if err = os.RemoveAll(root.Source); err != nil {
				return err
			}
			if err = copyRoot(ctx, e.Runner, root.Saved, root.Source); err != nil {
				return err
			}
			restored, verifyErr := digestTree(root.Source)
			if verifyErr != nil {
				return verifyErr
			}
			if !reflect.DeepEqual(restored, root.Files) {
				return errors.New("recovered original files differ from the cold backup")
			}
		}
		if _, err = e.compose(ctx, "up", "-d", "postgres", "redis"); err != nil {
			return err
		}
		if err = e.waitDependencies(ctx); err != nil {
			return err
		}
		if _, err = e.compose(ctx, "up", "-d", "--no-deps", "sub2api"); err != nil {
			return err
		}
	} else {
		if _, err = e.compose(ctx, "start", "postgres", "redis"); err != nil {
			return err
		}
		if err = e.waitDependencies(ctx); err != nil {
			return err
		}
		if _, err = e.compose(ctx, "start", "sub2api"); err != nil {
			return err
		}
	}
	if err = e.waitHealth(ctx); err != nil {
		return err
	}
	current, versionErr := e.CurrentVersion(ctx)
	if versionErr != nil {
		return versionErr
	}
	if current != job.SourceVersion {
		return errors.New("previous application version was not restored")
	}
	if err = e.maintenance(job.ID, false); err != nil {
		return err
	}
	return store.Update(ctx, job.ID, FailedRolledBack, "Upgrade failed; previous version and data restored")
}

func (e *Engine) waitDependencies(ctx context.Context) error {
	postgres, err := e.container(ctx, "postgres")
	if err != nil {
		return err
	}
	if err = e.waitPostgres(ctx, postgres); err != nil {
		return err
	}
	redisContainer, err := e.container(ctx, "redis")
	if err != nil {
		return err
	}
	return e.redisBackend().Ready(ctx, redisContainer, redisContainer)
}

func (e *Engine) validateCheckpoint(job *Job, state *UpgradeState) error {
	if !jobPattern.MatchString(job.ID) || state.Snapshot == nil || state.BackupPath == "" || !inside(state.BackupPath, e.Config.BackupDir) || state.BackupPath == e.Config.BackupDir || state.PrivateProject != "kano-verify-"+strings.TrimPrefix(job.ID, "up-") || state.PrivateCompose != filepath.Join(state.BackupPath, "private.json") {
		return errors.New("unsafe updater recovery checkpoint")
	}
	for _, root := range state.Snapshot.Roots {
		if !inside(root.Saved, state.BackupPath) || root.Saved == state.BackupPath {
			return errors.New("unsafe backup data source")
		}
		captured := root.Source == e.Config.DeploymentDir
		for _, container := range state.Snapshot.Containers {
			for _, mount := range container.Mounts {
				if mount.Source == root.Source && !e.managedMount(mount) {
					captured = true
				}
			}
		}
		if !captured {
			return errors.New("recovery source is not a captured deployment root")
		}
		if err := validateDataPath(root.Source, e.Config); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) cleanup(ctx context.Context, state *UpgradeState) error {
	if state.BackupPath == "" || !inside(state.BackupPath, e.Config.BackupDir) || state.BackupPath == e.Config.BackupDir {
		return errors.New("unsafe temporary restore path")
	}
	if state.PrivateProject == "" {
		return errors.New("private restoration project is missing")
	}
	containers, inspectErr := e.run(ctx, "docker", "ps", "--all", "--filter", "label=com.docker.compose.project="+state.PrivateProject, "--format", "{{.ID}}")
	if inspectErr != nil {
		return inspectErr
	}
	if strings.TrimSpace(string(containers)) != "" {
		return errors.New("private restore containers still reference temporary data")
	}
	entries, err := os.ReadDir(state.BackupPath)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "restored-") || entry.Name() == "private-postgres" {
			if err = os.RemoveAll(filepath.Join(state.BackupPath, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}
