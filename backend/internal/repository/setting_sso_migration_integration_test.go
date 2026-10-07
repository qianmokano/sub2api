//go:build integration

package repository

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent/setting"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestSSOSettingsMigrationPostgres(t *testing.T) {
	ctx := context.Background()
	repo := NewSettingRepository(testEntClient(t)).(*settingRepository)
	keys := []string{"sso_issuer_url", "sso_enabled", "sso_only_enabled", "sso_registration_enabled", "sso_organization", "sso_application", "migration_sso_auth_v1", "oidc_connect_issuer_url"}
	before, err := repo.GetMultiple(ctx, keys)
	require.NoError(t, err)
	reset := func(values map[string]string) {
		_, err := repo.client.Setting.Delete().Where(setting.KeyIn(keys...)).Exec(ctx)
		require.NoError(t, err)
		require.NoError(t, repo.SetMultiple(ctx, values))
	}
	t.Cleanup(func() { reset(before) })
	sql, err := migrations.FS.ReadFile("241_sso_issuer_independence.sql")
	require.NoError(t, err)
	fallback := map[string]string{
		"sso_issuer_url": "https://runtime.example", "sso_enabled": "true", "sso_only_enabled": "true",
		"sso_registration_enabled": "true", "sso_organization": "kano", "sso_application": "admin/sub2api",
	}
	for _, tc := range []struct {
		name   string
		values map[string]string
		issuer string
	}{
		{"new DB empty wins", map[string]string{"sso_issuer_url": "", "sso_enabled": "false", "oidc_connect_issuer_url": "https://legacy.example"}, ""},
		{"legacy DB wins runtime", map[string]string{"oidc_connect_issuer_url": "https://legacy.example"}, "https://legacy.example"},
		{"runtime fills absent DB", nil, "https://runtime.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reset(tc.values)
			_, err := integrationDB.ExecContext(ctx, string(sql))
			require.NoError(t, err)
			require.NoError(t, repo.MigrateSSOSettings(ctx, fallback))
			got, err := repo.GetMultiple(ctx, keys)
			require.NoError(t, err)
			require.Equal(t, tc.issuer, got["sso_issuer_url"])
			if tc.values["sso_enabled"] == "false" {
				require.Equal(t, "false", got["sso_enabled"])
			}
			require.Equal(t, "done", got["migration_sso_auth_v1"])
			require.NoError(t, repo.Set(ctx, "oidc_connect_issuer_url", "https://changed-legacy.example"))
			require.NoError(t, repo.MigrateSSOSettings(ctx, map[string]string{"sso_issuer_url": "https://changed-runtime.example"}))
			issuer, err := repo.GetValue(ctx, "sso_issuer_url")
			require.NoError(t, err)
			require.Equal(t, tc.issuer, issuer)
			require.NoError(t, repo.Delete(ctx, "sso_issuer_url"))
			require.Error(t, repo.MigrateSSOSettings(ctx, fallback))
			_, err = repo.GetValue(ctx, "sso_issuer_url")
			require.ErrorIs(t, err, service.ErrSettingNotFound)
		})
	}
	t.Run("failed transaction rolls back marker and configuration", func(t *testing.T) {
		reset(map[string]string{"migration_sso_auth_v1": "pending"})
		require.Error(t, repo.MigrateSSOSettings(ctx, map[string]string{"sso_issuer_url": "https://runtime.example", strings.Repeat("x", 300): "invalid"}))
		marker, err := repo.GetValue(ctx, "migration_sso_auth_v1")
		require.NoError(t, err)
		require.Equal(t, "pending", marker)
		_, err = repo.GetValue(ctx, "sso_issuer_url")
		require.ErrorIs(t, err, service.ErrSettingNotFound)
	})
	t.Run("concurrent starts serialize", func(t *testing.T) {
		reset(map[string]string{"migration_sso_auth_v1": "pending"})
		var wg sync.WaitGroup
		results := make([]error, 2)
		for i := range results {
			wg.Add(1)
			go func(i int) { defer wg.Done(); results[i] = repo.MigrateSSOSettings(ctx, fallback) }(i)
		}
		wg.Wait()
		for _, err := range results {
			require.NoError(t, err)
		}
		got, err := repo.GetValue(ctx, "sso_issuer_url")
		require.NoError(t, err)
		require.Equal(t, fallback["sso_issuer_url"], got)
	})
}

func TestSSOCaptchaRedisAtomicConsumption(t *testing.T) {
	ctx := context.Background()
	cache := &ssoChallengeCache{client: testRedis(t)}
	created, err := cache.Set(ctx, "sso:captcha:test", "state", 5*time.Minute)
	require.NoError(t, err)
	require.True(t, created)
	created, err = cache.Set(ctx, "sso:captcha:test", "replacement", time.Minute)
	require.NoError(t, err)
	require.False(t, created)
	var wg sync.WaitGroup
	values := make([]string, 2)
	found := make([]bool, 2)
	errors := make([]error, 2)
	for i := range values {
		wg.Add(1)
		go func(i int) { defer wg.Done(); values[i], found[i], errors[i] = cache.Take(ctx, "sso:captcha:test") }(i)
	}
	wg.Wait()
	require.NoError(t, errors[0])
	require.NoError(t, errors[1])
	require.NotEqual(t, found[0], found[1])
	require.Equal(t, "state", values[0]+values[1])
	_, _, err = (&ssoChallengeCache{}).Take(ctx, "key")
	require.Error(t, err)
	_, err = (&ssoChallengeCache{}).Set(ctx, "key", "state", time.Minute)
	require.Error(t, err)
}
