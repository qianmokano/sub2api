package repository

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/casdoor"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestSSOChallengeRetryExpiryAndSingleUse(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := NewSSOChallengeCache(client)
	ctx := context.Background()
	session := &casdoor.Session{Config: casdoor.Config{Issuer: "https://auth.example"}, Cookies: []*http.Cookie{{Name: "session", Value: "pending"}}, Methods: []casdoor.MFAMethod{{Type: "otp"}}}
	require.NoError(t, store.Put(ctx, "token", session))
	require.ErrorIs(t, store.Put(ctx, "token", session), service.ErrSSOChallengeInvalid)
	loaded, err := store.Lock(ctx, "token", "lease")
	require.NoError(t, err)
	require.Equal(t, session, loaded)
	_, err = store.Lock(ctx, "token", "other")
	require.ErrorIs(t, err, service.ErrSSOChallengeBusy)
	require.NoError(t, store.Unlock(ctx, "token", "other"))
	consumed, err := store.Consume(ctx, "token", "other")
	require.NoError(t, err)
	require.False(t, consumed)
	loaded.Cookies[0].Value = "rotated"
	require.ErrorIs(t, store.Update(ctx, "token", "other", loaded), service.ErrSSOChallengeInvalid)
	server.FastForward(time.Minute)
	// Expired leases cannot update or consume a still-valid challenge.
	consumed, err = store.Consume(ctx, "token", "lease")
	require.NoError(t, err)
	require.False(t, consumed)
	loaded, err = store.Lock(ctx, "token", "retry")
	require.NoError(t, err)
	loaded.Cookies[0].Value = "rotated"
	require.NoError(t, store.Update(ctx, "token", "retry", loaded))
	require.NoError(t, store.Unlock(ctx, "token", "retry"))
	loaded, err = store.Lock(ctx, "token", "success")
	require.NoError(t, err)
	require.Equal(t, "rotated", loaded.Cookies[0].Value)
	consumed, err = store.Consume(ctx, "token", "success")
	require.NoError(t, err)
	require.True(t, consumed)
	consumed, err = store.Consume(ctx, "token", "success")
	require.NoError(t, err)
	require.False(t, consumed)
	_, err = store.Lock(ctx, "token", "replay")
	require.ErrorIs(t, err, service.ErrSSOChallengeInvalid)
	require.NoError(t, store.Put(ctx, "expire", session))
	server.FastForward(10 * time.Minute)
	_, err = store.Lock(ctx, "expire", "lease")
	require.ErrorIs(t, err, service.ErrSSOChallengeInvalid)
}

func TestSSOChallengeAttemptLimitAndConcurrentConsumption(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := NewSSOChallengeCache(client)
	ctx := context.Background()
	require.NoError(t, store.Put(ctx, "attempts", &casdoor.Session{}))
	for i := 0; i < 10; i++ {
		_, err := store.Lock(ctx, "attempts", "lease")
		require.NoError(t, err)
		require.NoError(t, store.Unlock(ctx, "attempts", "lease"))
	}
	_, err := store.Lock(ctx, "attempts", "lease")
	require.ErrorIs(t, err, service.ErrSSOChallengeInvalid)
	require.NoError(t, store.Put(ctx, "once", &casdoor.Session{}))
	_, err = store.Lock(ctx, "once", "lease")
	require.NoError(t, err)
	var successful atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := store.Consume(ctx, "once", "lease")
			if err == nil && ok {
				successful.Add(1)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, int32(1), successful.Load())
	// Existing API-site TOTP challenges are consumed atomically too.
	cache := NewTotpCache(client)
	require.NoError(t, cache.SetLoginSession(ctx, "totp", &service.TotpLoginSession{AuthenticationSource: "sso"}, time.Minute))
	ok, err := cache.ConsumeLoginSession(ctx, "totp")
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = cache.ConsumeLoginSession(ctx, "totp")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestSSOChallengeRedisFailureIsClosed(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = client.Close() })
	server.Close()
	store := NewSSOChallengeCache(client)
	require.ErrorIs(t, store.Put(context.Background(), "token", &casdoor.Session{}), service.ErrServiceUnavailable)
	_, err := store.Lock(context.Background(), "token", "lease")
	require.ErrorIs(t, err, service.ErrServiceUnavailable)
	require.ErrorIs(t, store.Update(context.Background(), "token", "lease", &casdoor.Session{}), service.ErrServiceUnavailable)
}
