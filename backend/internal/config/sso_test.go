package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSSOLoadKeepsIndependentExplicitEmptyAndFalse(t *testing.T) {
	resetViperWithJWTSecret(t)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("sso:\n  enabled: false\n  issuer_url: ''\n  organization: ''\noidc_connect:\n  enabled: true\n  issuer_url: https://legacy.example\n  client_id: ''\n"), 0600))
	t.Setenv("DATA_DIR", dir)
	cfg, err := Load()
	require.NoError(t, err)
	require.False(t, cfg.SSO.Enabled)
	require.Empty(t, cfg.SSO.IssuerURL)
	require.Empty(t, cfg.SSO.Organization)
	require.True(t, cfg.SSOConfigured["enabled"])
	require.True(t, cfg.SSOConfigured["issuer_url"])
	require.True(t, cfg.SSOConfigured["organization"])
	require.Equal(t, "https://legacy.example", *cfg.LegacySSOIssuer)
}

func TestSSOLoadEnvironmentPresenceAndDefaults(t *testing.T) {
	resetViperWithJWTSecret(t)
	t.Setenv("SSO_ENABLED", "true")
	t.Setenv("SSO_ONLY_ENABLED", "false")
	t.Setenv("SSO_REGISTRATION_ENABLED", "true")
	t.Setenv("SSO_ISSUER_URL", "")
	t.Setenv("SSO_APPLICATION", "")
	t.Setenv("OIDC_CONNECT_ISSUER_URL", "")
	cfg, err := Load()
	require.NoError(t, err)
	require.True(t, cfg.SSO.Enabled)
	require.False(t, cfg.SSO.OnlyEnabled)
	require.True(t, cfg.SSO.RegistrationEnabled)
	require.Empty(t, cfg.SSO.IssuerURL)
	require.Empty(t, cfg.SSO.Application)
	require.Equal(t, "kano", cfg.SSO.Organization)
	require.Empty(t, *cfg.LegacySSOIssuer)
	require.True(t, cfg.SSOConfigured["issuer_url"])
	require.True(t, cfg.SSOConfigured["only_enabled"])
}

func TestSSOLoadDoesNotConsumeLegacyProtocolSecrets(t *testing.T) {
	resetViperWithJWTSecret(t)
	t.Setenv("OIDC_CONNECT_ENABLED", "true")
	t.Setenv("OIDC_CONNECT_CLIENT_SECRET", "legacy-secret")
	cfg, err := Load()
	require.NoError(t, err)
	require.False(t, cfg.SSO.Enabled)
	require.Nil(t, cfg.LegacySSOIssuer)
	require.Equal(t, "admin/sub2api", cfg.SSO.Application)
	require.False(t, cfg.SSOConfigured["issuer_url"])
}
