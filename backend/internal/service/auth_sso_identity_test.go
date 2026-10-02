//go:build unit

package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/authidentity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/casdoor"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestSSOProfileSyncPreservesBusinessAndStableIdentity(t *testing.T) {
	svc, repo, client := newAuthServiceWithEnt(t, map[string]string{service.SettingKeySSOOnlyEnabled: "true"}, nil)
	ctx := context.Background()
	user := &service.User{Email: "original@example.com", PasswordHash: "local-hash", Role: service.RoleUser,
		Status: service.StatusActive, Username: "Old name", Balance: 42, Concurrency: 7, RPMLimit: 30, BalanceNotifyEnabled: true}
	require.NoError(t, repo.Create(ctx, user))
	bound, err := client.AuthIdentity.Create().SetUserID(user.ID).SetProviderType("oidc").
		SetProviderKey("https://auth.example").SetProviderSubject("stable-subject").
		SetMetadata(map[string]any{"email": user.Email, "custom": "retained"}).Save(ctx)
	require.NoError(t, err)
	avatarURL := "https://auth.example/files/avatar.png"
	identity := &casdoor.Identity{Subject: "stable-subject", Email: "different@example.com", Username: "account-name",
		DisplayName: "New nickname", AvatarURL: &avatarURL}
	for range 2 {
		resolved, err := svc.ResolveSSOIdentity(ctx, "https://auth.example", identity)
		require.NoError(t, err)
		require.Equal(t, user.ID, resolved.ID)
		require.Equal(t, "New nickname", resolved.Username)
		require.Equal(t, avatarURL, resolved.AvatarURL)
	}
	stored, err := client.User.Get(ctx, user.ID)
	require.NoError(t, err)
	require.Equal(t, user.Email, stored.Email)
	require.Equal(t, user.PasswordHash, stored.PasswordHash)
	require.Equal(t, 42.0, stored.Balance)
	require.Equal(t, 7, stored.Concurrency)
	require.Equal(t, 30, stored.RpmLimit)
	require.True(t, stored.BalanceNotifyEnabled)
	require.Equal(t, service.RoleUser, stored.Role)
	avatar, err := repo.GetUserAvatar(ctx, user.ID)
	require.NoError(t, err)
	require.Equal(t, avatarURL, avatar.URL)
	bound, err = client.AuthIdentity.Get(ctx, bound.ID)
	require.NoError(t, err)
	require.Equal(t, "stable-subject", bound.ProviderSubject)
	require.Equal(t, "retained", bound.Metadata["custom"])
	require.Equal(t, "New nickname", bound.Metadata["display_name"])
	require.Equal(t, avatarURL, bound.Metadata["avatar_url"])
}

func TestSSOProfileSyncHandlesMissingInvalidAndRemovedAvatar(t *testing.T) {
	svc, repo, client := newAuthServiceWithEnt(t, map[string]string{service.SettingKeySSOOnlyEnabled: "true"}, nil)
	ctx := context.Background()
	avatarURL := "https://auth.example/old.png"
	user, err := svc.ResolveSSOIdentity(ctx, "https://auth.example", &casdoor.Identity{
		Subject: "new-user", Email: "new@example.com", EmailVerified: true, DisplayName: "Initial", AvatarURL: &avatarURL,
	})
	require.NoError(t, err)
	require.Equal(t, avatarURL, user.AvatarURL, "new accounts also adopt the Passport avatar")
	invalidAvatar := "javascript:alert(1)"
	for _, picture := range []*string{nil, &invalidAvatar} {
		resolved, err := svc.ResolveSSOIdentity(ctx, "https://auth.example", &casdoor.Identity{
			Subject: "new-user", DisplayName: strings.Repeat("名", 40), AvatarURL: picture,
		})
		require.NoError(t, err)
		require.True(t, utf8.ValidString(resolved.Username))
		require.Len(t, []rune(resolved.Username), 33)
		avatar, err := repo.GetUserAvatar(ctx, user.ID)
		require.NoError(t, err)
		require.Equal(t, avatarURL, avatar.URL)
	}
	empty := ""
	resolved, err := svc.ResolveSSOIdentity(ctx, "https://auth.example", &casdoor.Identity{Subject: "new-user", AvatarURL: &empty})
	require.NoError(t, err)
	require.Empty(t, resolved.AvatarURL)
	require.NotEmpty(t, resolved.Username, "an absent name does not clear the local display name")
	avatar, err := repo.GetUserAvatar(ctx, user.ID)
	require.NoError(t, err)
	require.Nil(t, avatar)
	bound, err := client.AuthIdentity.Query().Where(authidentity.UserIDEQ(user.ID), authidentity.ProviderTypeEQ("oidc")).Only(ctx)
	require.NoError(t, err)
	require.Equal(t, "", bound.Metadata["avatar_url"])
}

