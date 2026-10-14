// Package kanoupdate implements the fork's host-managed container upgrade protocol.
// It deliberately does not import application database or authentication services.
package kanoupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	Repository      = "qianmokano/sub2api"
	ImageRepository = "ghcr.io/qianmokano/sub2api"
	AgentVersion    = "1.0.0"
	ManifestName    = "kano-update.json"
	ProtocolVersion = 1
)

var (
	releasePattern    = regexp.MustCompile(`^v?([0-9]+)\.([0-9]+)\.([0-9]+)-(?:kano\.)?([1-9][0-9]*)$`)
	newReleasePattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+-[1-9][0-9]*$`)
	digestPattern     = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	revisionPattern   = regexp.MustCompile(`^[a-f0-9]{40}$`)
	identifierPattern = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
)

// CompareVersions accepts both historical kano tags and current vX.Y.Z-N tags.
// The release sequence is numeric, so -10 is newer than -2.
func CompareVersions(a, b string) (int, error) {
	x, err := versionParts(a)
	if err != nil {
		return 0, err
	}
	y, err := versionParts(b)
	if err != nil {
		return 0, err
	}
	for i := range x {
		if x[i] < y[i] {
			return -1, nil
		}
		if x[i] > y[i] {
			return 1, nil
		}
	}
	return 0, nil
}

func versionParts(value string) ([4]uint64, error) {
	var result [4]uint64
	matches := releasePattern.FindStringSubmatch(value)
	if len(matches) != 5 {
		return result, errors.New("invalid fork release version")
	}
	for i := range result {
		part, err := strconv.ParseUint(matches[i+1], 10, 64)
		if err != nil {
			return result, errors.New("invalid fork release version")
		}
		result[i] = part
	}
	return result, nil
}

func ParseVersion(value string) ([4]uint64, error) { return versionParts(value) }
func ValidReleaseVersion(value string) bool        { return newReleasePattern.MatchString(value) }
func ValidDigest(value string) bool                { return digestPattern.MatchString(value) }
func ProtectedSetting(key string) bool             { return protectedSetting(key) }

type ColumnRule struct {
	Type     string          `json:"type"`
	Nullable bool            `json:"nullable"`
	Default  string          `json:"default"`
	Value    json.RawMessage `json:"value"`
}

// ValidationPolicy is reviewed with the source change, not supplied by an admin.
// v1 only permits new columns/tables and declared settings; old business values
// and column types must remain intact. It cannot contain SQL or commands.
type ValidationPolicy struct {
	MinimumSourceVersion string                           `json:"minimum_source_version"`
	AddedColumns         map[string]map[string]ColumnRule `json:"added_columns"`
	NewTables            map[string]int64                 `json:"new_tables"`
	Settings             map[string]string                `json:"settings"`
}

type Manifest struct {
	SchemaVersion       int               `json:"schema_version"`
	Version             string            `json:"version"`
	Image               string            `json:"image"`
	ImageDigest         string            `json:"image_digest"`
	Revision            string            `json:"revision"`
	MinimumAgentVersion string            `json:"minimum_agent_version"`
	Migrations          map[string]string `json:"migrations"`
	Validation          ValidationPolicy  `json:"validation"`
}

func (m Manifest) Validate() error {
	if m.SchemaVersion != ProtocolVersion || !newReleasePattern.MatchString(m.Version) {
		return errors.New("unsupported update manifest or release version")
	}
	if _, err := versionParts(m.Version); err != nil {
		return errors.New("invalid fork release version")
	}
	if m.Image != ImageRepository+":"+m.Version || !digestPattern.MatchString(m.ImageDigest) || !revisionPattern.MatchString(m.Revision) {
		return errors.New("update manifest image, digest or revision is invalid")
	}
	comparison, versionErr := CompareVersions("v"+AgentVersion+"-1", "v"+m.MinimumAgentVersion+"-1")
	if versionErr != nil || comparison < 0 {
		return errors.New("this release requires a different updater version")
	}
	if _, err := versionParts(m.Validation.MinimumSourceVersion); err != nil {
		return errors.New("update manifest source version is invalid")
	}
	if len(m.Migrations) == 0 {
		return errors.New("update manifest migration catalog is empty")
	}
	for name, checksum := range m.Migrations {
		if name == "" || strings.ContainsAny(name, `/\\`) || !strings.HasSuffix(name, ".sql") || !digestPattern.MatchString("sha256:"+checksum) {
			return errors.New("update manifest migration catalog is invalid")
		}
	}
	for table, columns := range m.Validation.AddedColumns {
		if !identifierPattern.MatchString(table) || len(columns) == 0 {
			return errors.New("invalid added column policy")
		}
		for column, rule := range columns {
			if !identifierPattern.MatchString(column) || rule.Type == "" || !json.Valid(rule.Value) {
				return errors.New("invalid added column policy")
			}
		}
	}
	for table, count := range m.Validation.NewTables {
		if !identifierPattern.MatchString(table) || count < 0 {
			return errors.New("invalid new table policy")
		}
	}
	for key := range m.Validation.Settings {
		if key == "" || protectedSetting(key) {
			return errors.New("authentication or initial benefits cannot be changed by an online update")
		}
	}
	return nil
}

func protectedSetting(key string) bool {
	for _, prefix := range []string{"sso_", "oidc_", "turnstile_", "tencent_captcha_", "aliyun_captcha_"} {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	switch key {
	case "migration_sso_auth_v1", "registration_enabled", "email_verify_enabled", "registration_email_suffix_whitelist", "invite_code_enabled", "password_reset_enabled", "default_balance", "default_concurrency", "default_subscriptions":
		return true
	}
	return false
}

type Release struct {
	Tag               string         `json:"tag_name"`
	Name              string         `json:"name"`
	Body              string         `json:"body"`
	URL               string         `json:"html_url"`
	PublishedAt       string         `json:"published_at"`
	Draft             bool           `json:"draft"`
	Prerelease        bool           `json:"prerelease"`
	Assets            []ReleaseAsset `json:"assets"`
	Manifest          *Manifest      `json:"manifest,omitempty"`
	UnavailableReason string         `json:"unavailable_reason,omitempty"`
}

type ReleaseAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

type ReleaseSource interface {
	Latest(context.Context) (*Release, error)
	Resolve(context.Context, string) (*Release, error)
}

type GitHubSource struct {
	Client  *http.Client
	APIBase string
}

func NewGitHubSource() *GitHubSource {
	return &GitHubSource{APIBase: "https://api.github.com", Client: &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 || !trustedGitHubURL(req.URL) {
				return errors.New("untrusted release redirect")
			}
			return nil
		},
	}}
}

func trustedGitHubURL(u *url.URL) bool {
	if u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	switch u.Hostname() {
	case "api.github.com", "github.com", "release-assets.githubusercontent.com", "objects.githubusercontent.com":
		return true
	}
	return false
}

func (s *GitHubSource) fetch(ctx context.Context, address string, target any) error {
	u, err := url.Parse(address)
	if err != nil || !trustedGitHubURL(u) {
		return errors.New("untrusted release URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "kano-sub2api-updater/"+AgentVersion)
	res, err := s.Client.Do(req)
	if err != nil {
		return errors.New("release service unavailable")
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("release service returned HTTP %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return errors.New("release response is unavailable or too large")
	}
	if err = json.Unmarshal(body, target); err != nil {
		return errors.New("invalid release response")
	}
	return nil
}

func (s *GitHubSource) Latest(ctx context.Context) (*Release, error) {
	var releases []Release
	if err := s.fetch(ctx, s.APIBase+"/repos/"+Repository+"/releases?per_page=100", &releases); err != nil {
		return nil, err
	}
	eligible := make([]Release, 0, len(releases))
	for _, release := range releases {
		if newReleasePattern.MatchString(release.Tag) && !release.Draft && !release.Prerelease {
			eligible = append(eligible, release)
		}
	}
	if len(eligible) == 0 {
		return nil, errors.New("no fork release available")
	}
	sort.Slice(eligible, func(i, j int) bool {
		comparison, _ := CompareVersions(eligible[i].Tag, eligible[j].Tag)
		return comparison > 0
	})
	return s.attachManifest(ctx, &eligible[0])
}

func (s *GitHubSource) Resolve(ctx context.Context, version string) (*Release, error) {
	if !newReleasePattern.MatchString(version) {
		return nil, errors.New("invalid update target")
	}
	var release Release
	if err := s.fetch(ctx, s.APIBase+"/repos/"+Repository+"/releases/tags/"+url.PathEscape(version), &release); err != nil {
		return nil, err
	}
	if release.Tag != version || release.Draft || release.Prerelease {
		return nil, errors.New("update target is not a stable fork release")
	}
	return s.attachManifest(ctx, &release)
}

func (s *GitHubSource) attachManifest(ctx context.Context, release *Release) (*Release, error) {
	for _, asset := range release.Assets {
		if asset.Name != ManifestName {
			continue
		}
		// Validate the repository and tag path before following GitHub's asset CDN.
		u, err := url.Parse(asset.URL)
		if err != nil || u.Hostname() != "github.com" || !trustedGitHubURL(u) || u.Path != "/"+Repository+"/releases/download/"+release.Tag+"/"+ManifestName || u.RawQuery != "" {
			return nil, errors.New("manifest asset is not from this fork release")
		}
		var manifest Manifest
		if err = s.fetch(ctx, asset.URL, &manifest); err != nil {
			return nil, err
		}
		if err = manifest.Validate(); err != nil {
			release.UnavailableReason = err.Error()
			return release, nil
		}
		if manifest.Version != release.Tag {
			return nil, errors.New("manifest version differs from its release")
		}
		if err = s.verifyTag(ctx, release.Tag, manifest.Revision); err != nil {
			return nil, err
		}
		release.Manifest = &manifest
		return release, nil
	}
	release.UnavailableReason = "This release has no verified container update manifest"
	return release, nil
}

type gitObject struct {
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

func (s *GitHubSource) verifyTag(ctx context.Context, version, revision string) error {
	var reference struct {
		Object gitObject `json:"object"`
	}
	if err := s.fetch(ctx, s.APIBase+"/repos/"+Repository+"/git/ref/tags/"+url.PathEscape(version), &reference); err != nil {
		return err
	}
	object := reference.Object
	for depth := 0; depth < 5 && object.Type == "tag"; depth++ {
		if !revisionPattern.MatchString(object.SHA) {
			return errors.New("invalid release source tag")
		}
		var annotated struct {
			Object gitObject `json:"object"`
		}
		if err := s.fetch(ctx, s.APIBase+"/repos/"+Repository+"/git/tags/"+object.SHA, &annotated); err != nil {
			return err
		}
		object = annotated.Object
	}
	if object.Type != "commit" || object.SHA != revision {
		return errors.New("release source tag differs from its manifest")
	}
	return nil
}
