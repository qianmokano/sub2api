package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/casdoor"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/require"
)

func ssoRequest(path, body string) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, w
}

func TestSSOLocalPasswordAdminRecoveryAndMFAGate(t *testing.T) {
	cache := &oauthPendingFlowTotpCacheStub{}
	h, client := newOAuthPendingFlowTestHandlerWithDependencies(t, oauthPendingFlowTestHandlerOptions{
		settingValues: map[string]string{service.SettingKeySSOOnlyEnabled: "true", service.SettingKeyTotpEnabled: "true"}, totpCache: cache,
	})
	ctx := context.Background()
	hash, err := h.authService.HashPassword("password")
	require.NoError(t, err)
	for _, role := range []string{service.RoleUser, service.RoleAdmin} {
		user, err := client.User.Create().SetEmail(role + "@example.com").SetRole(role).SetPasswordHash(hash).SetStatus(service.StatusActive).
			SetTotpEnabled(true).SetTotpSecretEncrypted("JBSWY3DPEHPK3PXP").Save(ctx)
		require.NoError(t, err)
		c, w := ssoRequest("/api/v1/auth/login?local=1", `{"email":"`+user.Email+`","password":"password"}`)
		h.Login(c)
		if role == service.RoleUser {
			require.Equal(t, http.StatusForbidden, w.Code)
			require.NotContains(t, w.Body.String(), "access_token")
			continue
		}
		require.Equal(t, http.StatusOK, w.Code)
		data := decodeJSONResponseData(t, w)
		require.Equal(t, true, data["requires_2fa"])
		require.NotContains(t, data, "access_token")
		token, ok := data["temp_token"].(string)
		require.True(t, ok)
		code, err := totp.GenerateCode("JBSWY3DPEHPK3PXP", time.Now())
		require.NoError(t, err)
		c, w = ssoRequest("/api/v1/auth/login/2fa", `{"temp_token":"`+token+`","totp_code":"`+code+`"}`)
		h.Login2FA(c)
		require.Equal(t, http.StatusOK, w.Code)
		require.NotEmpty(t, decodeJSONResponseData(t, w)["access_token"])
		c, w = ssoRequest("/api/v1/auth/login/2fa", `{"temp_token":"`+token+`","totp_code":"`+code+`"}`)
		h.Login2FA(c)
		require.Equal(t, http.StatusBadRequest, w.Code)
	}
}

func TestSSOFinishRequiresLocalMFAEvenIfGlobalToggleOff(t *testing.T) {
	cache := &oauthPendingFlowTotpCacheStub{}
	h, client := newOAuthPendingFlowTestHandlerWithDependencies(t, oauthPendingFlowTestHandlerOptions{
		settingValues: map[string]string{service.SettingKeySSOOnlyEnabled: "true", service.SettingKeyTotpEnabled: "false"}, totpCache: cache,
	})
	entity, err := client.User.Create().SetEmail("sso@example.com").SetPasswordHash("hash").SetRole(service.RoleUser).SetStatus(service.StatusActive).
		SetTotpEnabled(true).SetTotpSecretEncrypted("JBSWY3DPEHPK3PXP").Save(context.Background())
	require.NoError(t, err)
	user, err := h.userService.GetByID(context.Background(), entity.ID)
	require.NoError(t, err)
	c, w := ssoRequest("/api/v1/auth/sso/password-login", `{}`)
	h.finishSSOLogin(c, user)
	data := decodeJSONResponseData(t, w)
	require.Equal(t, true, data["requires_2fa"])
	require.NotContains(t, data, "access_token")
	token, ok := data["temp_token"].(string)
	require.True(t, ok)
	require.Equal(t, "sso", cache.loginSessions[token].AuthenticationSource)
	code, err := totp.GenerateCode("JBSWY3DPEHPK3PXP", time.Now())
	require.NoError(t, err)
	c, w = ssoRequest("/api/v1/auth/login/2fa", `{"temp_token":"`+token+`","totp_code":"`+code+`"}`)
	h.Login2FA(c)
	require.Equal(t, http.StatusOK, w.Code)
	require.NotEmpty(t, decodeJSONResponseData(t, w)["access_token"])
	h.totpService = nil
	c, w = ssoRequest("/api/v1/auth/sso/password-login", `{}`)
	h.finishSSOLogin(c, user)
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.NotContains(t, w.Body.String(), "access_token")
}

type handlerSSOStore struct{ session *casdoor.Session }

func (s *handlerSSOStore) Put(_ context.Context, _ string, v *casdoor.Session) error {
	s.session = v
	return nil
}
func (s *handlerSSOStore) Lock(context.Context, string, string) (*casdoor.Session, error) {
	return s.session, nil
}
func (s *handlerSSOStore) Unlock(context.Context, string, string) error                   { return nil }
func (s *handlerSSOStore) Update(context.Context, string, string, *casdoor.Session) error { return nil }
func (s *handlerSSOStore) Consume(context.Context, string, string) (bool, error)          { return true, nil }

func TestSSOPasswordHandlerKeepsProviderCookiesOnServer(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/login", r.URL.Path)
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "provider-secret", Path: "/"})
		_, _ = w.Write([]byte(`{"status":"ok","msg":"NextMfa","data":[{"mfaType":"otp"}]}`))
	}))
	defer srv.Close()
	previous := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = previous })
	h, _ := newOAuthPendingFlowTestHandlerWithDependencies(t, oauthPendingFlowTestHandlerOptions{settingValues: map[string]string{
		service.SettingKeySSOEnabled: "true", service.SettingKeySSOIssuerURL: srv.URL,
	}})
	store := &handlerSSOStore{}
	h.SetSSOService(service.NewSSOService(h.settingSvc, h.authService, store))
	c, w := ssoRequest("/api/v1/auth/sso/password-login", `{"account":"user","password":"secret"}`)
	h.SSOPasswordLogin(c)
	require.Equal(t, http.StatusOK, w.Code)
	data := decodeJSONResponseData(t, w)
	require.Equal(t, true, data["requires_sso_mfa"])
	require.NotContains(t, w.Body.String(), "provider-secret")
	require.NotContains(t, w.Body.String(), "access_token")
	require.NotEmpty(t, store.session.Cookies)
	challenge, ok := data["challenge"].(map[string]any)
	require.True(t, ok)
	challengeToken, ok := challenge["token"].(string)
	require.True(t, ok)
	require.Len(t, challengeToken, 64)
	// Every handler rejects malformed input before contacting the provider.
	for _, fn := range []func(*gin.Context){h.SSOPasswordLogin, h.SSOMFA, h.SSOSendCode, h.SSORegister} {
		c, w = ssoRequest("/api/v1/auth/sso/test", `{}`)
		fn(c)
		require.Equal(t, http.StatusBadRequest, w.Code)
	}
	var payload map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &payload))
	require.NotContains(t, strings.ToLower(w.Body.String()), "password\":")
}
