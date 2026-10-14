//go:build unit

package kanoagent

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	miniserver "github.com/alicebob/miniredis/v2/server"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	root, canonicalErr := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, canonicalErr)
	deployment := filepath.Join(root, "deployment")
	require.NoError(t, os.MkdirAll(deployment, 0o700))
	return Config{DeploymentDir: deployment, ComposeFiles: []string{filepath.Join(deployment, "compose.yml"), filepath.Join(deployment, "image.json")}, ImageOverride: filepath.Join(deployment, "image.json"), BackupDir: filepath.Join(root, "backups"), StateDir: filepath.Join(root, "state"), MaintenanceFile: filepath.Join(root, "public", "maintenance"), ControlSocket: filepath.Join(root, "control", "agent.sock"), ProgressSocket: filepath.Join(root, "progress", "agent.sock"), ControlGID: os.Getgid(), ProgressGID: os.Getgid(), LiveURL: "http://127.0.0.1:18081"}
}

func TestNativeRedisSnapshotsEveryDatabaseAndSavesWithoutChangingValues(t *testing.T) {
	ctx := context.Background()
	server := miniredis.RunT(t)
	server.SetTime(time.Now())
	server.RequireAuth("fixture-password")
	configuration := "2"
	require.NoError(t, server.Server().Register("CONFIG", func(peer *miniserver.Peer, _ string, _ []string) {
		peer.WriteLen(2)
		peer.WriteBulk("databases")
		peer.WriteBulk(configuration)
	}))
	require.NoError(t, server.Server().Register("SAVE", func(peer *miniserver.Peer, _ string, _ []string) { peer.WriteInline("OK") }))
	container := Container{ID: "redis"}
	container.Config.Cmd = []string{"redis-server", "--port", server.Port()}
	container.Config.Env = []string{"REDIS_PASSWORD=fixture-password"}
	container.NetworkSettings.Networks = map[string]struct{ IPAddress string }{"private": {IPAddress: "127.0.0.1"}}
	for db := 0; db < 2; db++ {
		client, err := redisClient(container, container, db)
		require.NoError(t, err)
		require.NoError(t, client.Set(ctx, "same-key", []byte{byte(db), 255}, time.Hour).Err())
		require.NoError(t, client.Close())
	}
	backend := nativeRedis{}
	require.NoError(t, backend.Ready(ctx, container, container))
	before, err := backend.Snapshot(ctx, container, container)
	require.NoError(t, err)
	require.Len(t, before, 2)
	key := hex.EncodeToString([]byte("same-key"))
	require.Len(t, before[0], 1)
	require.Len(t, before[1], 1)
	require.NotEqual(t, before[0][key].Digest, before[1][key].Digest)
	require.NoError(t, backend.Save(ctx, container))
	after, err := backend.Snapshot(ctx, container, container)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.NoError(t, compareRedis(before, after, time.Now()))
	configuration = "invalid"
	_, err = backend.Snapshot(ctx, container, container)
	require.Error(t, err)
	configuration = "2000"
	_, err = backend.Snapshot(ctx, container, container)
	require.Error(t, err)
	container.Config.Cmd = []string{"redis-server", "--port", "invalid"}
	_, err = redisAddress(container)
	require.Error(t, err)
	container.Config.Cmd = []string{"redis-server", "--port"}
	_, err = redisAddress(container)
	require.Error(t, err)
	_, err = redisClient(Container{}, Container{}, 0)
	require.Error(t, err)
}

type runnerFunc func(context.Context, []string, io.Reader, io.Writer) ([]byte, error)

func (f runnerFunc) Run(ctx context.Context, args []string, input io.Reader, output io.Writer) ([]byte, error) {
	return f(ctx, args, input, output)
}

func TestConfigRejectsAmbiguousOrUnsafeDeployment(t *testing.T) {
	base := testConfig(t)
	require.NoError(t, base.Validate())
	path := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, writePrivateJSON(path, base))
	loaded, err := LoadConfig(path)
	require.NoError(t, err)
	require.Equal(t, base, loaded)
	for _, mutate := range []func(*Config){func(c *Config) { c.StateDir = "relative" }, func(c *Config) { c.BackupDir = c.DeploymentDir }, func(c *Config) { c.StateDir = filepath.Join(c.DeploymentDir, "state") }, func(c *Config) { c.ImageOverride = filepath.Join(t.TempDir(), "other") }, func(c *Config) { c.ComposeFiles = nil }, func(c *Config) {
		c.ComposeFiles = []string{c.ImageOverride, filepath.Join(c.DeploymentDir, "compose.yml")}
	}, func(c *Config) { c.LiveURL = "https://evil.test" }, func(c *Config) { c.LiveURL = "http://user@127.0.0.1:8080" }, func(c *Config) { c.LiveURL = "http://127.0.0.1:8080/path" }, func(c *Config) { c.ControlGID = -1 }, func(c *Config) { c.MaintenanceFile = "/run/kano-updater/maintenance" }} {
		value := base
		value.ComposeFiles = append([]string(nil), base.ComposeFiles...)
		mutate(&value)
		require.Error(t, value.Validate())
	}
	require.NoError(t, os.WriteFile(path, []byte(`{"unrecognized":"value"}`), 0o600))
	_, err = LoadConfig(path)
	require.Error(t, err)
	_, err = LoadConfig(filepath.Join(t.TempDir(), "missing"))
	require.Error(t, err)
	for _, bad := range []string{"/", "/etc/config", "/run/docker.sock", "/var/lib/docker/volumes", filepath.Dir(base.BackupDir)} {
		require.Error(t, validateDataPath(bad, base))
	}
	require.NoError(t, validateDataPath(filepath.Join(base.DeploymentDir, "data"), base))
	require.True(t, inside("/opt/app/data", "/opt/app"))
	require.False(t, inside("/opt/application", "/opt/app"))
}

