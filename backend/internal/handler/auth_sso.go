package handler

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/casdoor"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// SetSSOService attaches the provider without changing the existing handler constructor.
func (h *AuthHandler) SetSSOService(sso *service.SSOService) { h.ssoService = sso }

type ssoCaptchaRequest struct {
	TurnstileToken        string `json:"turnstile_token"`
	TencentCaptchaTicket  string `json:"tencent_captcha_ticket"`
	TencentCaptchaRandstr string `json:"tencent_captcha_randstr"`
}

func (h *AuthHandler) verifySSOCaptcha(c *gin.Context, req ssoCaptchaRequest) bool {
	if h.ssoService == nil {
		response.ErrorFrom(c, service.ErrSSODisabled)
		return false
	}
	if err := h.authService.VerifyCaptcha(c.Request.Context(), captchaProof(req.TurnstileToken, req.TencentCaptchaTicket, req.TencentCaptchaRandstr), ip.GetClientIP(c)); err != nil {
		response.ErrorFrom(c, err)
		return false
	}
	return true
}

func (h *AuthHandler) SSOPasswordLogin(c *gin.Context) {
	var req struct {
		ssoCaptchaRequest
		Account  string `json:"account" binding:"required,max=255"`
		Password string `json:"password" binding:"required,max=4096"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Check the sign-in fields")
		return
	}
	if !h.verifySSOCaptcha(c, req.ssoCaptchaRequest) {
		return
	}
	result, err := h.ssoService.Login(c.Request.Context(), req.Account, req.Password)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if result.Challenge != nil {
		response.Success(c, gin.H{"requires_sso_mfa": true, "challenge": result.Challenge})
		return
	}
	h.finishSSOLogin(c, result.User)
}

func (h *AuthHandler) SSOMFA(c *gin.Context) {
	var req struct {
		Challenge string `json:"challenge" binding:"required,len=64"`
		Method    string `json:"mfa_type" binding:"required,max=64"`
		Passcode  string `json:"passcode" binding:"required,max=128"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Check the verification fields")
		return
	}
	if h.ssoService == nil {
		response.ErrorFrom(c, service.ErrSSODisabled)
		return
	}
	user, err := h.ssoService.CompleteMFA(c.Request.Context(), req.Challenge, req.Method, req.Passcode)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	h.finishSSOLogin(c, user)
}

func (h *AuthHandler) SSOSendCode(c *gin.Context) {
	var req struct {
		ssoCaptchaRequest
		Email string `json:"email" binding:"required,email,max=255"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Enter a valid email address")
		return
	}
	if !h.verifySSOCaptcha(c, req.ssoCaptchaRequest) {
		return
	}
	if err := h.ensureBackendModeAllowsNewUserLogin(c.Request.Context()); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if err := h.ssoService.SendCode(c.Request.Context(), req.Email); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, SendVerifyCodeResponse{Message: "Verification code sent", Countdown: 60})
}

func (h *AuthHandler) SSORegister(c *gin.Context) {
	var req struct {
		ssoCaptchaRequest
		Email       string `json:"email" binding:"required,email,max=255"`
		Password    string `json:"password" binding:"required,min=6,max=4096"`
		Code        string `json:"code" binding:"required,max=128"`
		DisplayName string `json:"display_name" binding:"max=100"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Check the registration fields")
		return
	}
	if !h.verifySSOCaptcha(c, req.ssoCaptchaRequest) {
		return
	}
	if err := h.ensureBackendModeAllowsNewUserLogin(c.Request.Context()); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	user, err := h.ssoService.Register(c.Request.Context(), req.Email, req.Password, req.Code, req.DisplayName)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	h.finishSSOLogin(c, user)
}

