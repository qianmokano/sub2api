//go:build integration

package repository

import (
	"context"
	"database/sql"
	"io/fs"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

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
	baseline := fstest.MapFS{}
	names, err := fs.Glob(migrations.FS, "*.sql")
	require.NoError(t, err)
	for _, name := range names {
		if upstreamFiles[name] {
			continue
		}
		content, err := fs.ReadFile(migrations.FS, name)
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
	require.NoError(t, ApplyMigrations(ctx, db))
	require.NoError(t, ApplyMigrations(ctx, db))
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