func TestSSOProfileSyncPreservesAdminAndMixedModeEditing(t *testing.T) {
	for _, tc := range []struct{ role, only string }{{service.RoleAdmin, "true"}, {service.RoleUser, "false"}} {
		t.Run(tc.role+tc.only, func(t *testing.T) {
			svc, repo, client := newAuthServiceWithEnt(t, map[string]string{service.SettingKeySSOOnlyEnabled: tc.only}, nil)
			ctx := context.Background()
			user := &service.User{Email: "local@example.com", PasswordHash: "hash", Role: tc.role, Status: service.StatusActive, Username: "Local name"}
			require.NoError(t, repo.Create(ctx, user))
			_, err := client.AuthIdentity.Create().SetUserID(user.ID).SetProviderType("oidc").SetProviderKey("https://auth.example").SetProviderSubject("local-sub").Save(ctx)
			require.NoError(t, err)
			avatar := "https://auth.example/passport.png"
			resolved, err := svc.ResolveSSOIdentity(ctx, "https://auth.example", &casdoor.Identity{Subject: "local-sub", DisplayName: "Passport name", AvatarURL: &avatar})
			require.NoError(t, err)
			require.Equal(t, "Local name", resolved.Username)
			storedAvatar, err := repo.GetUserAvatar(ctx, user.ID)
			require.NoError(t, err)
			require.Nil(t, storedAvatar)
		})
	}
}

func TestSSOProfileSyncRollsBackProfileOnMetadataFailure(t *testing.T) {
	svc, repo, client := newAuthServiceWithEnt(t, map[string]string{service.SettingKeySSOOnlyEnabled: "true"}, nil)
	ctx := context.Background()
	user := &service.User{Email: "rollback@example.com", PasswordHash: "hash", Role: service.RoleUser, Status: service.StatusActive, Username: "Original"}
	require.NoError(t, repo.Create(ctx, user))
	_, err := client.AuthIdentity.Create().SetUserID(user.ID).SetProviderType("oidc").SetProviderKey("https://auth.example").SetProviderSubject("rollback-sub").Save(ctx)
	require.NoError(t, err)
	client.AuthIdentity.Use(func(next dbent.Mutator) dbent.Mutator {
		return dbent.MutateFunc(func(ctx context.Context, m dbent.Mutation) (dbent.Value, error) {
			if m.Op().Is(dbent.OpUpdateOne) {
				return nil, errors.New("metadata write failed")
			}
			return next.Mutate(ctx, m)
		})
	})
	avatar := "https://auth.example/new.png"
	_, err = svc.ResolveSSOIdentity(ctx, "https://auth.example", &casdoor.Identity{Subject: "rollback-sub", DisplayName: "Changed", AvatarURL: &avatar})
	require.ErrorIs(t, err, service.ErrServiceUnavailable)
	stored, err := client.User.Get(ctx, user.ID)
	require.NoError(t, err)
	require.Equal(t, "Original", stored.Username)
	storedAvatar, err := repo.GetUserAvatar(ctx, user.ID)
	require.NoError(t, err)
	require.Nil(t, storedAvatar, "avatar and nickname roll back together")
}

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
