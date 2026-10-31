//go:build unit

package kanoupdate

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func validManifest() Manifest {
	return Manifest{SchemaVersion: 1, Version: "v0.2.16-2", Image: ImageRepository + ":v0.2.16-2", ImageDigest: "sha256:" + strings.Repeat("a", 64), Revision: strings.Repeat("b", 40), MinimumAgentVersion: AgentVersion, Migrations: map[string]string{"241_sso_issuer_independence.sql": strings.Repeat("c", 64)}, Validation: ValidationPolicy{MinimumSourceVersion: "v0.2.16-1"}}
}

func TestCompareVersions(t *testing.T) {
	for _, test := range []struct {
		a, b string
		want int
	}{
		{"v0.2.16-10", "v0.2.16-2", 1}, {"0.2.16-2", "v0.2.16-2", 0}, {"v0.2.14-kano.1", "v0.2.16-1", -1}, {"v0.2.16-1", "v0.2.14-kano.10", 1}, {"v1.0.0-1", "v0.99.99-99", 1},
	} {
		actual, err := CompareVersions(test.a, test.b)
		require.NoError(t, err)
		require.Equal(t, test.want, actual)
	}
	for _, invalid := range []string{"latest", "v0.2.16", "v0.2.16-0", "v0.2.16-rc1", "v999999999999999999999.0.0-1"} {
		_, err := CompareVersions(invalid, "v0.2.16-1")
		require.Error(t, err)
	}
	_, err := CompareVersions("v0.2.16-1", "bad")
	require.Error(t, err)
}

