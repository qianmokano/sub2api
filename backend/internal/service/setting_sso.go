package service

import (
	"context"
	"net/url"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/casdoor"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	SettingKeySSOIssuerURL           = "sso_issuer_url"
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
	Config              casdoor.Config
}

var ErrSSOOnly = infraerrors.Forbidden("SSO_ONLY", "Please use kano Passport to sign in or manage your account")

type ssoSettingsMigrator interface {
	MigrateSSOSettings(context.Context, map[string]string) error
}

func (s *SettingService) ssoSettingDefaults() map[string]string {
	values := map[string]string{
		SettingKeySSOEnabled: "false", SettingKeySSOOnlyEnabled: "false",
		SettingKeySSORegistrationEnabled: "false", SettingKeySSOIssuerURL: "",
		SettingKeySSOOrganization: "kano", SettingKeySSOApplication: "admin/sub2api",
	}
	if s != nil && s.cfg != nil {
		cfg := s.cfg.SSO
		values[SettingKeySSOEnabled] = strconv.FormatBool(cfg.Enabled)
		values[SettingKeySSOOnlyEnabled] = strconv.FormatBool(cfg.OnlyEnabled)
		values[SettingKeySSORegistrationEnabled] = strconv.FormatBool(cfg.RegistrationEnabled)
		values[SettingKeySSOIssuerURL] = cfg.IssuerURL
		if cfg.Organization != "" || s.cfg.SSOConfigured["organization"] {
			values[SettingKeySSOOrganization] = cfg.Organization
		}
		if cfg.Application != "" || s.cfg.SSOConfigured["application"] {
			values[SettingKeySSOApplication] = cfg.Application
		}
	}
	return values
}

func (s *SettingService) ssoSettingValue(settings map[string]string, key string) string {
	if value, exists := settings[key]; exists {
		return value
	}
	return s.ssoSettingDefaults()[key]
}

// MigrateSSOSettings consumes legacy runtime input only during the first migration.
func (s *SettingService) MigrateSSOSettings(ctx context.Context) error {
	migrator, ok := s.settingRepo.(ssoSettingsMigrator)
	if !ok {
		return ErrServiceUnavailable
	}
	values := s.ssoSettingDefaults()
	if s.cfg != nil && !s.cfg.SSOConfigured["issuer_url"] && s.cfg.LegacySSOIssuer != nil {
		values[SettingKeySSOIssuerURL] = *s.cfg.LegacySSOIssuer
	}
	return migrator.MigrateSSOSettings(ctx, values)
}

// GetSSOSettings reads the current policy without silently disabling it on storage errors.
func (s *SettingService) GetSSOSettings(ctx context.Context) (*SSOSettings, error) {
	if s == nil || s.settingRepo == nil || s.ssoMigrationErr != nil {
		return nil, ErrServiceUnavailable
	}
	settings, err := s.settingRepo.GetMultiple(ctx, []string{
		SettingKeySSOEnabled, SettingKeySSOOnlyEnabled, SettingKeySSORegistrationEnabled,
		SettingKeySSOOrganization, SettingKeySSOApplication, SettingKeySSOIssuerURL,
	})
	if err != nil {
		return nil, ErrServiceUnavailable
	}
	flags := make(map[string]bool, 3)
	for _, key := range []string{SettingKeySSOEnabled, SettingKeySSOOnlyEnabled, SettingKeySSORegistrationEnabled} {
		value := s.ssoSettingValue(settings, key)
		if value != "true" && value != "false" {
			return nil, ErrServiceUnavailable
		}
		flags[key] = value == "true"
	}
	return &SSOSettings{
		Enabled: flags[SettingKeySSOEnabled], OnlyEnabled: flags[SettingKeySSOOnlyEnabled],
		RegistrationEnabled: flags[SettingKeySSORegistrationEnabled],
		Config: casdoor.Config{
			Issuer:       strings.TrimRight(strings.TrimSpace(s.ssoSettingValue(settings, SettingKeySSOIssuerURL)), "/"),
			Organization: s.ssoSettingValue(settings, SettingKeySSOOrganization),
			Application:  s.ssoSettingValue(settings, SettingKeySSOApplication),
		},
	}, nil
}

func validateSSOSettings(settings *SystemSettings) error {
	settings.SSOIssuerURL = strings.TrimRight(strings.TrimSpace(settings.SSOIssuerURL), "/")
	settings.SSOOrganization = strings.TrimSpace(settings.SSOOrganization)
	settings.SSOApplication = strings.TrimSpace(settings.SSOApplication)
	if !settings.SSOEnabled {
		if settings.SSOOnlyEnabled || settings.SSORegistrationEnabled {
			return infraerrors.BadRequest("SSO_CONFIG_INVALID", "Enable in-page SSO before enabling its login policy or registration")
		}
		return nil
	}
	return validateSSOConfig(casdoor.Config{
		Issuer: settings.SSOIssuerURL, Organization: settings.SSOOrganization, Application: settings.SSOApplication,
	})
}

func validateSSOConfig(cfg casdoor.Config) error {
	u, err := url.Parse(cfg.Issuer)
	parts := strings.Split(cfg.Application, "/")
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") != "" ||
		cfg.Organization == "" || len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return infraerrors.BadRequest("SSO_CONFIG_INVALID", "Configure an HTTPS Passport issuer, a user organization and an application ID (owner/name)")
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
	issuer := s.ssoSettingValue(settings, SettingKeySSOIssuerURL)
	u, err := url.Parse(issuer)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") != "" {
		return "", ""
	}
	organization := s.ssoSettingValue(settings, SettingKeySSOOrganization)
	parts := strings.Split(s.ssoSettingValue(settings, SettingKeySSOApplication), "/")
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
