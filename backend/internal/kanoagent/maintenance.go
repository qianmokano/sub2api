package kanoagent

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/kanoupdate"
)

//go:embed maintenance.html
var maintenanceHTML string

func atomicPublicJSON(path string, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return atomicFile(path, body, 0o644)
}

func (s *Server) publicStatus(ctx context.Context) (kanoupdate.MaintenanceStatus, error) {
	var status kanoupdate.MaintenanceStatus
	if s.MaintenanceFile != "" {
		_, err := os.Stat(s.MaintenanceFile)
		if err != nil && !os.IsNotExist(err) {
			return status, err
		}
		status.Maintenance = err == nil
	}
	job, err := s.Store.Current(ctx)
	if err == ErrNotFound {
		return status, nil
	}
	if err != nil {
		return status, err
	}
	if job.Status == kanoupdate.Scheduled {
		status.ScheduledAt = job.ScheduledAt
	}
	if status.Maintenance {
		for _, step := range job.Steps {
			if step.Status == BackingUp {
				at := step.StartedAt
				status.StartedAt = &at
				break
			}
		}
		switch job.Status {
		case BackingUp:
			status.Phase = "backup"
		case Restoring, Verifying:
			status.Phase = "validation"
		case Switching, Checking:
			status.Phase = "starting"
		case RollingBack:
			status.Phase = "recovery"
		default:
			status.Phase = "attention"
		}
	}
	return status, nil
}

func maintenancePolicy() string {
	var hashes []string
	for _, tag := range []string{"script", "style"} {
		_, body, _ := strings.Cut(maintenanceHTML, "<"+tag+">")
		body, _, _ = strings.Cut(body, "</"+tag+">")
		sum := sha256.Sum256([]byte(body))
		hashes = append(hashes, tag+"-src 'sha256:"+base64.StdEncoding.EncodeToString(sum[:])+"'")
	}
	return "default-src 'none'; " + strings.Join(hashes, "; ") + "; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'"
}

func maintenanceSnippets(config Config) (string, string) {
	publicDir := filepath.Dir(config.MaintenanceFile)
	progress := fmt.Sprintf(`location ^~ /kano-updates/ {
    proxy_pass http://unix:%s:;
    proxy_set_header Host $host;
    proxy_buffering off;
    proxy_read_timeout 15s;
    access_log off;
}
location = /kano-maintenance/status {
    proxy_pass http://unix:%s:;
    proxy_set_header Cookie "";
    proxy_set_header Authorization "";
    proxy_pass_request_body off;
    proxy_set_header Content-Length "";
    proxy_connect_timeout 1s;
    proxy_read_timeout 2s;
    proxy_intercept_errors on;
    error_page 502 503 504 =200 /kano-maintenance/fallback;
    access_log off;
}
location = /kano-maintenance/fallback {
    internal;
    default_type application/json;
    add_header Cache-Control "no-store" always;
    if (!-f %s) { return 200 '{"maintenance":false}'; }
    root %s;
    try_files /maintenance-status.json =503;
}
location @kano_maintenance {
    default_type application/json;
    add_header Retry-After "60" always;
    add_header Cache-Control "no-store, max-age=0" always;
    add_header X-Kano-Maintenance "true" always;
    add_header X-Content-Type-Options "nosniff" always;
    add_header Content-Security-Policy "%s" always;
    if ($request_method != GET) { return 503 '{"error":{"type":"maintenance","message":"Service temporarily unavailable"}}'; }
    if ($uri ~ "^/(api|v1|v1beta|v3|backend-api|antigravity|responses|chat|messages|models|contents|health)(/|$)") { return 503 '{"error":{"type":"maintenance","message":"Service temporarily unavailable"}}'; }
    if ($http_upgrade != "") { return 503 '{"error":{"type":"maintenance","message":"Service temporarily unavailable"}}'; }
    if ($http_accept !~* "text/html") { return 503 '{"error":{"type":"maintenance","message":"Service temporarily unavailable"}}'; }
    root %s;
    types { text/html html; }
    try_files /maintenance.html =503;
}
`, config.ProgressSocket, config.ProgressSocket, config.MaintenanceFile, publicDir, maintenancePolicy(), publicDir)
	gate := fmt.Sprintf("error_page 503 =503 @kano_maintenance;\nif (-f %s) { return 503; }\n", config.MaintenanceFile)
	return progress, gate
}
