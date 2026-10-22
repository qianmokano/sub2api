package kanoagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

type Config struct {
	DeploymentDir   string   `json:"deployment_dir"`
	ComposeFiles    []string `json:"compose_files"`
	ImageOverride   string   `json:"image_override"`
	BackupDir       string   `json:"backup_dir"`
	StateDir        string   `json:"state_dir"`
	MaintenanceFile string   `json:"maintenance_file"`
	ControlSocket   string   `json:"control_socket"`
	ProgressSocket  string   `json:"progress_socket"`
	ControlGID      int      `json:"control_gid"`
	ProgressGID     int      `json:"progress_gid"`
	LiveURL         string   `json:"live_url"`
}

func (c Config) Validate() error {
	for _, path := range []string{c.DeploymentDir, c.ImageOverride, c.BackupDir, c.StateDir, c.MaintenanceFile, c.ControlSocket, c.ProgressSocket} {
		if !filepath.IsAbs(path) || filepath.Clean(path) == "/" {
			return errors.New("updater paths must be absolute and specific")
		}
	}
	if inside(c.BackupDir, c.DeploymentDir) || inside(c.StateDir, c.DeploymentDir) || inside(c.DeploymentDir, c.BackupDir) {
		return errors.New("backup and updater state must be outside the application directory")
	}
	if !inside(c.ImageOverride, c.DeploymentDir) || len(c.ComposeFiles) == 0 {
		return errors.New("invalid compose configuration")
	}
	found := false
	for _, path := range c.ComposeFiles {
		if !filepath.IsAbs(path) || !inside(path, c.DeploymentDir) {
			return errors.New("compose files must belong to the fixed deployment")
		}
		if path == c.ImageOverride {
			found = true
		}
	}
	if !found {
		return errors.New("image override must be the last compose file")
	}
	if c.ComposeFiles[len(c.ComposeFiles)-1] != c.ImageOverride {
		return errors.New("image override must be the last compose file")
	}
	u, err := url.Parse(c.LiveURL)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Hostname() != "127.0.0.1" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("live health URL must use the local application binding")
	}
	if c.ControlGID < 0 || c.ProgressGID < 0 {
		return errors.New("invalid updater socket group")
	}
	if inside(c.MaintenanceFile, "/run") || inside(c.MaintenanceFile, "/var/run") || inside(c.MaintenanceFile, c.StateDir) || inside(c.MaintenanceFile, c.DeploymentDir) {
		return errors.New("maintenance gate must persist outside volatile runtime and private application/state directories")
	}
	return nil
}

func LoadConfig(path string) (Config, error) {
	var value Config
	file, err := os.Open(path)
	if err != nil {
		return value, err
	}
	defer func() { _ = file.Close() }()
	decoder := json.NewDecoder(io.LimitReader(file, 64<<10))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&value); err != nil {
		return value, errors.New("invalid updater configuration")
	}
	if err = decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return value, errors.New("invalid updater configuration")
	}
	return value, value.Validate()
}

func inside(path, parent string) bool {
	relative, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(path))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

type Runner interface {
	Run(context.Context, []string, io.Reader, io.Writer) ([]byte, error)
}

type CommandRunner struct{ DiagnosticDir string }

func (r CommandRunner) Run(ctx context.Context, args []string, input io.Reader, output io.Writer) ([]byte, error) {
	if len(args) == 0 {
		return nil, errors.New("empty updater command")
	}
	// The root-only engine/installer constructs fixed argument lists from
	// validated deployment paths and release metadata; callers cannot submit
	// executable names, commands or shell expressions through the protocol.
	command := exec.CommandContext(ctx, args[0], args[1:]...) //nolint:gosec // G702: this is the trusted host executor boundary described above.
	command.Stdin = input
	var stdout, stderr bytes.Buffer
	if output != nil {
		command.Stdout = output
	} else {
		command.Stdout = &stdout
	}
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if r.DiagnosticDir != "" {
			if os.MkdirAll(r.DiagnosticDir, 0o700) == nil {
				_ = os.WriteFile(filepath.Join(r.DiagnosticDir, fmt.Sprintf("command-%d.log", time.Now().UnixNano())), stderr.Bytes(), 0o600)
			}
		}
		return nil, fmt.Errorf("updater command %s failed", filepath.Base(args[0]))
	}
	return stdout.Bytes(), nil
}

type Mount struct {
	Type        string
	Source      string
	Destination string
	RW          bool
}

type Container struct {
	ID           string `json:"Id"`
	Name         string
	Image        string
	RestartCount int
	Config       struct {
		Image      string
		Env        []string
		Cmd        []string
		Entrypoint []string
		Labels     map[string]string
	}
	HostConfig      map[string]any
	NetworkSettings struct {
		Networks map[string]struct{ IPAddress string }
	}
	Mounts []Mount
	State  struct {
		Running bool
		Health  struct{ Status string }
	}
}