func TestColdFileCopyVerifiesBytesModesOwnershipAndSymlinks(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	source := filepath.Join(root, "data")
	require.NoError(t, os.MkdirAll(filepath.Join(source, "nested"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(source, "nested", "state"), []byte("complete-data"), 0o640))
	require.NoError(t, os.Symlink("nested/state", filepath.Join(source, "link")))
	before, err := digestTree(source)
	require.NoError(t, err)
	require.Equal(t, "link", before["link"].Type)
	require.Equal(t, "nested/state", before["link"].Link)
	saved := filepath.Join(root, "saved")
	runner := CommandRunner{DiagnosticDir: filepath.Join(root, "diagnostics")}
	require.NoError(t, copyRoot(ctx, runner, source, saved))
	record := SavedRoot{Source: source, Saved: saved, Files: before}
	require.NoError(t, verifyRoot(record))
	require.Error(t, copyRoot(ctx, runner, source, saved))
	require.NoError(t, os.WriteFile(filepath.Join(saved, "nested", "state"), []byte("corruption"), 0o640))
	require.Error(t, verifyRoot(record))
	require.NoError(t, atomicFile(filepath.Join(root, "override.json"), []byte("first"), 0o600))
	require.NoError(t, atomicFile(filepath.Join(root, "override.json"), []byte("second"), 0o600))
	body, err := os.ReadFile(filepath.Join(root, "override.json"))
	require.NoError(t, err)
	require.Equal(t, "second", string(body))
	_, err = runner.Run(ctx, []string{"false"}, nil, nil)
	require.Error(t, err)
	require.Equal(t, "updater command false failed", err.Error())
	logs, err := os.ReadDir(runner.DiagnosticDir)
	require.NoError(t, err)
	require.Len(t, logs, 1)
	_, err = runner.Run(ctx, nil, nil, nil)
	require.Error(t, err)
	_, err = digestTree(filepath.Join(root, "missing"))
	require.Error(t, err)
}

func originalDatabase() DatabaseSnapshot {
	return DatabaseSnapshot{Tables: map[string]TableSnapshot{
		"users":             {Columns: []Column{{Name: "id", Type: "int8"}, {Name: "balance", Type: "numeric"}}, Count: 2, Digest: "all-original-users"},
		"groups":            {Columns: []Column{{Name: "id", Type: "int8"}, {Name: "i18n", Type: "jsonb", Default: "'{}'::jsonb"}}, Count: 1, Digest: "customized-i18n"},
		"payment_orders":    {Columns: []Column{{Name: "id", Type: "int8"}, {Name: "bonus_amount", Type: "numeric"}}, Count: 3, Digest: "existing-payment-bonus"},
		"schema_migrations": {Columns: []Column{{Name: "filename", Type: "text"}}, Count: 1, Digest: "migration-row"},
		"settings":          {Columns: []Column{{Name: "key", Type: "text"}, {Name: "value", Type: "text"}}, Count: 2, Digest: "settings-row"},
	}, Migrations: validManifest().Migrations, Settings: map[string]string{"sso_enabled": "true", "default_balance": "0"}}
}

func copyDatabase(value DatabaseSnapshot) DatabaseSnapshot {
	body, _ := json.Marshal(value)
	var result DatabaseSnapshot
	_ = json.Unmarshal(body, &result)
	return result
}

func TestMigrationComparisonProtectsAllExistingFields(t *testing.T) {
	before := originalDatabase()
	after := copyDatabase(before)
	manifest := validManifest()
	require.NoError(t, CompareDatabase(before, after, manifest, false))
	require.NoError(t, validateRestoredDatabase(before, after))
	for _, mutate := range []func(*DatabaseSnapshot){
		func(db *DatabaseSnapshot) {
			table := db.Tables["users"]
			table.Digest = "changed balance"
			db.Tables["users"] = table
		},
		func(db *DatabaseSnapshot) {
			table := db.Tables["groups"]
			table.Digest = "lost customized i18n"
			db.Tables["groups"] = table
		},
		func(db *DatabaseSnapshot) {
			table := db.Tables["payment_orders"]
			table.Digest = "changed bonus_amount"
			db.Tables["payment_orders"] = table
		},
		func(db *DatabaseSnapshot) { delete(db.Tables, "users") },
		func(db *DatabaseSnapshot) {
			table := db.Tables["users"]
			table.Columns[1].Type = "text"
			db.Tables["users"] = table
		},
		func(db *DatabaseSnapshot) {
			table := db.Tables["users"]
			table.Columns = append(table.Columns, Column{Name: "unexpected", Type: "text"})
			db.Tables["users"] = table
		},
		func(db *DatabaseSnapshot) { db.Tables["unexpected_table"] = TableSnapshot{} },
		func(db *DatabaseSnapshot) { db.Migrations["241_sso_issuer_independence.sql"] = "tampered" },
		func(db *DatabaseSnapshot) { db.Migrations["new.sql"] = "not in release catalog" },
		func(db *DatabaseSnapshot) { db.Settings["sso_enabled"] = "false" },
		func(db *DatabaseSnapshot) { db.Settings["default_balance"] = "100" },
		func(db *DatabaseSnapshot) { db.Settings["unexpected_setting"] = "true" },
	} {
		after = copyDatabase(before)
		mutate(&after)
		require.Error(t, CompareDatabase(before, after, manifest, false))
		require.Error(t, validateRestoredDatabase(before, after))
	}
	manifest.Validation.NewTables = map[string]int64{"cache": 0}
	after = copyDatabase(before)
	after.Tables["cache"] = TableSnapshot{Count: 0}
	require.NoError(t, CompareDatabase(before, after, manifest, false))
	table := after.Tables["cache"]
	table.Count = 1
	after.Tables["cache"] = table
	require.Error(t, CompareDatabase(before, after, manifest, false))
	manifest = validManifest()
	manifest.Validation.AddedColumns = map[string]map[string]ColumnRule{"users": {"metadata": {Type: "jsonb", Default: "'{}'::jsonb", Value: json.RawMessage(`{}`)}}}
	after = copyDatabase(before)
	table = after.Tables["users"]
	table.Columns = append(table.Columns, Column{Name: "metadata", Type: "jsonb", Default: "'{}'::jsonb"})
	after.Tables["users"] = table
	require.NoError(t, CompareDatabase(before, after, manifest, false))
	manifest = validManifest()
	manifest.Validation.Settings = map[string]string{"feature_enabled": "true"}
	after = copyDatabase(before)
	after.Settings["feature_enabled"] = "true"
	require.NoError(t, CompareDatabase(before, after, manifest, false))
	before.Settings["feature_enabled"] = "customized"
	after.Settings["feature_enabled"] = "customized"
	require.NoError(t, CompareDatabase(before, after, manifest, false))
	manifest = validManifest()
	after = copyDatabase(before)
	after.Settings, _ = pausedSettings(before.Settings)
	require.NoError(t, CompareDatabase(before, after, manifest, true))
	require.Error(t, CompareDatabase(before, after, manifest, false))
}

func TestDatabaseProjectionAndAddedValueCheck(t *testing.T) {
	queries := []string{}
	runner := runnerFunc(func(_ context.Context, args []string, _ io.Reader, _ io.Writer) ([]byte, error) {
		query := args[len(args)-1]
		queries = append(queries, query)
		switch {
		case strings.Contains(query, "FROM pg_tables"):
			return []byte(`["groups"]`), nil
		case strings.Contains(query, "information_schema.columns"):
			return []byte(`[{"name":"id","type":"int8","nullable":false,"default":""},{"name":"i18n","type":"jsonb","nullable":false,"default":"'{}'::jsonb"}]`), nil
		case strings.Contains(query, "md5(row_to_json"):
			return []byte("1|2|3"), nil
		case strings.Contains(query, "jsonb_object_agg(filename"):
			return []byte(`{"migration.sql":"checksum"}`), nil
		case strings.Contains(query, "jsonb_object_agg(key"):
			return []byte(`{"sso_enabled":"true"}`), nil
		default:
			return []byte("0"), nil
		}
	})
	engine := &Engine{Runner: runner}
	container := Container{ID: "postgres"}
	container.Config.Env = []string{"POSTGRES_USER=postgres", "POSTGRES_DB=sub2api"}
	before := originalDatabase()
	result, err := engine.database(context.Background(), container, &before)
	require.NoError(t, err)
	require.Equal(t, int64(1), result.Tables["groups"].Count)
	require.True(t, strings.Contains(strings.Join(queries, "\n"), `SELECT "id","i18n" FROM public."groups"`))
	policy := ValidationPolicy{AddedColumns: map[string]map[string]ColumnRule{"groups": {"i18n": {Value: json.RawMessage(`{}`)}}}}
	queries = nil
	require.NoError(t, engine.checkAddedValues(context.Background(), container, before, policy))
	require.Empty(t, queries, "previously added i18n must retain arbitrary existing values")
	policy.AddedColumns["groups"]["new_column"] = ColumnRule{Value: json.RawMessage(`"it's safe"`)}
	require.NoError(t, engine.checkAddedValues(context.Background(), container, before, policy))
	require.Contains(t, strings.Join(queries, "\n"), "it''s safe")
	require.Equal(t, `"odd""name"`, quoteIdentifier(`odd"name`))
	require.Equal(t, `'it''s safe'`, quoteLiteral(`it's safe`))
	_, err = engine.pg(context.Background(), Container{}, "SELECT 1")
	require.Error(t, err)
	_, err = pausedSettings(map[string]string{"ops_runtime_log_config": "invalid"})
	require.Error(t, err)
	engine.Runner = runnerFunc(func(context.Context, []string, io.Reader, io.Writer) ([]byte, error) {
		return nil, errors.New("private error")
	})
	_, err = engine.database(context.Background(), container, nil)
	require.Error(t, err)
}

func TestRedisCanonicalValuesAndAbsoluteExpiry(t *testing.T) {
	ctx := context.Background()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr(), Protocol: 2})
	defer func() { _ = client.Close() }()
	require.NoError(t, client.Set(ctx, "string", []byte{0, 255, 1}, 0).Err())
	require.NoError(t, client.HSet(ctx, "hash", "a", "1", "b", "2").Err())
	require.NoError(t, client.SAdd(ctx, "set", "c", "a").Err())
	require.NoError(t, client.RPush(ctx, "list", "second", "first").Err())
	require.NoError(t, client.ZAdd(ctx, "zset", redis.Z{Score: 1.5, Member: "value"}).Err())
	for key, kind := range map[string]string{"string": "string", "hash": "hash", "set": "set", "list": "list", "zset": "zset"} {
		digest, err := canonicalRedis(ctx, client, key, kind)
		require.NoError(t, err)
		require.Len(t, digest, 64)
	}
	before, err := snapshotRedisDatabase(ctx, client)
	require.NoError(t, err)
	after, err := snapshotRedisDatabase(ctx, client)
	require.NoError(t, err)
	require.True(t, reflect.DeepEqual(before, after))
	require.NoError(t, compareRedis(RedisSnapshot{before}, RedisSnapshot{after}, time.Now()))
	require.NoError(t, client.Set(ctx, "string", "changed", 0).Err())
	after, err = snapshotRedisDatabase(ctx, client)
	require.NoError(t, err)
	require.Error(t, compareRedis(RedisSnapshot{before}, RedisSnapshot{after}, time.Now()))
	_, err = canonicalRedis(ctx, client, "string", "stream")
	require.Error(t, err)
	entry := RedisEntry{Type: "string", Digest: "digest", Expires: time.Now().Add(-time.Second).UnixMilli()}
	require.NoError(t, compareRedis(RedisSnapshot{{"expired": entry}}, RedisSnapshot{{}}, time.Now()))
	entry.Expires = -1
	require.Error(t, compareRedis(RedisSnapshot{{"persistent": entry}}, RedisSnapshot{{}}, time.Now()))
	require.Error(t, compareRedis(RedisSnapshot{{}}, RedisSnapshot{{"extra": entry}}, time.Now()))
	require.Error(t, compareRedis(RedisSnapshot{{}}, RedisSnapshot{}, time.Now()))
	container := Container{}
	container.Config.Env = []string{"REDIS_PASSWORD=private"}
	require.Equal(t, "private", redisPassword(container))
	container.Config.Env = nil
	container.Config.Cmd = []string{"redis-server", "--requirepass", "private"}
	require.Equal(t, "private", redisPassword(container))
	_, err = redisAddress(container)
	require.Error(t, err)
}

func TestComposeEscapingPreservesLiteralCredentials(t *testing.T) {
	value := map[string]any{"environment": map[string]any{"TOKEN": "literal${not_interpolation}"}, "command": []any{"echo", "$value"}, "count": 1}
	escaped := escapeCompose(value).(map[string]any)
	require.Equal(t, "literal$${not_interpolation}", escaped["environment"].(map[string]any)["TOKEN"])
	require.Equal(t, []any{"echo", "$$value"}, escaped["command"])
	require.Equal(t, "literal${not_interpolation}", value["environment"].(map[string]any)["TOKEN"])
	clone, err := cloneJSON(value)
	require.NoError(t, err)
	require.Equal(t, float64(1), clone["count"])
}
