package handler

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/pkg/casdoorcaptcha"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// SetSSOService attaches the provider without changing the existing handler constructor.
func (h *AuthHandler) SetSSOService(sso *service.SSOService) { h.ssoService = sso }

func (h *AuthHandler) SSOCaptcha(c *gin.Context) {
	var req struct {
		Action  string `json:"action" binding:"required,oneof=login register-send-code register"`
		Account string `json:"account" binding:"required,max=255"`
	}
	if c.ShouldBindJSON(&req) != nil {
		response.BadRequest(c, "Check the verification fields")
		return
	}
	if h.ssoService == nil {
		response.ErrorFrom(c, service.ErrSSODisabled)
		return
	}
	challenge, err := h.ssoService.PrepareCaptcha(c.Request.Context(), req.Action, req.Account)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, challenge)
}

type ssoCaptchaRequest struct {
	Captcha               *casdoorcaptcha.Proof `json:"captcha,omitempty"`
	TurnstileToken        string                `json:"turnstile_token"`
	TencentCaptchaTicket  string                `json:"tencent_captcha_ticket"`
	TencentCaptchaRandstr string                `json:"tencent_captcha_randstr"`
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
	result, err := h.ssoService.Login(c.Request.Context(), req.Account, req.Password, req.Captcha)
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
	if err := h.ssoService.SendCode(c.Request.Context(), req.Email, req.Captcha); err != nil {
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
	user, err := h.ssoService.Register(c.Request.Context(), req.Email, req.Password, req.Code, req.DisplayName, req.Captcha)
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

func (h *AuthHandler) checkLocalLoginPolicy(ctx context.Context, user *service.User, ssoAuthenticated bool) error {
	if user != nil && user.IsAdmin() {
		return nil
	}
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
