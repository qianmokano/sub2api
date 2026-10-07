package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/mail"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/casdoor"
	"github.com/Wei-Shaw/sub2api/internal/pkg/casdoorcaptcha"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

var (
	ErrSSODisabled         = infraerrors.Forbidden("SSO_DISABLED", "Unified sign-in is disabled")
	ErrSSOChallengeInvalid = infraerrors.BadRequest("SSO_CHALLENGE_INVALID", "Verification expired; please sign in again")
	ErrSSOChallengeBusy    = infraerrors.TooManyRequests("SSO_CHALLENGE_BUSY", "Verification is in progress; please try again")
	ErrSSOIdentityConflict = infraerrors.Conflict("SSO_IDENTITY_CONFLICT", "This email belongs to an existing account; contact support to verify its identity binding")
)

// SSOChallengeStore keeps IdP cookies on the server and leases challenges while verifying them.
type SSOChallengeStore interface {
	Put(context.Context, string, *casdoor.Session) error
	Lock(context.Context, string, string) (*casdoor.Session, error)
	Unlock(context.Context, string, string) error
	Update(context.Context, string, string, *casdoor.Session) error
	Consume(context.Context, string, string) (bool, error)
}

type ssoProvider interface {
	Login(context.Context, string, string, ...*casdoorcaptcha.Proof) (*casdoor.Identity, *casdoor.Session, error)
	PrepareCaptcha(context.Context, string, string) (*casdoorcaptcha.Challenge, error)
	CompleteMFA(context.Context, *casdoor.Session, string, string) (*casdoor.Identity, error)
	SendCode(context.Context, string, ...*casdoorcaptcha.Proof) error
	Register(context.Context, string, string, string, string, string, ...*casdoorcaptcha.Proof) (*casdoor.Identity, error)
}

type SSOChallenge struct {
	Token   string              `json:"token"`
	Methods []casdoor.MFAMethod `json:"methods"`
}

type SSOLoginResult struct {
	User      *User
	Challenge *SSOChallenge
}

type SSOService struct {
	settings   *SettingService
	auth       *AuthService
	challenges SSOChallengeStore
	newClient  func(casdoor.Config) ssoProvider
}

func NewSSOService(settings *SettingService, auth *AuthService, challenges SSOChallengeStore) *SSOService {
	return &SSOService{
		settings: settings, auth: auth, challenges: challenges,
		newClient: func(cfg casdoor.Config) ssoProvider {
			store, _ := challenges.(casdoorcaptcha.Store)
			return casdoor.New(cfg, nil, store)
		},
	}
}

func (s *SSOService) config(ctx context.Context, registration bool) (*SSOSettings, error) {
	cfg, err := s.settings.GetSSOSettings(ctx)
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, ErrSSODisabled
	}
	if err := validateSSOConfig(cfg.Config); err != nil {
		return nil, err
	}
	if registration && !cfg.RegistrationEnabled {
		return nil, ErrRegDisabled
	}
	return cfg, nil
}

func (s *SSOService) PrepareCaptcha(ctx context.Context, action, account string) (*casdoorcaptcha.Challenge, error) {
	if action != casdoorcaptcha.ActionLogin && action != casdoorcaptcha.ActionSendCode && action != casdoorcaptcha.ActionRegister {
		return nil, ssoProviderError(casdoorcaptcha.ErrInvalid)
	}
	cfg, err := s.config(ctx, action != casdoorcaptcha.ActionLogin)
	if err != nil {
		return nil, err
	}
	if action != casdoorcaptcha.ActionLogin {
		account, err = ssoRegistrationEmail(account)
		if err != nil {
			return nil, err
		}
	}
	challenge, err := s.newClient(cfg.Config).PrepareCaptcha(ctx, action, account)
	return challenge, ssoProviderError(err)
}

func (s *SSOService) Login(ctx context.Context, account, password string, proofs ...*casdoorcaptcha.Proof) (*SSOLoginResult, error) {
	cfg, err := s.config(ctx, false)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(account) == "" || password == "" || len(account) > 255 || len(password) > 4096 {
		return nil, ErrInvalidCredentials
	}
	identity, session, err := s.newClient(cfg.Config).Login(ctx, account, password, proofs...)
	if err != nil {
		return nil, ssoProviderError(err)
	}
	if session != nil {
		if s.challenges == nil {
			return nil, ErrServiceUnavailable
		}
		token, err := randomSSOToken()
		if err != nil {
			return nil, ErrServiceUnavailable
		}
		if err := s.challenges.Put(ctx, token, session); err != nil {
			return nil, ErrServiceUnavailable
		}
		return &SSOLoginResult{Challenge: &SSOChallenge{Token: token, Methods: session.Methods}}, nil
	}
	user, err := s.auth.ResolveSSOIdentity(ctx, cfg.Config.Issuer, identity)
	return &SSOLoginResult{User: user}, err
}

