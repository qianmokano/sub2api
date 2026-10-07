package middleware

import (
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// SSOAuthGuard blocks alternate authentication routes, including callbacks started before a policy change.
// Local password and local 2FA handlers enforce the administrator exception themselves.
func SSOAuthGuard(settings *service.SettingService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if settings == nil {
			c.Next()
			return
		}
		policy, err := settings.GetSSOSettings(c.Request.Context())
		if err != nil {
			path := strings.TrimPrefix(c.Request.URL.Path, "/api/v1/auth")
			if path == "/login" || path == "/login/2fa" || path == "/refresh" || path == "/logout" {
				c.Next()
				return
			}
			response.ErrorFrom(c, err)
			c.Abort()
			return
		}
		if !policy.OnlyEnabled || ssoAllowsAuthPath(c.Request.URL.Path) {
			c.Next()
			return
		}
		response.ErrorFrom(c, service.ErrSSOOnly)
		c.Abort()
	}
}

func ssoAllowsAuthPath(path string) bool {
	path = strings.TrimPrefix(path, "/api/v1/auth")
	switch path {
	case "/login", "/login/2fa", "/refresh", "/logout", "/me", "/revoke-all-sessions":
		return true
	case "/sso/captcha", "/sso/password-login", "/sso/mfa", "/sso/register/send-code", "/sso/register":
		return true
	case "/oauth/wechat/payment/start", "/oauth/wechat/payment/callback":
		return true
	default:
		return false
	}
}

// SSOUserGuard directs credential management to Casdoor while preserving admin recovery.
func SSOUserGuard(settings *service.SettingService) gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		protected := path == "/api/v1/user/password" || strings.HasPrefix(path, "/api/v1/user/account-bindings/") ||
			path == "/api/v1/user/auth-identities/bind/start" || strings.HasPrefix(path, "/api/v1/user/totp/") ||
			strings.HasPrefix(path, "/api/v1/user/passkeys")
		if !protected || settings == nil {
			c.Next()
			return
		}
		role, _ := GetUserRoleFromContext(c)
		if role == service.RoleAdmin {
			c.Next()
			return
		}
		policy, err := settings.GetSSOSettings(c.Request.Context())
		if err != nil {
			response.ErrorFrom(c, err)
			c.Abort()
			return
		}
		// Existing local TOTP is still needed for step-up and authentication.
		readOnlyTOTP := (c.Request.Method == "GET" && strings.HasPrefix(path, "/api/v1/user/totp/")) || path == "/api/v1/user/totp/step-up"
		if !policy.OnlyEnabled || role == service.RoleAdmin || readOnlyTOTP {
			c.Next()
			return
		}
		response.ErrorFrom(c, service.ErrSSOOnly)
		c.Abort()
	}
}