func (c Container) Environment() map[string]string {
	result := map[string]string{}
	for _, value := range c.Config.Env {
		parts := strings.SplitN(value, "=", 2)
		if len(parts) == 2 {
			result[parts[0]] = parts[1]
		}
	}
	return result
}

type RuntimeSnapshot struct {
	PostgresData string               `json:"postgres_data"`
	Compose      map[string]any       `json:"compose"`
	Containers   map[string]Container `json:"containers"`
	MountBackups map[string]string    `json:"mount_backups"`
	Roots        []SavedRoot          `json:"roots"`
	Database     DatabaseSnapshot     `json:"database"`
	Redis        RedisSnapshot        `json:"redis"`
	ReadonlyKey  string               `json:"readonly_key"`
}

type UpgradeState struct {
	BackupPath        string           `json:"backup_path"`
	PrivateProject    string           `json:"private_project"`
	PrivateCompose    string           `json:"private_compose"`
	Maintenance       bool             `json:"maintenance"`
	BackupComplete    bool             `json:"backup_complete"`
	ProductionTouched bool             `json:"production_touched"`
	PublicCommitted   bool             `json:"public_committed"`
	PrunedBackups     []string         `json:"pruned_backups,omitempty"`
	Snapshot          *RuntimeSnapshot `json:"snapshot,omitempty"`
	DumpDigest        string           `json:"dump_digest"`
	GlobalsDigest     string           `json:"globals_digest"`
}

type ImageVerifier interface {
	Prepare(context.Context, Manifest) error
}

type Engine struct {
	Now      func() time.Time
	Config   Config
	Runner   Runner
	Source   ReleaseSource
	Verifier ImageVerifier
	Redis    RedisBackend
}

func (e *Engine) run(ctx context.Context, args ...string) ([]byte, error) {
	timeout, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	return e.Runner.Run(timeout, args, nil, nil)
}

func (e *Engine) compose(ctx context.Context, parts ...string) ([]byte, error) {
	args := []string{"docker", "compose", "--project-directory", e.Config.DeploymentDir}
	for _, file := range e.Config.ComposeFiles {
		args = append(args, "-f", file)
	}
	return e.run(ctx, append(args, parts...)...)
}

func (e *Engine) private(ctx context.Context, state *UpgradeState, parts ...string) ([]byte, error) {
	args := []string{"docker", "compose", "-p", state.PrivateProject, "-f", state.PrivateCompose}
	return e.run(ctx, append(args, parts...)...)
}

func (e *Engine) inspect(ctx context.Context, id string) (Container, error) {
	var entries []Container
	value, err := e.run(ctx, "docker", "inspect", id)
	if err != nil {
		return Container{}, err
	}
	if json.Unmarshal(value, &entries) != nil || len(entries) != 1 {
		return Container{}, errors.New("unexpected deployment container")
	}
	return entries[0], nil
}

func (e *Engine) container(ctx context.Context, service string) (Container, error) {
	value, err := e.compose(ctx, "ps", "-q", "--all", service)
	if err != nil {
		return Container{}, err
	}
	id := strings.TrimSpace(string(value))
	if id == "" || strings.ContainsAny(id, " \r\n") {
		return Container{}, errors.New("deployment must have one container per service")
	}
	return e.inspect(ctx, id)
}

func (e *Engine) CurrentVersion(ctx context.Context) (string, error) {
	app, err := e.container(ctx, "sub2api")
	if err != nil {
		return "", err
	}
	version := app.Config.Labels["org.opencontainers.image.version"]
	if _, err = ParseVersion(version); err != nil {
		return "", errors.New("running application is not a labeled fork image")
	}
	return version, nil
}

