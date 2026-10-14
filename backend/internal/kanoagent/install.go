package kanoagent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ConnectDeployment adds a socket overlay and independent progress route to an
// existing host deployment. It leaves activation to the installation script.
func ConnectDeployment(ctx context.Context, config Config, nginxSite string, runner Runner) error {
	if err := config.Validate(); err != nil {
		return err
	}
	site, err := filepath.EvalSymlinks(nginxSite)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(site) {
		return errors.New("nginx site must be an absolute existing path")
	}
	original, err := os.ReadFile(site)
	if err != nil {
		return err
	}
	progressSnippet := filepath.Join(filepath.Dir(config.StateDir), "kano-updater-nginx-progress.conf")
	// The snippets are configuration, not job state; root owns this parent.
	gateSnippet := filepath.Join(filepath.Dir(config.StateDir), "kano-updater-nginx-maintenance.conf")
	modified, err := wireNginx(string(original), config.LiveURL, progressSnippet, gateSnippet)
	if err != nil {
		return err
	}
	overlayPath := filepath.Join(config.DeploymentDir, "kano-updater.compose.json")
	found := false
	for _, path := range config.ComposeFiles {
		if path == overlayPath {
			found = true
		}
	}
	if !found {
		return errors.New("fixed compose list must include kano-updater.compose.json before the image override")
	}
	backup := filepath.Join(config.StateDir, "installation-"+time.Now().UTC().Format("20060102T150405.000000000Z"))
	if err = os.MkdirAll(backup, 0o700); err != nil {
		return err
	}
	if err = atomicFile(filepath.Join(backup, "nginx-original.conf"), original, 0o600); err != nil {
		return err
	}
	for _, path := range config.ComposeFiles {
		body, readErr := os.ReadFile(path)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return readErr
		}
		if err = atomicFile(filepath.Join(backup, filepath.Base(path)), body, 0o600); err != nil {
			return err
		}
	}
	overlay := map[string]any{"services": map[string]any{"sub2api": map[string]any{
		"environment": map[string]string{"UPDATE_CONTAINER_ENABLED": "true", "UPDATE_SOCKET_PATH": "/run/kano-updater/control/agent.sock"},
		"volumes":     []any{map[string]any{"type": "bind", "source": filepath.Dir(config.ControlSocket), "target": "/run/kano-updater/control", "read_only": true}},
	}}}
	if err = writePrivateJSON(overlayPath, overlay); err != nil {
		return err
	}
	if _, err = os.Stat(config.ImageOverride); errors.Is(err, os.ErrNotExist) {
		if err = writePrivateJSON(config.ImageOverride, map[string]any{"services": map[string]any{}}); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	progress := fmt.Sprintf("location ^~ /kano-updates/ {\n    proxy_pass http://unix:%s:;\n    proxy_set_header Host $host;\n    proxy_buffering off;\n    proxy_read_timeout 15s;\n    access_log off;\n}\n", config.ProgressSocket)
	gate := fmt.Sprintf("if (-f %s) { return 503; }\n", config.MaintenanceFile)
	if err = atomicFile(progressSnippet, []byte(progress), 0o644); err != nil {
		return err
	}
	if err = atomicFile(gateSnippet, []byte(gate), 0o644); err != nil {
		return err
	}
	if err = atomicFile(site, []byte(modified), 0o644); err != nil {
		return err
	}
	if _, err = runner.Run(ctx, []string{"nginx", "-t"}, nil, nil); err != nil {
		if restoreErr := atomicFile(site, original, 0o644); restoreErr != nil {
			return errors.New("nginx validation failed and its original configuration needs manual restoration")
		}
		return errors.New("nginx validation failed; original site configuration restored")
	}
	return nil
}

var nginxServerPattern = regexp.MustCompile(`(?m)^\s*server\s*\{`)

// This intentionally supports ordinary server/location sites. nginx -t is the
// authority before activation; complicated proxy layouts fail without reload.
func wireNginx(source, liveURL, progressSnippet, gateSnippet string) (string, error) {
	if strings.Contains(source, "include "+progressSnippet+";") && strings.Contains(source, "include "+gateSnippet+";") {
		return source, nil
	}
	proxy := regexp.MustCompile(`(?m)^([\t ]*)proxy_pass\s+` + regexp.QuoteMeta(liveURL) + `/?\s*;`)
	servers := nginxServerPattern.FindAllStringIndex(source, -1)
	result := source
	changed := false
	for index := len(servers) - 1; index >= 0; index-- {
		start := servers[index][0]
		open := servers[index][1] - 1
		end, err := nginxClosingBrace(source, open)
		if err != nil {
			return "", err
		}
		block := source[start : end+1]
		if !proxy.MatchString(block) {
			continue
		}
		block = proxy.ReplaceAllString(block, "${1}include "+gateSnippet+";\n${0}")
		block = block[:len(block)-1] + "    include " + progressSnippet + ";\n}"
		result = result[:start] + block + result[end+1:]
		changed = true
	}
	if !changed {
		return "", errors.New("nginx site has no supported proxy for the fixed application binding")
	}
	return result, nil
}

func nginxClosingBrace(source string, start int) (int, error) {
	depth := 0
	quote := byte(0)
	comment := false
	escaped := false
	for index := start; index < len(source); index++ {
		character := source[index]
		if comment {
			if character == '\n' {
				comment = false
			}
			continue
		}
		if escaped {
			escaped = false
			continue
		}
		if quote != 0 {
			switch character {
			case '\\':
				escaped = true
			case quote:
				quote = 0
			}
			continue
		}
		switch character {
		case '#':
			comment = true
		case '\'', '"':
			quote = character
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return index, nil
			}
		}
	}
	return 0, errors.New("nginx server block is incomplete")
}
