//go:build unit

package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/casdoor"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

type ssoProviderStub struct {
	identity         *casdoor.Identity
	session          *casdoor.Session
	err              error
	registerUsername string
	registerDisplay  string
	sentEmail        string
}

func (s *ssoProviderStub) Login(context.Context, string, string) (*casdoor.Identity, *casdoor.Session, error) {
	return s.identity, s.session, s.err
}
func (s *ssoProviderStub) CompleteMFA(context.Context, *casdoor.Session, string, string) (*casdoor.Identity, error) {
	return s.identity, s.err
}
func (s *ssoProviderStub) SendCode(_ context.Context, email string) error {
	s.sentEmail = email
	return s.err
}
func (s *ssoProviderStub) Register(_ context.Context, _, _, _, display, username string) (*casdoor.Identity, error) {
	s.registerUsername = username
	s.registerDisplay = display
	return s.identity, s.err
}

type ssoStoreStub struct {
	session  *casdoor.Session
	err      error
	consume  bool
	updated  bool
	unlocked bool
}

func (s *ssoStoreStub) Put(_ context.Context, _ string, v *casdoor.Session) error {
	s.session = v
	return s.err
}
func (s *ssoStoreStub) Lock(context.Context, string, string) (*casdoor.Session, error) {
	return s.session, s.err
}
func (s *ssoStoreStub) Unlock(context.Context, string, string) error { s.unlocked = true; return nil }
func (s *ssoStoreStub) Update(context.Context, string, string, *casdoor.Session) error {
	s.updated = true
	return s.err
}
func (s *ssoStoreStub) Consume(context.Context, string, string) (bool, error) {
	return s.consume, s.err
}

func newSSOTestService() (*SSOService, *ssoProviderStub, *ssoStoreStub, *settingRepoStub) {
	repo := &settingRepoStub{values: map[string]string{SettingKeySSOEnabled: "true", SettingKeySSORegistrationEnabled: "true", SettingKeyOIDCConnectEnabled: "true", SettingKeyOIDCConnectIssuerURL: "https://auth.example"}}
	settings := NewSettingService(repo, &config.Config{})
	provider := &ssoProviderStub{identity: &casdoor.Identity{Subject: "subject", Email: "u@example.com", EmailVerified: true}}
	store := &ssoStoreStub{consume: true}
	svc := NewSSOService(settings, &AuthService{}, store)
	svc.newClient = func(casdoor.Config) ssoProvider { return provider }
	return svc, provider, store, repo
}

func TestSSOLoginChallengeAndValidation(t *testing.T) {
	ctx := context.Background()
	svc, provider, store, repo := newSSOTestService()
	cfg, err := svc.settings.GetSSOSettings(ctx)
	require.NoError(t, err)
	provider.session = &casdoor.Session{Config: cfg.Config, Methods: []casdoor.MFAMethod{{Type: "otp"}}}
	result, err := svc.Login(ctx, "user", "password")
	require.NoError(t, err)
	require.Nil(t, result.User)
	require.Len(t, result.Challenge.Token, 64)
	require.Equal(t, provider.session, store.session)
	require.Equal(t, provider.session.Methods, result.Challenge.Methods)
	_, err = svc.Login(ctx, "", "password")
	require.ErrorIs(t, err, ErrInvalidCredentials)
	_, err = svc.Login(ctx, "u", strings.Repeat("p", 4097))
	require.ErrorIs(t, err, ErrInvalidCredentials)
	provider.err = casdoor.ErrCredentials
	_, err = svc.Login(ctx, "u", "pw")
	require.ErrorIs(t, err, ErrInvalidCredentials)
	provider.err = nil
	store.err = errors.New("redis offline")
	_, err = svc.Login(ctx, "u", "pw")
	require.ErrorIs(t, err, ErrServiceUnavailable)
	svc.challenges = nil
	_, err = svc.Login(ctx, "u", "pw")
	require.ErrorIs(t, err, ErrServiceUnavailable)
	provider.session = nil
	_, err = svc.Login(ctx, "u", "pw")
	require.ErrorIs(t, err, ErrServiceUnavailable, "resolver does not mint tokens without an account")
	repo.values[SettingKeySSOEnabled] = "false"
	_, err = svc.Login(ctx, "u", "pw")
	require.ErrorIs(t, err, ErrSSODisabled)
	repo.err = errors.New("settings offline")
	_, err = svc.Login(ctx, "u", "pw")
	require.ErrorIs(t, err, ErrServiceUnavailable)
}