func (e *Engine) snapshotRuntime(ctx context.Context) (*RuntimeSnapshot, error) {
	value, err := e.compose(ctx, "config", "--format", "json")
	if err != nil {
		return nil, err
	}
	var compose map[string]any
	if json.Unmarshal(value, &compose) != nil {
		return nil, errors.New("invalid compose deployment")
	}
	services, ok := compose["services"].(map[string]any)
	if !ok || len(services) != 3 {
		return nil, errors.New("online upgrade supports the app, PostgreSQL and Redis compose deployment")
	}
	result := &RuntimeSnapshot{Compose: compose, Containers: map[string]Container{}, MountBackups: map[string]string{}}
	for _, service := range []string{"sub2api", "postgres", "redis"} {
		if _, ok = services[service]; !ok {
			return nil, errors.New("required deployment service is missing")
		}
		info, inspectErr := e.container(ctx, service)
		if inspectErr != nil {
			return nil, inspectErr
		}
		if !info.State.Running {
			return nil, errors.New("deployment service is not running")
		}
		result.Containers[service] = info
	}
	app := result.Containers["sub2api"]
	version := app.Config.Labels["org.opencontainers.image.version"]
	if _, err = ParseVersion(version); err != nil || app.State.Health.Status != "healthy" {
		return nil, errors.New("current fork application must be healthy")
	}
	pgEnv := result.Containers["postgres"].Environment()
	if !strings.HasPrefix(pgEnv["PG_MAJOR"], "18") {
		return nil, errors.New("online upgrade requires the existing PostgreSQL 18 deployment")
	}
	dataDirectory, dataErr := e.pg(ctx, result.Containers["postgres"], "SHOW data_directory")
	if dataErr != nil {
		return nil, dataErr
	}
	captured := false
	for _, mount := range result.Containers["postgres"].Mounts {
		if inside(dataDirectory, mount.Destination) {
			captured = true
		}
	}
	if !filepath.IsAbs(dataDirectory) || !captured {
		return nil, errors.New("actual PostgreSQL data directory is not inside a captured mount")
	}
	result.PostgresData = dataDirectory
	for _, info := range result.Containers {
		for _, mount := range info.Mounts {
			if e.managedMount(mount) {
				continue
			}
			if err = validateDataPath(mount.Source, e.Config); err != nil {
				return nil, err
			}
			resolved, resolveErr := filepath.EvalSymlinks(mount.Source)
			if resolveErr != nil || resolved != filepath.Clean(mount.Source) {
				return nil, errors.New("symlinked data mount sources require manual upgrade")
			}
		}
	}
	return result, nil
}

func (e *Engine) managedMount(mount Mount) bool {
	return mount.Source == filepath.Dir(e.Config.ControlSocket) && !mount.RW
}

func validateDataPath(path string, c Config) error {
	if !filepath.IsAbs(path) || path == "/" || inside(c.BackupDir, path) || inside(c.StateDir, path) {
		return errors.New("unsafe deployment data mount")
	}
	for _, parent := range []string{"/etc", "/usr", "/bin", "/sbin", "/lib", "/dev", "/proc", "/sys", "/run"} {
		if inside(path, parent) {
			return errors.New("unsupported system directory data mount")
		}
	}
	if path == "/var/lib/docker" || path == "/var/lib/docker/volumes" {
		return errors.New("unsupported Docker root data mount")
	}
	return nil
}

func (e *Engine) Prepare(ctx context.Context, m Manifest) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if _, err := e.run(ctx, "docker", "pull", ImageRepository+"@"+m.ImageDigest); err != nil {
		return err
	}
	value, err := e.run(ctx, "docker", "image", "inspect", ImageRepository+"@"+m.ImageDigest)
	if err != nil {
		return err
	}
	var images []struct {
		ID           string `json:"Id"`
		Architecture string
		RepoDigests  []string
		Config       struct{ Labels map[string]string }
	}
	if json.Unmarshal(value, &images) != nil || len(images) != 1 {
		return errors.New("invalid pulled image")
	}
	image := images[0]
	labels := image.Config.Labels
	if labels["org.opencontainers.image.source"] != "https://github.com/"+Repository || labels["org.opencontainers.image.version"] != m.Version || labels["org.opencontainers.image.revision"] != m.Revision {
		return errors.New("image source, version or revision does not match release")
	}
	if image.Architecture != runtime.GOARCH {
		return errors.New("image architecture differs from this host")
	}
	found := false
	for _, digest := range image.RepoDigests {
		if digest == ImageRepository+"@"+m.ImageDigest {
			found = true
		}
	}
	if !found {
		return errors.New("pulled image digest differs from release")
	}
	value, err = e.run(ctx, "docker", "manifest", "inspect", ImageRepository+"@"+m.ImageDigest)
	if err != nil {
		return err
	}
	var index struct {
		Manifests []struct {
			Digest   string
			Platform struct {
				OS           string
				Architecture string
			}
		}
	}
	if json.Unmarshal(value, &index) != nil {
		return errors.New("invalid release image index")
	}
	platforms := map[string]bool{}
	for _, entry := range index.Manifests {
		if entry.Platform.OS == "linux" && ValidDigest(entry.Digest) {
			platforms[entry.Platform.Architecture] = true
		}
	}
	if !platforms["amd64"] || !platforms["arm64"] {
		return errors.New("release image is missing a supported architecture")
	}
	return nil
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func cloneJSON(value any) (map[string]any, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	err = json.Unmarshal(body, &result)
	return result, err
}

func escapeCompose(value any) any {
	switch data := value.(type) {
	case string:
		return strings.ReplaceAll(data, "$", "$$")
	case map[string]any:
		result := map[string]any{}
		for key, item := range data {
			result[key] = escapeCompose(item)
		}
		return result
	case []any:
		result := make([]any, len(data))
		for i, item := range data {
			result[i] = escapeCompose(item)
		}
		return result
	}
	return value
}
