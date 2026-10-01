//go:build integration

package repository

import (
	"context"
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