func (s *SSOService) CompleteMFA(ctx context.Context, token, method, code string) (*User, error) {
	cfg, err := s.config(ctx, false)
	if err != nil {
		return nil, err
	}
	if s.challenges == nil || len(token) != 64 || method == "" || code == "" || len(code) > 128 {
		return nil, ErrSSOChallengeInvalid
	}
	lease, err := randomSSOToken()
	if err != nil {
		return nil, ErrServiceUnavailable
	}
	session, err := s.challenges.Lock(ctx, token, lease)
	if err != nil {
		return nil, err
	}
	defer func() { _ = s.challenges.Unlock(context.WithoutCancel(ctx), token, lease) }()
	if session == nil || session.Config != cfg.Config {
		return nil, ErrSSOChallengeInvalid
	}
	identity, err := s.newClient(cfg.Config).CompleteMFA(ctx, session, method, code)
	if err != nil {
		if updateErr := s.challenges.Update(ctx, token, lease, session); updateErr != nil {
			return nil, ErrServiceUnavailable
		}
		return nil, ssoProviderError(err)
	}
	consumed, err := s.challenges.Consume(ctx, token, lease)
	if err != nil {
		return nil, ErrServiceUnavailable
	}
	if !consumed {
		return nil, ErrSSOChallengeInvalid
	}
	return s.auth.ResolveSSOIdentity(ctx, cfg.Config.Issuer, identity)
}

func (s *SSOService) SendCode(ctx context.Context, email string, proofs ...*casdoorcaptcha.Proof) error {
	cfg, err := s.config(ctx, true)
	if err != nil {
		return err
	}
	email, err = ssoRegistrationEmail(email)
	if err != nil {
		return err
	}
	return ssoProviderError(s.newClient(cfg.Config).SendCode(ctx, email, proofs...))
}

func (s *SSOService) Register(ctx context.Context, email, password, code, displayName string, proofs ...*casdoorcaptcha.Proof) (*User, error) {
	cfg, err := s.config(ctx, true)
	if err != nil {
		return nil, err
	}
	email, err = ssoRegistrationEmail(email)
	if err != nil {
		return nil, err
	}
	if len(password) < 6 || len(password) > 4096 || strings.TrimSpace(code) == "" || len(code) > 128 || len(displayName) > 100 {
		return nil, infraerrors.BadRequest("SSO_REGISTER_INVALID", "Check the registration fields")
	}
	// A random username avoids collisions between email local parts without retrying a signup.
	suffix, err := randomSSOToken()
	if err != nil {
		return nil, ErrServiceUnavailable
	}
	username := "u_" + suffix[:24]
	if strings.TrimSpace(displayName) == "" {
		displayName = strings.SplitN(email, "@", 2)[0]
	}
	identity, err := s.newClient(cfg.Config).Register(ctx, email, password, strings.TrimSpace(code), displayName, username, proofs...)
	if err != nil {
		return nil, ssoProviderError(err)
	}
	user, err := s.auth.ResolveSSOIdentity(ctx, cfg.Config.Issuer, identity)
	if err != nil {
		return nil, infraerrors.Conflict("SSO_ACCOUNT_SYNC_FAILED", "Passport created; sign in with the same account to retry. Contact support if account linking fails.")
	}
	return user, nil
}

func ssoRegistrationEmail(email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	parsed, err := mail.ParseAddress(email)
	if err != nil || parsed.Address != email || len(email) > 255 || isReservedEmail(email) {
		return "", infraerrors.BadRequest("INVALID_EMAIL", "Enter a valid email address")
	}
	return email, nil
}

func randomSSOToken() (string, error) {
	var bytes [32]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}

func ssoProviderError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, casdoor.ErrCredentials):
		return ErrInvalidCredentials
	case errors.Is(err, casdoor.ErrFrozen):
		return infraerrors.TooManyRequests("SSO_ACCOUNT_LOCKED", "Too many attempts; please try again later")
	case errors.Is(err, casdoorcaptcha.ErrInvalid), errors.Is(err, casdoorcaptcha.ErrChallenge):
		return infraerrors.BadRequest("SSO_CAPTCHA_INVALID", "Verification expired or is invalid; please try again")
	case errors.Is(err, casdoorcaptcha.ErrUnsupported):
		return infraerrors.BadRequest("SSO_CAPTCHA_UNSUPPORTED", "The Passport verification method is unsupported")
	case errors.Is(err, casdoor.ErrCaptcha):
		return infraerrors.BadRequest("SSO_CAPTCHA_REQUIRED", "Complete the Passport verification on this page")
	case errors.Is(err, casdoor.ErrMFACode):
		return infraerrors.BadRequest("SSO_MFA_CODE_INVALID", "Incorrect verification code; please try again")
	case errors.Is(err, casdoor.ErrCode):
		return infraerrors.BadRequest("SSO_REGISTER_CODE_INVALID", "Invalid or expired email verification code")
	case errors.Is(err, casdoor.ErrEmailExists):
		return ErrEmailExists
	case errors.Is(err, casdoor.ErrResendWait):
		return infraerrors.TooManyRequests("SSO_CODE_RESEND_WAIT", "Wait before requesting another verification code")
	case errors.Is(err, casdoor.ErrIdentity):
		return infraerrors.Forbidden("SSO_IDENTITY_INVALID", "This identity cannot sign in")
	default:
		return infraerrors.ServiceUnavailable("SSO_PROVIDER_UNAVAILABLE", "kano Passport is temporarily unavailable; please try again")
	}
}
