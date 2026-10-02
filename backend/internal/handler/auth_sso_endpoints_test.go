package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/casdoor"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestSSOEndpointsCompleteAuthenticationWithoutLeakingProviderState(t *testing.T) {
	mode := "mfa"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/login":
			var payload map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
			if payload["passcode"] == "wrong" {
				_, _ = w.Write([]byte(`{"status":"error","msg":"invalid mfa code"}`))
				return
			}
			if mode == "unavailable" {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "idp-cookie", Path: "/"})
			if mode == "mfa" {
				_, _ = w.Write([]byte(`{"status":"ok","msg":"NextMfa","data":[{"mfaType":"otp"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/api/send-verification-code", "/api/signup":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/api/get-account":
			_, _ = w.Write([]byte(`{"status":"ok","data":{"id":"bound-sub","owner":"kano","email":"bound@example.com","emailVerified":true,"displayName":"Passport nickname","avatar":"https://auth.example/password-avatar.png"}}`))
		default:
			t.Fatalf("unexpected IdP path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	previous := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = previous })
	h, client := newOAuthPendingFlowTestHandlerWithDependencies(t, oauthPendingFlowTestHandlerOptions{settingValues: map[string]string{
		service.SettingKeySSOEnabled: "true", service.SettingKeySSOOnlyEnabled: "true", service.SettingKeySSORegistrationEnabled: "true", service.SettingKeyOIDCConnectEnabled: "true", service.SettingKeyOIDCConnectIssuerURL: srv.URL,
	}})
	entity, err := client.User.Create().SetEmail("bound@example.com").SetPasswordHash("hash").SetRole(service.RoleUser).SetStatus(service.StatusActive).Save(context.Background())
	require.NoError(t, err)
	_, err = client.AuthIdentity.Create().SetUserID(entity.ID).SetProviderType("oidc").SetProviderKey(srv.URL).SetProviderSubject("bound-sub").Save(context.Background())
	require.NoError(t, err)
	store := &handlerSSOStore{}
	h.SetSSOService(service.NewSSOService(h.settingSvc, h.authService, store))
	c, w := ssoRequest("/api/v1/auth/sso/password-login", `{"account":"user","password":"password"}`)
	h.SSOPasswordLogin(c)
	require.Equal(t, http.StatusOK, w.Code)
	challengeData, ok := decodeJSONResponseData(t, w)["challenge"].(map[string]any)
	require.True(t, ok)
	challenge, ok := challengeData["token"].(string)
	require.True(t, ok)
	c, w = ssoRequest("/api/v1/auth/sso/mfa", `{"challenge":"`+challenge+`","mfa_type":"otp","passcode":"wrong"}`)
	h.SSOMFA(c)
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.NotContains(t, w.Body.String(), "access_token")
	mode = "success"
	c, w = ssoRequest("/api/v1/auth/sso/mfa", `{"challenge":"`+challenge+`","mfa_type":"otp","passcode":"123456"}`)
	h.SSOMFA(c)
	require.Equal(t, http.StatusOK, w.Code)
	require.NotEmpty(t, decodeJSONResponseData(t, w)["access_token"])
	require.NotContains(t, w.Body.String(), "idp-cookie")
	c, w = ssoRequest("/api/v1/auth/sso/password-login", `{"account":"user","password":"password"}`)
	h.SSOPasswordLogin(c)
	require.Equal(t, http.StatusOK, w.Code)
	require.NotEmpty(t, decodeJSONResponseData(t, w)["refresh_token"])
	profile, err := h.userService.GetProfile(context.Background(), entity.ID)
	require.NoError(t, err)
	require.Equal(t, "Passport nickname", profile.Username)
	require.Equal(t, "https://auth.example/password-avatar.png", profile.AvatarURL)
	c, w = ssoRequest("/api/v1/auth/sso/register/send-code", `{"email":"new@example.com"}`)
	h.SSOSendCode(c)
	require.Equal(t, http.StatusOK, w.Code)
	require.EqualValues(t, 60, decodeJSONResponseData(t, w)["countdown"])
	c, w = ssoRequest("/api/v1/auth/sso/register", `{"email":"new@example.com","password":"password","code":"123456"}`)
	h.SSORegister(c)
	require.Equal(t, http.StatusOK, w.Code)
	require.NotEmpty(t, decodeJSONResponseData(t, w)["access_token"])
	mode = "unavailable"
	c, w = ssoRequest("/api/v1/auth/sso/password-login", `{"account":"user","password":"password"}`)
	h.SSOPasswordLogin(c)
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	h.ssoService = nil
	c, w = ssoRequest("/api/v1/auth/sso/password-login", `{"account":"user","password":"password"}`)
	h.SSOPasswordLogin(c)
	require.Equal(t, http.StatusForbidden, w.Code)
	c, w = ssoRequest("/api/v1/auth/sso/mfa", `{"challenge":"`+strings.Repeat("a", 64)+`","mfa_type":"otp","passcode":"123456"}`)
	h.SSOMFA(c)
	require.Equal(t, http.StatusForbidden, w.Code)
}

func TestSSOOIDCCallbackNoMFAIssuesTokensAfterSubjectResolution(t *testing.T) {
	h, client := newOAuthPendingFlowTestHandlerWithDependencies(t, oauthPendingFlowTestHandlerOptions{settingValues: map[string]string{
		service.SettingKeySSOEnabled: "true", service.SettingKeySSOOnlyEnabled: "true", service.SettingKeyOIDCConnectEnabled: "true", service.SettingKeyOIDCConnectIssuerURL: "https://auth.example",
	}})
	entity, err := client.User.Create().SetEmail("bound@example.com").SetPasswordHash("hash").SetRole(service.RoleUser).SetStatus(service.StatusActive).Save(context.Background())
	require.NoError(t, err)
	_, err = client.AuthIdentity.Create().SetUserID(entity.ID).SetProviderType("oidc").SetProviderKey("https://auth.example").SetProviderSubject("bound-sub").Save(context.Background())
	require.NoError(t, err)
	c, w := ssoRequest("/api/v1/auth/oauth/oidc/callback", `{}`)
	avatar := "https://auth.example/avatar.png"
	require.True(t, h.trySSOOIDCCallback(c, "/auth/oidc/callback", "/keys", oauthIntentLogin, "https://auth.example", &casdoor.Identity{Subject: "bound-sub", Email: "changed@example.com", DisplayName: "New nickname", AvatarURL: &avatar}))
	stored, err := client.User.Get(context.Background(), entity.ID)
	require.NoError(t, err)
	require.Equal(t, entity.Email, stored.Email)
	require.Equal(t, "New nickname", stored.Username)
	profile, err := h.userService.GetProfile(context.Background(), entity.ID)
	require.NoError(t, err)
	require.Equal(t, avatar, profile.AvatarURL)
	location, err := url.Parse(w.Header().Get("Location"))
	require.NoError(t, err)
	fragment, err := url.ParseQuery(location.Fragment)
	require.NoError(t, err)
	require.NotEmpty(t, fragment.Get("access_token"))
	require.NotEmpty(t, fragment.Get("refresh_token"))
	require.Equal(t, "/keys", fragment.Get("redirect"))
	c, w = ssoRequest("/api/v1/auth/oauth/oidc/callback", `{}`)
	require.True(t, h.trySSOOIDCCallback(c, "/auth/oidc/callback", "/keys", oauthIntentLogin, "https://wrong.example", &casdoor.Identity{Subject: "bound-sub"}))
	require.NotContains(t, w.Header().Get("Location"), "access_token")
	_, err = client.User.UpdateOneID(entity.ID).SetStatus("disabled").Save(context.Background())
	require.NoError(t, err)
	c, w = ssoRequest("/api/v1/auth/oauth/oidc/callback", `{}`)
	require.True(t, h.trySSOOIDCCallback(c, "/auth/oidc/callback", "/keys", oauthIntentLogin, "https://auth.example", &casdoor.Identity{Subject: "bound-sub"}))
	require.NotContains(t, w.Header().Get("Location"), "access_token")
	c, w = ssoRequest("/api/v1/auth/sso/password-login", `{}`)
	h.finishSSOLogin(c, &service.User{Status: "disabled"})
	require.Equal(t, http.StatusForbidden, w.Code)
}
