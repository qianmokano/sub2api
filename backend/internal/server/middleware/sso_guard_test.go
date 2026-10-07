//go:build unit

package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type ssoGuardRepo struct {
	bmSettingRepo
	err error
}

func (r *ssoGuardRepo) GetMultiple(context.Context, []string) (map[string]string, error) {
	return r.values, r.err
}

func TestSSOAuthGuardClosesAlternateEntrypoints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, path := range []string{
		"/register", "/forgot-password", "/reset-password", "/send-verify-code", "/passkey/login/begin", "/passkey/login/finish",
		"/oauth/github/start", "/oauth/google/callback", "/oauth/linuxdo/callback", "/oauth/wechat/callback", "/oauth/dingtalk/start",
		"/oauth/pending/exchange", "/oauth/pending/bind-login", "/oauth/pending/create-account", "/oauth/oidc/bind/start", "/oauth/oidc/complete-registration",
	} {
		t.Run(path, func(t *testing.T) {
			r := gin.New()
			svc := service.NewSettingService(&ssoGuardRepo{bmSettingRepo: bmSettingRepo{values: map[string]string{service.SettingKeySSOOnlyEnabled: "true"}}}, nil)
			r.Use(SSOAuthGuard(svc))
			r.Any("/api/v1/auth"+path, func(c *gin.Context) { c.Status(http.StatusOK) })
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/auth"+path+"?local=1", nil))
			require.Equal(t, http.StatusForbidden, w.Code)
		})
	}
	for _, path := range []string{"/login", "/login/2fa", "/refresh", "/logout", "/me", "/revoke-all-sessions", "/sso/password-login", "/sso/mfa", "/sso/register/send-code", "/sso/register", "/sso/captcha", "/oauth/wechat/payment/start", "/oauth/wechat/payment/callback"} {
		require.True(t, ssoAllowsAuthPath("/api/v1/auth"+path))
	}
	for _, only := range []string{"false", "true"} {
		r := gin.New()
		svc := service.NewSettingService(&ssoGuardRepo{bmSettingRepo: bmSettingRepo{values: map[string]string{service.SettingKeySSOOnlyEnabled: only}}, err: errors.New("db offline")}, nil)
		r.Use(SSOAuthGuard(svc))
		r.POST("/api/v1/auth/sso/password-login", func(c *gin.Context) { c.Status(http.StatusOK) })
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/auth/sso/password-login", nil))
		require.Equal(t, http.StatusServiceUnavailable, w.Code)
	}
}

func TestSSOUserGuardPreservesAdminAndStepUp(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		role, method, path string
		status             int
	}{
		{"user", "PUT", "/password", 403}, {"admin", "PUT", "/password", 200},
		{"user", "POST", "/account-bindings/email", 403}, {"user", "DELETE", "/account-bindings/oidc", 403},
		{"user", "POST", "/auth-identities/bind/start", 403}, {"user", "POST", "/totp/disable", 403}, {"user", "POST", "/totp/setup", 403},
		{"user", "GET", "/totp/status", 200}, {"user", "POST", "/totp/step-up", 200},
		{"user", "POST", "/passkeys/register/begin", 403}, {"user", "PATCH", "/passkeys/1", 403},
		{"user", "GET", "/subscriptions", 200},
	} {
		t.Run(tc.role+tc.method+tc.path, func(t *testing.T) {
			r := gin.New()
			r.Use(func(c *gin.Context) { c.Set(string(ContextKeyUserRole), tc.role); c.Next() })
			svc := service.NewSettingService(&ssoGuardRepo{bmSettingRepo: bmSettingRepo{values: map[string]string{service.SettingKeySSOOnlyEnabled: "true"}}}, nil)
			r.Use(SSOUserGuard(svc))
			r.Handle(tc.method, "/api/v1/user"+tc.path, func(c *gin.Context) { c.Status(http.StatusOK) })
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(tc.method, "/api/v1/user"+tc.path, nil))
			require.Equal(t, tc.status, w.Code)
		})
	}
}

func TestSSOGuardsPreserveAdminRecoveryDuringSettingsFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := service.NewSettingService(&ssoGuardRepo{err: errors.New("db offline")}, nil)
	for _, path := range []string{"/login", "/login/2fa", "/refresh", "/logout"} {
		r := gin.New()
		r.Use(SSOAuthGuard(svc))
		r.POST("/api/v1/auth"+path, func(c *gin.Context) { c.Status(http.StatusOK) })
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/auth"+path, nil))
		require.Equal(t, http.StatusOK, w.Code)
	}
	for _, tc := range []struct {
		role   string
		status int
	}{{"admin", 200}, {"user", 503}} {
		r := gin.New()
		r.Use(func(c *gin.Context) { c.Set(string(ContextKeyUserRole), tc.role); c.Next() })
		r.Use(SSOUserGuard(svc))
		r.PUT("/api/v1/user/password", func(c *gin.Context) { c.Status(http.StatusOK) })
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/v1/user/password", nil))
		require.Equal(t, tc.status, w.Code)
	}
}