func (h *AuthHandler) finishSSOLogin(c *gin.Context, user *service.User) {
	if err := ensureLoginUserActive(user); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if err := h.ensureBackendModeAllowsUser(c.Request.Context(), user); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if h.totpService != nil && user.TotpEnabled {
		token, err := h.totpService.CreateSSOLoginSession(c.Request.Context(), user.ID, user.Email)
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		response.Success(c, TotpLoginResponse{Requires2FA: true, TempToken: token, UserEmailMasked: service.MaskEmail(user.Email)})
		return
	}
	if user.TotpEnabled {
		response.ErrorFrom(c, service.ErrServiceUnavailable)
		return
	}
	h.authService.RecordSuccessfulLogin(c.Request.Context(), user.ID)
	h.respondWithTokenPair(c, user)
}

// OIDC and password authentication share the authoritative subject resolver and local MFA gate.
func (h *AuthHandler) trySSOOIDCCallback(c *gin.Context, frontend, redirect, intent, issuer, subject, email, name string, verified *bool) bool {
	if h.settingSvc == nil {
		return false
	}
	policy, err := h.settingSvc.GetSSOSettings(c.Request.Context())
	if err != nil {
		redirectOAuthError(c, frontend, "login_blocked", infraerrors.Reason(err), infraerrors.Message(err))
		return true
	}
	if !policy.Enabled && !policy.OnlyEnabled {
		return false
	}
	if intent != oauthIntentLogin {
		if !policy.OnlyEnabled {
			return false
		}
		err = service.ErrSSOOnly
	} else if !policy.Enabled || strings.TrimRight(issuer, "/") != policy.Config.Issuer {
		err = service.ErrSSODisabled
	}
	var user *service.User
	if err == nil {
		user, err = h.authService.ResolveSSOIdentity(c.Request.Context(), policy.Config.Issuer, &casdoor.Identity{
			Subject: subject, Email: email, EmailVerified: verified != nil && *verified, DisplayName: name,
		})
	}
	if err == nil {
		err = ensureLoginUserActive(user)
	}
	if err == nil {
		err = h.ensureBackendModeAllowsUser(c.Request.Context(), user)
	}
	if err != nil {
		redirectOAuthError(c, frontend, "login_blocked", infraerrors.Reason(err), infraerrors.Message(err))
		return true
	}
	clearOAuthPendingSessionCookie(c, isRequestHTTPS(c))
	clearOAuthPendingBrowserCookie(c, isRequestHTTPS(c))
	fragment := url.Values{"redirect": {redirect}}
	if user.TotpEnabled {
		if h.totpService == nil {
			redirectOAuthError(c, frontend, "login_blocked", "MFA_UNAVAILABLE", "Verification is temporarily unavailable")
			return true
		}
		token, err := h.totpService.CreateSSOLoginSession(c.Request.Context(), user.ID, user.Email)
		if err != nil {
			redirectOAuthError(c, frontend, "login_blocked", "MFA_UNAVAILABLE", "Verification is temporarily unavailable")
			return true
		}
		fragment.Set("sso_totp_token", token)
		fragment.Set("email_masked", service.MaskEmail(user.Email))
		redirectWithFragment(c, "/login", fragment)
		return true
	}
	pair, err := h.authService.GenerateTokenPair(c.Request.Context(), user, "")
	if err != nil {
		redirectOAuthError(c, frontend, "login_blocked", "TOKEN_UNAVAILABLE", "Sign-in is temporarily unavailable")
		return true
	}
	h.authService.RecordSuccessfulLogin(c.Request.Context(), user.ID)
	fragment.Set("access_token", pair.AccessToken)
	fragment.Set("refresh_token", pair.RefreshToken)
	fragment.Set("expires_in", fmt.Sprint(pair.ExpiresIn))
	fragment.Set("token_type", "Bearer")
	redirectWithFragment(c, frontend, fragment)
	return true
}

func (h *AuthHandler) checkLocalLoginPolicy(ctx context.Context, user *service.User, ssoAuthenticated bool) error {
	if h.settingSvc == nil {
		return nil
	}
	settings, err := h.settingSvc.GetSSOSettings(ctx)
	if err != nil {
		return err
	}
	if settings.OnlyEnabled && !ssoAuthenticated && !user.IsAdmin() {
		return service.ErrSSOOnly
	}
	return nil
}
