package service

import (
	"context"
	"net/url"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/casdoor"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	SettingKeySSOEnabled             = "sso_enabled"
	SettingKeySSOOnlyEnabled         = "sso_only_enabled"
	SettingKeySSORegistrationEnabled = "sso_registration_enabled"
	SettingKeySSOOrganization        = "sso_organization"
	SettingKeySSOApplication         = "sso_application"
)

type SSOSettings struct {
	Enabled             bool
	OnlyEnabled         bool
	RegistrationEnabled bool
	OIDCEnabled         bool
	Config              casdoor.Config
}

var ErrSSOOnly = infraerrors.Forbidden("SSO_ONLY", "Please use kano Passport to sign in or manage your account")

// GetSSOSettings reads the current policy without silently disabling it on storage errors.
func (s *SettingService) GetSSOSettings(ctx context.Context) (*SSOSettings, error) {
	if s == nil || s.settingRepo == nil {
		return nil, ErrServiceUnavailable
	}
	settings, err := s.settingRepo.GetMultiple(ctx, []string{
		SettingKeySSOEnabled, SettingKeySSOOnlyEnabled, SettingKeySSORegistrationEnabled,
		SettingKeySSOOrganization, SettingKeySSOApplication, SettingKeyOIDCConnectIssuerURL, SettingKeyOIDCConnectEnabled,
	})
	if err != nil {
		return nil, ErrServiceUnavailable
	}
	issuer := settings[SettingKeyOIDCConnectIssuerURL]
	oidcEnabled := settings[SettingKeyOIDCConnectEnabled] == "true"
	if s.cfg != nil {
		if strings.TrimSpace(issuer) == "" {
			issuer = s.cfg.OIDC.IssuerURL
		}
		if _, exists := settings[SettingKeyOIDCConnectEnabled]; !exists {
			oidcEnabled = s.cfg.OIDC.Enabled
		}
	}
	return &SSOSettings{
		Enabled: settings[SettingKeySSOEnabled] == "true", OnlyEnabled: settings[SettingKeySSOOnlyEnabled] == "true",
		RegistrationEnabled: settings[SettingKeySSORegistrationEnabled] == "true", OIDCEnabled: oidcEnabled,
		Config: casdoor.Config{
			Issuer:       strings.TrimRight(strings.TrimSpace(issuer), "/"),
			Organization: firstNonEmpty(settings[SettingKeySSOOrganization], "kano"),
			Application:  firstNonEmpty(settings[SettingKeySSOApplication], "admin/sub2api"),
		},
	}, nil
}

func validateSSOSettings(settings *SystemSettings) error {
	settings.SSOOrganization = firstNonEmpty(settings.SSOOrganization, "kano")
	settings.SSOApplication = firstNonEmpty(settings.SSOApplication, "admin/sub2api")
	if !settings.SSOEnabled {
		if settings.SSOOnlyEnabled || settings.SSORegistrationEnabled {
			return infraerrors.BadRequest("SSO_CONFIG_INVALID", "Enable in-page SSO before enabling its login policy or registration")
		}
		return nil
	}
	return validateSSOConfig(settings.OIDCConnectEnabled, casdoor.Config{
		Issuer: settings.OIDCConnectIssuerURL, Organization: settings.SSOOrganization, Application: settings.SSOApplication,
	})
}

func validateSSOConfig(oidcEnabled bool, cfg casdoor.Config) error {
	u, err := url.Parse(cfg.Issuer)
	parts := strings.Split(cfg.Application, "/")
	if !oidcEnabled || err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") != "" ||
		cfg.Organization == "" || len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return infraerrors.BadRequest("SSO_CONFIG_INVALID", "Configure an enabled HTTPS OIDC issuer, a user organization and an application ID (owner/name)")
	}
	return nil
}

func (s *SettingService) ssoAccountURL(settings map[string]string) string {
	account, _ := s.ssoManagementURLs(settings)
	return account
}

func (s *SettingService) ssoPasswordResetURL(settings map[string]string) string {
	_, reset := s.ssoManagementURLs(settings)
	return reset
}

func (s *SettingService) ssoAdminURL(settings map[string]string) string {
	account, _ := s.ssoManagementURLs(settings)
	if account == "" {
		return ""
	}
	u, _ := url.Parse(account)
	u.Path, u.RawPath = "/login/built-in", ""
	return u.String()
}

// RequireLocalUserIdentity preserves administrator recovery and checks the
// current role before a proposed role change can bypass customer management.
func (s *SettingService) RequireLocalUserIdentity(ctx context.Context, currentRole, nextRole string) error {
	if currentRole == RoleAdmin && nextRole != RoleUser {
		return nil
	}
	// Settings are optional in legacy service fixtures; production injects them.
	if s == nil {
		return nil
	}
	policy, err := s.GetSSOSettings(ctx)
	if err != nil {
		return err
	}
	if policy.OnlyEnabled {
		return ErrSSOOnly
	}
	return nil
}

func (s *SettingService) ssoManagementURLs(settings map[string]string) (string, string) {
	issuer := settings[SettingKeyOIDCConnectIssuerURL]
	if issuer == "" && s.cfg != nil {
		issuer = s.cfg.OIDC.IssuerURL
	}
	u, err := url.Parse(issuer)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") != "" {
		return "", ""
	}
	organization := firstNonEmpty(settings[SettingKeySSOOrganization], "kano")
	parts := strings.Split(firstNonEmpty(settings[SettingKeySSOApplication], "admin/sub2api"), "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", ""
	}
	base := strings.TrimRight(issuer, "/")
	accountURL := base + "/login/" + url.PathEscape(organization)
	if organization == "kano" {
		accountURL = base + "/account"
	}
	return accountURL, base + "/forget/" + url.PathEscape(parts[1])
}
