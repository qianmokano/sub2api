package config

import (
	"os"
	"strings"

	"github.com/spf13/viper"
)

// SSOConfig contains only the Casdoor in-page authentication settings.
type SSOConfig struct {
	Enabled             bool   `mapstructure:"enabled"`
	OnlyEnabled         bool   `mapstructure:"only_enabled"`
	RegistrationEnabled bool   `mapstructure:"registration_enabled"`
	IssuerURL           string `mapstructure:"issuer_url"`
	Organization        string `mapstructure:"organization"`
	Application         string `mapstructure:"application"`
}

func loadSSOMigrationInputs(cfg *Config) {
	cfg.SSOConfigured = make(map[string]bool)
	for _, name := range []string{"enabled", "only_enabled", "registration_enabled", "issuer_url", "organization", "application"} {
		_, env := os.LookupEnv("SSO_" + strings.ToUpper(name))
		cfg.SSOConfigured[name] = viper.InConfig("sso."+name) || env
	}
	if value, exists := os.LookupEnv("SSO_ISSUER_URL"); exists {
		cfg.SSO.IssuerURL = value
	}
	if value, exists := os.LookupEnv("SSO_ORGANIZATION"); exists {
		cfg.SSO.Organization = value
	}
	if value, exists := os.LookupEnv("SSO_APPLICATION"); exists {
		cfg.SSO.Application = value
	}
	if !cfg.SSOConfigured["organization"] {
		cfg.SSO.Organization = "kano"
	}
	if !cfg.SSOConfigured["application"] {
		cfg.SSO.Application = "admin/sub2api"
	}
	_, env := os.LookupEnv("OIDC_CONNECT_ISSUER_URL")
	if viper.InConfig("oidc_connect.issuer_url") || env {
		issuer := viper.GetString("oidc_connect.issuer_url")
		if env {
			issuer = os.Getenv("OIDC_CONNECT_ISSUER_URL")
		}
		cfg.LegacySSOIssuer = &issuer
	}
}