func TestSSOMFARetryConsumptionAndConfigChange(t *testing.T) {
	ctx := context.Background()
	svc, provider, store, _ := newSSOTestService()
	cfg, err := svc.settings.GetSSOSettings(ctx)
	require.NoError(t, err)
	store.session = &casdoor.Session{Config: cfg.Config, Methods: []casdoor.MFAMethod{{Type: "otp"}}}
	token := strings.Repeat("a", 64)
	provider.err = casdoor.ErrMFACode
	_, err = svc.CompleteMFA(ctx, token, "otp", "wrong")
	require.Equal(t, "SSO_MFA_CODE_INVALID", infraerrors.Reason(err))
	require.True(t, store.updated)
	require.True(t, store.unlocked)
	provider.err = nil
	store.consume = false
	_, err = svc.CompleteMFA(ctx, token, "otp", "123456")
	require.ErrorIs(t, err, ErrSSOChallengeInvalid)
	store.consume = true
	_, err = svc.CompleteMFA(ctx, token, "otp", "123456")
	require.ErrorIs(t, err, ErrServiceUnavailable, "verified identity cannot bypass the business account resolver")
	store.session.Config.Application = "kano/other"
	_, err = svc.CompleteMFA(ctx, token, "otp", "123456")
	require.ErrorIs(t, err, ErrSSOChallengeInvalid)
	_, err = svc.CompleteMFA(ctx, "short", "otp", "123456")
	require.ErrorIs(t, err, ErrSSOChallengeInvalid)
	store.err = ErrSSOChallengeBusy
	_, err = svc.CompleteMFA(ctx, token, "otp", "123456")
	require.ErrorIs(t, err, ErrSSOChallengeBusy)
}

func TestSSORegistrationPoliciesAndProviderErrors(t *testing.T) {
	ctx := context.Background()
	svc, provider, _, repo := newSSOTestService()
	require.NoError(t, svc.SendCode(ctx, " USER@Example.com "))
	require.Equal(t, "user@example.com", provider.sentEmail)
	_, err := svc.Register(ctx, " USER@example.com ", "password", "123456", "")
	require.Equal(t, "SSO_ACCOUNT_SYNC_FAILED", infraerrors.Reason(err))
	require.Equal(t, "user", provider.registerDisplay)
	require.True(t, strings.HasPrefix(provider.registerUsername, "u_"))
	require.Len(t, provider.registerUsername, 26)
	provider.err = casdoor.ErrCode
	_, err = svc.Register(ctx, "u@example.com", "password", "wrong", "U")
	require.Equal(t, "SSO_REGISTER_CODE_INVALID", infraerrors.Reason(err))
	_, err = svc.Register(ctx, "u@example.com", "short", "123", "")
	require.Equal(t, "SSO_REGISTER_INVALID", infraerrors.Reason(err))
	require.Error(t, svc.SendCode(ctx, "Name <u@example.com>"))
	require.Error(t, svc.SendCode(ctx, "bad-email"))
	repo.values[SettingKeySSORegistrationEnabled] = "false"
	require.ErrorIs(t, svc.SendCode(ctx, "u@example.com"), ErrRegDisabled)
	_, err = svc.Register(ctx, "u@example.com", "password", "123", "")
	require.ErrorIs(t, err, ErrRegDisabled)
	for _, tc := range []struct {
		err    error
		reason string
	}{
		{casdoor.ErrCredentials, "INVALID_CREDENTIALS"}, {casdoor.ErrFrozen, "SSO_ACCOUNT_LOCKED"}, {casdoor.ErrCaptcha, "SSO_CAPTCHA_REQUIRED"},
		{casdoor.ErrMFACode, "SSO_MFA_CODE_INVALID"}, {casdoor.ErrCode, "SSO_REGISTER_CODE_INVALID"}, {casdoor.ErrEmailExists, infraerrors.Reason(ErrEmailExists)},
		{casdoor.ErrResendWait, "SSO_CODE_RESEND_WAIT"}, {casdoor.ErrIdentity, "SSO_IDENTITY_INVALID"}, {errors.New("secret"), "SSO_PROVIDER_UNAVAILABLE"},
	} {
		require.Equal(t, tc.reason, infraerrors.Reason(ssoProviderError(tc.err)))
	}
	require.NoError(t, ssoProviderError(nil))
}

