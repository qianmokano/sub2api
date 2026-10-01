//go:build unit

package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/ent/authidentity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/casdoor"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestSSOExistingIdentityPreservesAccount(t *testing.T) {
	svc, repo, client := newAuthServiceWithEnt(t, nil, nil)
	ctx := context.Background()
	user := &service.User{Email: "old@example.com", PasswordHash: "hash", Role: service.RoleUser, Status: service.StatusActive, Balance: 42, Concurrency: 7, Username: "Original"}
	require.NoError(t, repo.Create(ctx, user))
	_, err := client.AuthIdentity.Create().SetUserID(user.ID).SetProviderType("oidc").SetProviderKey("https://auth.example").SetProviderSubject("bound-sub").Save(ctx)
	require.NoError(t, err)
	resolved, err := svc.ResolveSSOIdentity(ctx, "https://auth.example", &casdoor.Identity{Subject: "bound-sub", Email: "changed@example.com"})
	require.NoError(t, err)
	require.Equal(t, user.ID, resolved.ID)
	require.Equal(t, "old@example.com", resolved.Email)
	require.Equal(t, 42.0, resolved.Balance)
	require.Equal(t, 7, resolved.Concurrency)
	_, err = client.User.UpdateOneID(user.ID).SetStatus("disabled").Save(ctx)
	require.NoError(t, err)
	_, err = svc.ResolveSSOIdentity(ctx, "https://auth.example", &casdoor.Identity{Subject: "bound-sub"})
	require.ErrorIs(t, err, service.ErrUserNotActive)
}

func TestSSOProvisioningIndependentOfLocalRegistrationAndIdempotent(t *testing.T) {
	svc, _, client := newAuthServiceWithEnt(t, map[string]string{
		service.SettingKeyRegistrationEnabled: "false", service.SettingKeyInvitationCodeEnabled: "true", service.SettingKeyRegistrationEmailSuffixWhitelist: `["blocked.invalid"]`,
	}, nil)
	ctx := context.Background()
	identity := &casdoor.Identity{Subject: "new-sub", Email: "new@example.com", EmailVerified: true, DisplayName: strings.Repeat("名", 110)}
	user, err := svc.ResolveSSOIdentity(ctx, "https://auth.example", identity)
	require.NoError(t, err)
	require.Equal(t, "oidc", user.SignupSource)
	require.LessOrEqual(t, len(user.Username), 100)
	require.Len(t, []rune(user.Username), 33)
	require.Equal(t, 3.5, user.Balance)
	require.Equal(t, 2, user.Concurrency)
	stored, err := client.User.Get(ctx, user.ID)
	require.NoError(t, err)
	require.NotEmpty(t, stored.PasswordHash)
	repeated, err := svc.ResolveSSOIdentity(ctx, "https://auth.example", identity)
	require.NoError(t, err)
	require.Equal(t, user.ID, repeated.ID)
	count, err := client.User.Query().Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	count, err = client.AuthIdentity.Query().Where(authidentity.ProviderTypeEQ("oidc")).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	_, err = svc.ResolveSSOIdentity(ctx, "https://auth.example", &casdoor.Identity{Subject: "other-sub", Email: identity.Email, EmailVerified: true})
	require.ErrorIs(t, err, service.ErrSSOIdentityConflict)
}

func TestSSORejectsUnverifiedConflictAndRollsBackFailedGrant(t *testing.T) {
	grant := &flakyAuthIdentityDefaultSubAssignerStub{failuresRemaining: 1}
	svc, repo, client := newAuthServiceWithEnt(t, map[string]string{service.SettingKeyDefaultSubscriptions: `[{"group_id":1,"validity_days":30}]`}, grant)
	ctx := context.Background()
	_, err := svc.ResolveSSOIdentity(ctx, "https://auth.example", &casdoor.Identity{Subject: "unverified", Email: "u@example.com"})
	require.Error(t, err)
	local := &service.User{Email: "existing@example.com", PasswordHash: "hash", Role: service.RoleUser, Status: service.StatusActive}
	require.NoError(t, repo.Create(ctx, local))
	_, err = svc.ResolveSSOIdentity(ctx, "https://auth.example", &casdoor.Identity{Subject: "other", Email: local.Email, EmailVerified: true})
	require.ErrorIs(t, err, service.ErrSSOIdentityConflict)
	identity := &casdoor.Identity{Subject: "new", Email: "new@example.com", EmailVerified: true}
	_, err = svc.ResolveSSOIdentity(ctx, "https://auth.example", identity)
	require.ErrorIs(t, err, service.ErrServiceUnavailable)
	count, err := client.User.Query().Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, count, "failed subscription provisioning must roll back new account")
	user, err := svc.ResolveSSOIdentity(ctx, "https://auth.example", identity)
	require.NoError(t, err)
	_, err = svc.ResolveSSOIdentity(ctx, "https://auth.example", identity)
	require.NoError(t, err)
	require.Len(t, grant.calls, 2, "retry grants once; subsequent login does not grant again")
	require.Positive(t, user.ID)
}

func TestSSOLocalPasswordVerification(t *testing.T) {
	svc, repo, _ := newAuthServiceWithEnt(t, nil, nil)
	ctx := context.Background()
	hash, err := svc.HashPassword("admin-password")
	require.NoError(t, err)
	admin := &service.User{Email: "admin@example.com", PasswordHash: hash, Role: service.RoleAdmin, Status: service.StatusActive}
	require.NoError(t, repo.Create(ctx, admin))
	user, err := svc.AuthenticatePassword(ctx, admin.Email, "admin-password")
	require.NoError(t, err)
	require.Equal(t, admin.ID, user.ID)
	_, err = svc.AuthenticatePassword(ctx, admin.Email, "wrong")
	require.ErrorIs(t, err, service.ErrInvalidCredentials)
	_, err = svc.AuthenticatePassword(ctx, "missing@example.com", "pw")
	require.ErrorIs(t, err, service.ErrInvalidCredentials)
	_, err = svc.ResolveSSOIdentity(ctx, "", nil)
	require.ErrorIs(t, err, service.ErrServiceUnavailable)
}
