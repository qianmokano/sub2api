//go:build integration

package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"
	"unicode"

	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

func upgradeMigrationFS(t *testing.T, through int) fstest.MapFS {
	t.Helper()
	selected := fstest.MapFS{}
	names, err := fs.Glob(migrations.FS, "*.sql")
	require.NoError(t, err)
	for _, name := range names {
		prefix, _, ok := strings.Cut(name, "_")
		require.True(t, ok, name)
		// Historical migrations may suffix the numeric prefix, for example 006b.
		version, err := strconv.Atoi(strings.TrimRightFunc(prefix, unicode.IsLetter))
		require.NoError(t, err, name)
		if version > through {
			continue
		}
		content, err := fs.ReadFile(migrations.FS, name)
		require.NoError(t, err)
		selected[name] = &fstest.MapFile{Data: content}
	}
	return selected
}

func TestMigrationsRunner_UpgradeV0214PreservesPassportMigration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	pg, err := tcpostgres.Run(ctx, selectDockerImage(ctx, postgresImageTag),
		tcpostgres.WithDatabase("sub2api_upgrade"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		tcpostgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pg.Terminate(context.Background())) })
	dsn, err := pg.ConnectionString(ctx, "sslmode=disable", "TimeZone=UTC")
	require.NoError(t, err)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	// Recreate the published fork schema before the two upstream 241 files existed.
	upstreamFiles := map[string]bool{
		"241_add_payment_order_bonus_amount.sql": true,
		"241_add_typesafe_platform.sql":          true,
	}
	target := upgradeMigrationFS(t, 241)
	baseline := fstest.MapFS{}
	names, err := fs.Glob(target, "*.sql")
	require.NoError(t, err)
	for _, name := range names {
		if upstreamFiles[name] {
			continue
		}
		content, err := fs.ReadFile(target, name)
		require.NoError(t, err)
		baseline[name] = &fstest.MapFile{Data: content}
	}
	require.NoError(t, applyMigrationsFS(ctx, db, baseline))
	_, err = db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES
		('sso_issuer_url', 'https://passport.example'),
		('sso_enabled', 'true'), ('sso_only_enabled', 'true')
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE settings SET value = 'done' WHERE key = 'migration_sso_auth_v1'`)
	require.NoError(t, err)
	var beforeCount int
	var passportChecksum string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations").Scan(&beforeCount))
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT checksum FROM schema_migrations WHERE filename = '241_sso_issuer_independence.sql'",
	).Scan(&passportChecksum))

	// A new file sorted before an already applied 241 migration must still run.
	require.NoError(t, applyMigrationsFS(ctx, db, target))
	require.NoError(t, applyMigrationsFS(ctx, db, target))
	var afterCount int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations").Scan(&afterCount))
	require.Equal(t, beforeCount+len(upstreamFiles), afterCount)
	var afterChecksum string
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT checksum FROM schema_migrations WHERE filename = '241_sso_issuer_independence.sql'",
	).Scan(&afterChecksum))
	require.Equal(t, passportChecksum, afterChecksum)
	for key, expected := range map[string]string{
		"sso_issuer_url": "https://passport.example", "sso_enabled": "true",
		"sso_only_enabled": "true", "migration_sso_auth_v1": "done",
	} {
		var value string
		require.NoError(t, db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = $1", key).Scan(&value))
		require.Equal(t, expected, value)
	}
	var defaultBonus string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT column_default FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'payment_orders' AND column_name = 'bonus_amount'`,
	).Scan(&defaultBonus))
	require.Contains(t, defaultBonus, "0")
	for _, constraint := range []string{"user_platform_quotas_platform_check", "composite_model_routes_target_platform_check"} {
		var definition string
		require.NoError(t, db.QueryRowContext(ctx,
			"SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1", constraint,
		).Scan(&definition))
		require.Contains(t, definition, "typesafe")
	}
}