func TestManifestValidationRejectsUnsafeUpdates(t *testing.T) {
	require.NoError(t, validManifest().Validate())
	for _, mutate := range []func(*Manifest){
		func(m *Manifest) { m.SchemaVersion = 2 }, func(m *Manifest) { m.Version = "v0.2.16-kano.2" }, func(m *Manifest) { m.Image = "weishaw/sub2api:latest" }, func(m *Manifest) { m.ImageDigest = "bad" }, func(m *Manifest) { m.Revision = "bad" }, func(m *Manifest) { m.MinimumAgentVersion = "9.0.0" }, func(m *Manifest) { m.Validation.MinimumSourceVersion = "bad" }, func(m *Manifest) { m.Migrations = nil }, func(m *Manifest) { m.Migrations["../../run.sql"] = strings.Repeat("c", 64) }, func(m *Manifest) { m.Migrations["x.sql"] = "bad" }, func(m *Manifest) { m.Validation.Settings = map[string]string{"sso_enabled": "false"} }, func(m *Manifest) { m.Validation.Settings = map[string]string{"default_balance": "10"} }, func(m *Manifest) { m.Validation.NewTables = map[string]int64{"users;DELETE": 0} }, func(m *Manifest) { m.Validation.NewTables = map[string]int64{"new_table": -1} }, func(m *Manifest) { m.Validation.AddedColumns = map[string]map[string]ColumnRule{"groups": {}} }, func(m *Manifest) {
			m.Validation.AddedColumns = map[string]map[string]ColumnRule{"groups": {"i18n": {Type: "jsonb", Value: json.RawMessage("bad")}}}
		}, func(m *Manifest) {
			m.Validation.ChangedColumns = map[string]map[string]ChangedColumn{"groups": {}}
		}, func(m *Manifest) {
			m.Validation.ChangedColumns = map[string]map[string]ChangedColumn{"groups;DELETE": {"i18n": {Reason: "reviewed"}}}
		}, func(m *Manifest) {
			m.Validation.ChangedColumns = map[string]map[string]ChangedColumn{"groups": {"i18n": {Reason: "   "}}}
		}, func(m *Manifest) {
			m.Validation.ChangedColumns = map[string]map[string]ChangedColumn{"groups": {"i18n": {Reason: strings.Repeat("r", 201)}}}
		}, func(m *Manifest) {
			m.Validation.AddedColumns = map[string]map[string]ColumnRule{"groups": {"i18n": {Type: "jsonb", Value: json.RawMessage(`{}`)}}}
			m.Validation.ChangedColumns = map[string]map[string]ChangedColumn{"groups": {"i18n": {Reason: "reviewed"}}}
		},
	} {
		m := validManifest()
		mutate(&m)
		require.Error(t, m.Validate())
	}
	m := validManifest()
	m.Validation.AddedColumns = map[string]map[string]ColumnRule{"groups": {"i18n": {Type: "jsonb", Value: json.RawMessage(`{}`)}}}
	m.Validation.NewTables = map[string]int64{"cache": 0}
	m.Validation.Settings = map[string]string{"feature_enabled": "true"}
	require.NoError(t, m.Validate())
	for _, key := range []string{"oidc_issuer", "turnstile_enabled", "tencent_captcha_enabled", "aliyun_captcha_enabled", "registration_enabled", "default_subscriptions", "migration_sso_auth_v1"} {
		require.True(t, protectedSetting(key))
	}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGitHubSourceSelectsNumericForkReleaseAndManifest(t *testing.T) {
	m := validManifest()
	m.Version = "v0.2.16-10"
	m.Image = ImageRepository + ":" + m.Version
	manifest, _ := json.Marshal(m)
	source := NewGitHubSource()
	source.Client.Transport = roundTripper(func(r *http.Request) (*http.Response, error) {
		var body string
		switch {
		case strings.Contains(r.URL.Path, "/git/ref/tags/"):
			body = `{"object":{"type":"commit","sha":"` + m.Revision + `"}}`
		case strings.Contains(r.URL.Path, "/releases/download/"):
			body = string(manifest)
		case strings.Contains(r.URL.Path, "/releases/tags/"):
			body = `{"tag_name":"v0.2.16-10","assets":[{"name":"kano-update.json","browser_download_url":"https://github.com/qianmokano/sub2api/releases/download/v0.2.16-10/kano-update.json"}]}`
		default:
			body = `[{"tag_name":"v0.2.16-2"},{"tag_name":"v0.2.16-10","assets":[{"name":"kano-update.json","browser_download_url":"https://github.com/qianmokano/sub2api/releases/download/v0.2.16-10/kano-update.json"}]},{"tag_name":"v9.0.0-1","draft":true},{"tag_name":"v8.0.0-1","prerelease":true},{"tag_name":"v0.2.17"}]`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	release, err := source.Latest(context.Background())
	require.NoError(t, err)
	require.Equal(t, "v0.2.16-10", release.Tag)
	require.Equal(t, m.ImageDigest, release.Manifest.ImageDigest)
	release, err = source.Resolve(context.Background(), "v0.2.16-10")
	require.NoError(t, err)
	require.NotNil(t, release.Manifest)
	_, err = source.Resolve(context.Background(), "../../bad")
	require.Error(t, err)
	_, err = source.fetchReleaseForTest(context.Background(), "http://127.0.0.1/private")
	require.Error(t, err)
}

func TestReleaseSourceChecksAnnotatedTagsAndRejectsWrongCommits(t *testing.T) {
	for _, fault := range []string{"", "mismatch", "invalid", "recursive"} {
		t.Run(fault, func(t *testing.T) {
			m := validManifest()
			source := NewGitHubSource()
			source.Client.Transport = roundTripper(func(r *http.Request) (*http.Response, error) {
				kind, sha := "tag", strings.Repeat("d", 40)
				if strings.Contains(r.URL.Path, "/git/tags/") {
					kind = "commit"
					sha = m.Revision
					if fault == "mismatch" {
						sha = strings.Repeat("e", 40)
					}
					if fault == "recursive" {
						kind = "tag"
					}
				}
				if fault == "invalid" {
					sha = "invalid"
				}
				body, _ := json.Marshal(map[string]any{"object": gitObject{Type: kind, SHA: sha}})
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
			})
			err := source.verifyTag(context.Background(), m.Version, m.Revision)
			if fault == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func (s *GitHubSource) fetchReleaseForTest(ctx context.Context, address string) (*Release, error) {
	var r Release
	return &r, s.fetch(ctx, address, &r)
}

func TestGitHubManifestMissingInvalidAndForeign(t *testing.T) {
	source := NewGitHubSource()
	r, err := source.attachManifest(context.Background(), &Release{Tag: "v0.2.16-2"})
	require.NoError(t, err)
	require.NotEmpty(t, r.UnavailableReason)
	_, err = source.attachManifest(context.Background(), &Release{Tag: "v0.2.16-2", Assets: []ReleaseAsset{{Name: ManifestName, URL: "https://github.com/attacker/repo/releases/download/v0.2.16-2/kano-update.json"}}})
	require.Error(t, err)
	for _, address := range []string{"http://github.com/test", "https://github.com:444/test", "https://user@github.com/test", "https://evil.test/"} {
		u, _ := url.Parse(address)
		require.False(t, trustedGitHubURL(u))
	}
	for _, address := range []string{"https://api.github.com/test", "https://github.com:443/test", "https://release-assets.githubusercontent.com/test", "https://objects.githubusercontent.com/test"} {
		u, _ := url.Parse(address)
		require.True(t, trustedGitHubURL(u))
	}
}
