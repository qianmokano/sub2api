package service

import (
	"context"
	"errors"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/pkg/casdoor"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// ResolveSSOIdentity resolves a trusted IdP subject without issuing any tokens.
// Existing bindings are authoritative even when the IdP email has changed.
func (s *AuthService) ResolveSSOIdentity(ctx context.Context, issuer string, identity *casdoor.Identity) (*User, error) {
	if s == nil || s.entClient == nil || s.userRepo == nil || identity == nil || identity.Subject == "" || issuer == "" {
		return nil, ErrServiceUnavailable
	}
	user, err := s.findEmailOAuthIdentityOwner(ctx, "oidc", issuer, identity.Subject)
	if err != nil {
		return nil, err
	}
	if user != nil {
		if !user.IsActive() || user.DeletedAt != nil {
			return nil, ErrUserNotActive
		}
		return user, nil
	}
	if !identity.EmailVerified {
		return nil, infraerrors.Forbidden("SSO_EMAIL_NOT_VERIFIED", "Verify your email in kano Passport before signing in")
	}
	email, err := ssoRegistrationEmail(identity.Email)
	if err != nil {
		return nil, err
	}
	if existing, lookupErr := s.userRepo.GetByEmail(ctx, email); lookupErr == nil && existing != nil {
		bound, bindErr := s.findEmailOAuthIdentityOwner(ctx, "oidc", issuer, identity.Subject)
		if bindErr != nil {
			return nil, ErrServiceUnavailable
		}
		if bound != nil && bound.ID == existing.ID {
			if !bound.IsActive() || bound.DeletedAt != nil {
				return nil, ErrUserNotActive
			}
			return bound, nil
		}
		return nil, ErrSSOIdentityConflict
	} else if lookupErr != nil && !errors.Is(lookupErr, ErrUserNotFound) {
		return nil, ErrServiceUnavailable
	}
	if s.settingService != nil && s.settingService.IsBackendModeEnabled(ctx) {
		return nil, infraerrors.Forbidden("BACKEND_MODE_ADMIN_ONLY", "Backend mode is active. Only admin login is allowed.")
	}
	return s.createSSOUser(ctx, issuer, email, identity)
}

func (s *AuthService) createSSOUser(ctx context.Context, issuer, email string, identity *casdoor.Identity) (*User, error) {
	password, err := randomSSOToken()
	if err != nil {
		return nil, ErrServiceUnavailable
	}
	hash, err := s.HashPassword(password)
	if err != nil {
		return nil, ErrServiceUnavailable
	}
	plan := s.resolveSignupGrantPlan(ctx, "oidc")
	rpm := 0
	if s.settingService != nil {
		rpm = s.settingService.GetDefaultUserRPMLimit(ctx)
	}
	user := &User{Email: email, Username: ssoDisplayName(identity), PasswordHash: hash, Role: RoleUser,
		Balance: plan.Balance, Concurrency: plan.Concurrency, RPMLimit: rpm, Status: StatusActive, SignupSource: "oidc"}
	tx, err := s.entClient.Tx(ctx)
	if err != nil {
		return nil, ErrServiceUnavailable
	}
	defer func() { _ = tx.Rollback() }()
	txCtx := dbent.NewTxContext(ctx, tx)
	if err := s.userRepo.Create(txCtx, user); err != nil {
		_ = tx.Rollback()
		if errors.Is(err, ErrEmailExists) {
			// A concurrent SSO request can win the insert; only its identical subject is reusable.
			existing, lookupErr := s.findEmailOAuthIdentityOwner(ctx, "oidc", issuer, identity.Subject)
			if lookupErr == nil && existing != nil && existing.IsActive() {
				return existing, nil
			}
			return nil, ErrSSOIdentityConflict
		}
		return nil, ErrServiceUnavailable
	}
	_, err = tx.AuthIdentity.Create().SetUserID(user.ID).SetProviderType("oidc").SetProviderKey(issuer).
		SetProviderSubject(identity.Subject).SetIssuer(issuer).SetVerifiedAt(time.Now().UTC()).
		SetMetadata(map[string]any{"email": email, "email_verified": true, "display_name": identity.DisplayName, "username": identity.Username}).Save(txCtx)
	if err != nil {
		return nil, ErrServiceUnavailable
	}
	// The account, identity and subscription grants commit together, so retries do not double-grant.
	if s.defaultSubAssigner != nil {
		for _, item := range plan.Subscriptions {
			if _, _, err := s.defaultSubAssigner.AssignOrExtendSubscription(txCtx, &AssignSubscriptionInput{
				UserID: user.ID, GroupID: item.GroupID, ValidityDays: item.ValidityDays, Notes: "auto assigned by OIDC signup defaults",
			}); err != nil {
				return nil, ErrServiceUnavailable
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, ErrServiceUnavailable
	}
	_ = s.snapshotPlatformQuotaDefaults(ctx, user.ID, &plan)
	s.bindOAuthAffiliate(ctx, user.ID, "")
	return user, nil
}

func ssoDisplayName(identity *casdoor.Identity) string {
	name := firstNonEmpty(identity.DisplayName, identity.Username)
	if name == "" {
		name = strings.SplitN(identity.Email, "@", 2)[0]
	}
	// Ent's MaxLen validates UTF-8 bytes; preserve complete runes within that limit.
	if len(name) > 100 {
		end := 0
		for index := range name {
			if index > 100 {
				break
			}
			end = index
		}
		name = name[:end]
	}
	return name
}

// AuthenticatePassword verifies local credentials without generating a pre-MFA token.
func (s *AuthService) AuthenticatePassword(ctx context.Context, email, password string) (*User, error) {
	user, err := s.userRepo.GetByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			return nil, ErrInvalidCredentials
		}
		return nil, ErrServiceUnavailable
	}
	if !s.CheckPassword(password, user.PasswordHash) {
		return nil, ErrInvalidCredentials
	}
	if !user.IsActive() || user.DeletedAt != nil {
		return nil, ErrUserNotActive
	}
	return user, nil
}