func TestMigrationsRunner_UpgradeV0216PreservesPassportAndBusinessRows(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	pg, err := tcpostgres.Run(ctx, selectDockerImage(ctx, postgresImageTag),
		tcpostgres.WithDatabase("sub2api_upgrade_v0216"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		tcpostgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pg.Terminate(context.Background())) })
	dsn, err := pg.ConnectionString(ctx, "sslmode=disable", "TimeZone=UTC")
	require.NoError(t, err)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	require.NoError(t, applyMigrationsFS(ctx, db, upgradeMigrationFS(t, 241)))

	var userID, groupID int64
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO users
		(email, password_hash, balance, concurrency) VALUES ('upgrade@example.test', 'preserved-password-hash', 27.65, 3)
		RETURNING id`).Scan(&userID))
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO groups
		(name, description, platform) VALUES ('upgrade-group', 'preserved description', 'openai') RETURNING id`).Scan(&groupID))
	_, err = db.ExecContext(ctx, `INSERT INTO api_keys (user_id, key, name, group_id)
		VALUES ($1, 'sk-upgrade-preserved', 'preserved key', $2)`, userID, groupID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO auth_identities
		(user_id, provider_type, provider_key, provider_subject, issuer, verified_at, metadata)
		VALUES ($1, 'oidc', 'https://passport.example', 'preserved-casdoor-subject',
		'https://passport.example', NOW(), '{"email_verified":true}'::jsonb)`, userID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO payment_orders (user_id, amount, pay_amount, bonus_amount, expires_at)
		VALUES ($1, 20.00, 20.00, 3.25, NOW() + INTERVAL '1 day')`, userID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES
		('sso_issuer_url', 'https://passport.example'), ('sso_enabled', 'true'),
		('sso_only_enabled', 'true'), ('sso_registration_enabled', 'true'), ('migration_sso_auth_v1', 'done')
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`)
	require.NoError(t, err)

	rowQueries := map[string]string{
		"users":           "SELECT jsonb_agg(to_jsonb(t) ORDER BY id)::text FROM users t",
		"api_keys":        "SELECT jsonb_agg(to_jsonb(t) ORDER BY id)::text FROM api_keys t",
		"auth_identities": "SELECT jsonb_agg(to_jsonb(t) ORDER BY id)::text FROM auth_identities t",
		"payment_orders":  "SELECT jsonb_agg(to_jsonb(t) ORDER BY id)::text FROM payment_orders t",
		"groups":          "SELECT jsonb_agg(to_jsonb(t) - 'i18n' - 'image_count_billing' ORDER BY id)::text FROM groups t",
		"settings":        "SELECT jsonb_agg(to_jsonb(t) ORDER BY id)::text FROM settings t",
	}
	before := map[string]string{}
	for table, query := range rowQueries {
		var rows string
		require.NoError(t, db.QueryRowContext(ctx, query).Scan(&rows))
		before[table] = rows
	}
	var oldMigrationJSON string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT jsonb_object_agg(filename, checksum)::text FROM schema_migrations").Scan(&oldMigrationJSON))
	oldMigrations := map[string]string{}
	require.NoError(t, json.Unmarshal([]byte(oldMigrationJSON), &oldMigrations))
	var cacheTable sql.NullString
	require.NoError(t, db.QueryRowContext(ctx, "SELECT to_regclass('public.gemini_cached_contents')").Scan(&cacheTable))
	require.False(t, cacheTable.Valid)

	require.NoError(t, ApplyMigrations(ctx, db))
	require.NoError(t, ApplyMigrations(ctx, db))
	for table, query := range rowQueries {
		var rows string
		require.NoError(t, db.QueryRowContext(ctx, query).Scan(&rows))
		require.Equal(t, before[table], rows, table)
	}
	for name, checksum := range oldMigrations {
		var actual string
		require.NoError(t, db.QueryRowContext(ctx, "SELECT checksum FROM schema_migrations WHERE filename = $1", name).Scan(&actual))
		require.Equal(t, checksum, actual, name)
	}
	newMigrations := []string{
		"242_drop_platform_check_constraints.sql", "242_group_i18n.sql", "243_gemini_cached_contents.sql",
		"244_gemini_cached_contents_channel_usage.sql", "245_gemini_cached_contents_mutation_lease.sql",
		"246_enable_gemini_image_generation_groups.sql", "247_scoped_admin_api_keys.sql",
		"248_group_image_count_billing.sql",
	}
	var migrationCount int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations").Scan(&migrationCount))
	require.Equal(t, len(oldMigrations)+len(newMigrations), migrationCount)
	for _, name := range newMigrations {
		var count int
		require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations WHERE filename = $1", name).Scan(&count))
		require.Equal(t, 1, count, name)
	}
	var nonEmptyI18n, cacheRows, cacheColumns, oldConstraints int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM groups WHERE i18n IS DISTINCT FROM '{}'::jsonb").Scan(&nonEmptyI18n))
	require.Zero(t, nonEmptyI18n)
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM gemini_cached_contents").Scan(&cacheRows))
	require.Zero(t, cacheRows)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'gemini_cached_contents'
		AND column_name IN ('channel_usage', 'mutation_token', 'mutation_lease_until')`).Scan(&cacheColumns))
	require.Equal(t, 3, cacheColumns)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pg_constraint
		WHERE conname IN ('user_platform_quotas_platform_check', 'composite_model_routes_target_platform_check')`).Scan(&oldConstraints))
	require.Zero(t, oldConstraints)
}
