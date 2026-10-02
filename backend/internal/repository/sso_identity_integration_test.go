//go:build integration

package repository

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent/authidentity"
	"github.com/Wei-Shaw/sub2api/ent/user"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/casdoor"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type ssoProfileSettings struct{ service.SettingRepository }

func (ssoProfileSettings) GetMultiple(context.Context, []string) (map[string]string, error) {
	return map[string]string{service.SettingKeySSOOnlyEnabled: "true"}, nil
}

type failingSSOAvatarRepo struct{ service.UserRepository }

func (r failingSSOAvatarRepo) UpsertUserAvatar(ctx context.Context, id int64, input service.UpsertUserAvatarInput) (*service.UserAvatar, error) {
	if _, err := r.UserRepository.UpsertUserAvatar(ctx, id, input); err != nil {
		return nil, err
	}
	return nil, errors.New("simulated failure after writing the avatar")
}

func TestSSOProfileSyncUsesOnePostgresTransaction(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	repo := NewUserRepository(integrationEntClient, integrationDB)
	settings := service.NewSettingService(ssoProfileSettings{}, nil)
	cfg := &config.Config{}
	svc := service.NewAuthService(integrationEntClient, repo, nil, nil, cfg, settings, nil, nil, nil, nil, nil, nil, nil)
	user := &service.User{Email: fmt.Sprintf("sso-profile-%d@example.com", time.Now().UnixNano()),
		PasswordHash: "hash", Role: service.RoleUser, Status: service.StatusActive, Username: "Original", Balance: 42, Concurrency: 7}
	require.NoError(t, repo.Create(ctx, user))
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM users WHERE id = $1", user.ID)
	})
	subject := fmt.Sprintf("profile-%d", user.ID)
	_, err := integrationEntClient.AuthIdentity.Create().SetUserID(user.ID).SetProviderType("oidc").
		SetProviderKey("https://auth.example").SetProviderSubject(subject).Save(ctx)
	require.NoError(t, err)
	avatar := "https://auth.example/profile.png"
	identity := &casdoor.Identity{Subject: subject, DisplayName: "Passport nickname", AvatarURL: &avatar}
	resolved, err := svc.ResolveSSOIdentity(ctx, "https://auth.example", identity)
	require.NoError(t, err)
	require.Equal(t, user.ID, resolved.ID)
	require.Equal(t, "Passport nickname", resolved.Username)
	require.Equal(t, avatar, resolved.AvatarURL)
	stored, err := repo.GetByID(ctx, user.ID)
	require.NoError(t, err)
	require.Equal(t, 42.0, stored.Balance)
	require.Equal(t, 7, stored.Concurrency)
	require.Equal(t, user.Email, stored.Email)
	failing := service.NewAuthService(integrationEntClient, failingSSOAvatarRepo{repo}, nil, nil, cfg, settings, nil, nil, nil, nil, nil, nil, nil)
	nextAvatar := "https://auth.example/rolled-back.png"
	_, err = failing.ResolveSSOIdentity(ctx, "https://auth.example", &casdoor.Identity{Subject: subject, DisplayName: "Must roll back", AvatarURL: &nextAvatar})
	require.ErrorIs(t, err, service.ErrServiceUnavailable)
	stored, err = repo.GetByID(ctx, user.ID)
	require.NoError(t, err)
	require.Equal(t, "Passport nickname", stored.Username)
	storedAvatar, err := repo.GetUserAvatar(ctx, user.ID)
	require.NoError(t, err)
	require.Equal(t, avatar, storedAvatar.URL)
	blank := ""
	_, err = svc.ResolveSSOIdentity(ctx, "https://auth.example", &casdoor.Identity{Subject: subject, AvatarURL: &blank})
	require.NoError(t, err)
	storedAvatar, err = repo.GetUserAvatar(ctx, user.ID)
	require.NoError(t, err)
	require.Nil(t, storedAvatar)
}

func TestSSOConcurrentProvisioningUsesOneAccountAndOneInitialGrant(t *testing.T) {
	ctx := context.Background()
	repo := NewUserRepository(integrationEntClient, integrationDB)
	cfg := &config.Config{Default: config.DefaultConfig{UserBalance: 9.5, UserConcurrency: 2}}
	svc := service.NewAuthService(integrationEntClient, repo, nil, nil, cfg, nil, nil, nil, nil, nil, nil, nil, nil)
	identity := &casdoor.Identity{Subject: fmt.Sprintf("sso-race-%d", time.Now().UnixNano()), Email: fmt.Sprintf("sso-race-%d@example.com", time.Now().UnixNano()), EmailVerified: true}
	t.Cleanup(func() { _, _ = integrationDB.ExecContext(ctx, "DELETE FROM users WHERE email = $1", identity.Email) })
	start := make(chan struct{})
	results := make(chan *service.User, 12)
	errs := make(chan error, 12)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resolved, err := svc.ResolveSSOIdentity(ctx, "https://auth.example", identity)
			results <- resolved
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	var accountID int64
	for resolved := range results {
		require.NotNil(t, resolved)
		if accountID == 0 {
			accountID = resolved.ID
		}
		require.Equal(t, accountID, resolved.ID)
		require.Equal(t, 9.5, resolved.Balance)
	}
	count, err := integrationEntClient.User.Query().Where(user.EmailEQ(identity.Email)).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	count, err = integrationEntClient.AuthIdentity.Query().Where(authidentity.ProviderTypeEQ("oidc"), authidentity.ProviderSubjectEQ(identity.Subject)).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	_, err = svc.ResolveSSOIdentity(ctx, "https://auth.example", &casdoor.Identity{Subject: "unrelated-subject", Email: identity.Email, EmailVerified: true})
	require.ErrorIs(t, err, service.ErrSSOIdentityConflict)
}