func TestSSOSettingsDependenciesAndFailClosed(t *testing.T) {
	svc, _, _, repo := newSSOTestService()
	policy, err := svc.settings.GetSSOSettings(context.Background())
	require.NoError(t, err)
	require.Equal(t, "kano/sub2api", policy.Config.Application)
	require.NoError(t, validateSSOConfig(true, policy.Config))
	for _, cfg := range []casdoor.Config{
		{Issuer: "http://auth.example", Organization: "kano", Application: "kano/sub2api"},
		{Issuer: "https://user:pass@auth.example", Organization: "kano", Application: "kano/sub2api"},
		{Issuer: "https://auth.example/path", Organization: "kano", Application: "kano/sub2api"},
		{Issuer: "https://auth.example?secret=1", Organization: "kano", Application: "kano/sub2api"},
		{Issuer: "https://auth.example", Organization: "kano", Application: "other/sub2api"},
	} {
		require.Error(t, validateSSOConfig(true, cfg))
	}
	require.Error(t, validateSSOConfig(false, policy.Config))
	require.Error(t, validateSSOSettings(&SystemSettings{SSOOnlyEnabled: true}))
	require.Error(t, validateSSOSettings(&SystemSettings{SSORegistrationEnabled: true}))
	require.NoError(t, validateSSOSettings(&SystemSettings{}))
	require.NoError(t, validateSSOSettings(&SystemSettings{SSOEnabled: true, OIDCConnectEnabled: true, OIDCConnectIssuerURL: "https://auth.example"}))
	require.Equal(t, "https://auth.example/account", svc.settings.ssoAccountURL(repo.values))
	require.Empty(t, svc.settings.ssoAccountURL(map[string]string{SettingKeyOIDCConnectIssuerURL: "javascript:invalid"}))
	repo.err = errors.New("db offline")
	_, err = svc.settings.GetSSOSettings(context.Background())
	require.ErrorIs(t, err, ErrServiceUnavailable)
	_, err = (*SettingService)(nil).GetSSOSettings(context.Background())
	require.ErrorIs(t, err, ErrServiceUnavailable)
}

func TestSSOBackendModeRejectsNewBusinessAccount(t *testing.T) {
	resetBackendModeTestCache(t)
	_, client := newAuthPendingIdentityServiceTestClient(t)
	settings := NewSettingService(&settingRepoStub{values: map[string]string{SettingKeyBackendModeEnabled: "true"}}, nil)
	auth := &AuthService{entClient: client, userRepo: &userRepoStub{getByEmailErr: ErrUserNotFound}, settingService: settings}
	_, err := auth.ResolveSSOIdentity(context.Background(), "https://auth.example", &casdoor.Identity{Subject: "new", Email: "new@example.com", EmailVerified: true})
	require.Equal(t, "BACKEND_MODE_ADMIN_ONLY", infraerrors.Reason(err))
}

func TestForkCannotInstallUpstreamBinary(t *testing.T) {
	ctx := context.Background()
	for _, svc := range []*UpdateService{{currentVersion: "0.2.11-kano.1"}, {currentVersion: "dev", buildType: "kano-container"}} {
		info, err := svc.CheckUpdate(ctx, true)
		require.NoError(t, err)
		require.False(t, info.HasUpdate)
		require.Equal(t, "kano-container", info.BuildType)
		require.ErrorIs(t, svc.PerformUpdate(ctx), ErrForkUpdateManaged)
		require.ErrorIs(t, svc.Rollback(), ErrForkUpdateManaged)
		require.ErrorIs(t, svc.RollbackToVersion(ctx, "0.2.12"), ErrForkUpdateManaged)
		versions, err := svc.ListRollbackVersions(ctx)
		require.NoError(t, err)
		require.Empty(t, versions)
	}
}
