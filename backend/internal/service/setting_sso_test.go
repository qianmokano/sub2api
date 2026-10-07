//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type ssoMigrationRepoStub struct {
	settingRepoStub
	defaults map[string]string
}

func (r *ssoMigrationRepoStub) MigrateSSOSettings(_ context.Context, values map[string]string) error {
	r.defaults = values
	return r.err
}

func TestSSOMigrationRuntimePriorityAndExplicitValues(t *testing.T) {
	legacy := "https://legacy.example"
	for _, tc := range []struct {
		name       string
		configured bool
		issuer     string
		want       string
	}{
		{"new explicit empty", true, "", ""},
		{"new runtime", true, "https://new.example", "https://new.example"},
		{"legacy runtime once", false, "", legacy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &ssoMigrationRepoStub{}
			svc := NewSettingService(repo, &config.Config{
				SSO:             config.SSOConfig{IssuerURL: tc.issuer},
				SSOConfigured:   map[string]bool{"issuer_url": tc.configured, "organization": true, "application": true},
				LegacySSOIssuer: &legacy,
			})
			require.NoError(t, svc.MigrateSSOSettings(context.Background()))
			require.Equal(t, tc.want, repo.defaults[SettingKeySSOIssuerURL])
			require.Equal(t, "false", repo.defaults[SettingKeySSOEnabled])
			require.Empty(t, repo.defaults[SettingKeySSOOrganization])
			require.Empty(t, repo.defaults[SettingKeySSOApplication])
		})
	}
}

func TestSSOSettingsExplicitDBValuesAndInvalidFlags(t *testing.T) {
	repo := &settingRepoStub{values: map[string]string{
		SettingKeySSOEnabled: "false", SettingKeySSOIssuerURL: "", SettingKeySSOOrganization: "", SettingKeySSOApplication: "",
		"oidc_connect_issuer_url": "https://legacy.example", "oidc_connect_enabled": "true",
	}}
	svc := NewSettingService(repo, &config.Config{SSO: config.SSOConfig{Enabled: true, IssuerURL: "https://runtime.example"}})
	got, err := svc.GetSSOSettings(context.Background())
	require.NoError(t, err)
	require.False(t, got.Enabled)
	require.Empty(t, got.Config.Issuer)
	require.Empty(t, got.Config.Organization)
	require.Empty(t, got.Config.Application)
	repo.values[SettingKeySSOOnlyEnabled] = "invalid"
	_, err = svc.GetSSOSettings(context.Background())
	require.ErrorIs(t, err, ErrServiceUnavailable)
}
