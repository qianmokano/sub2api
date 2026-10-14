//go:build unit

package kanoagent

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNginxWiringPreservesTLSAndOnlyGatesApplicationProxy(t *testing.T) {
	original := `server {
  listen 80;
  server_name api.kanoapi.top;
  return 301 https://$host$request_uri;
}
server {
  listen 443 ssl;
  ssl_certificate /etc/letsencrypt/live/api.kanoapi.top/fullchain.pem;
  # comments with } and quoted braces must survive
  set $example "}";
  location / {
    proxy_pass http://127.0.0.1:8081;
    proxy_set_header Host $host;
  }
}
`
	result, err := wireNginx(original, "http://127.0.0.1:8081", "/var/lib/progress.conf", "/var/lib/gate.conf")
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(result, "include /var/lib/progress.conf;"))
	require.Equal(t, 1, strings.Count(result, "include /var/lib/gate.conf;"))
	require.Contains(t, result, "ssl_certificate /etc/letsencrypt/live/api.kanoapi.top/fullchain.pem;")
	require.Contains(t, result, "return 301 https://$host$request_uri;")
	require.Contains(t, result, "proxy_set_header Host $host;")
	again, err := wireNginx(result, "http://127.0.0.1:8081", "/var/lib/progress.conf", "/var/lib/gate.conf")
	require.NoError(t, err)
	require.Equal(t, result, again)
	_, err = wireNginx(original, "http://127.0.0.1:9090", "p", "g")
	require.Error(t, err)
	_, err = nginxClosingBrace("server {", 7)
	require.Error(t, err)
}

func TestInstallationValidatesNginxAndRestoresOriginalOnFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "validation_failure"}[fail], func(t *testing.T) {
			config := testConfig(t)
			overlay := filepath.Join(config.DeploymentDir, "kano-updater.compose.json")
			config.ComposeFiles = []string{config.ComposeFiles[0], overlay, config.ImageOverride}
			original := []byte("server {\nlocation / {\n proxy_pass " + config.LiveURL + ";\n}\n}\n")
			site := filepath.Join(t.TempDir(), "site.conf")
			require.NoError(t, os.WriteFile(site, original, 0o644))
			runner := runnerFunc(func(_ context.Context, args []string, _ io.Reader, _ io.Writer) ([]byte, error) {
				require.Equal(t, []string{"nginx", "-t"}, args)
				if fail {
					return nil, errors.New("invalid configuration")
				}
				return nil, nil
			})
			err := ConnectDeployment(context.Background(), config, site, runner)
			body, readErr := os.ReadFile(site)
			require.NoError(t, readErr)
			if fail {
				require.Error(t, err)
				require.Equal(t, original, body)
			} else {
				require.NoError(t, err)
				require.Contains(t, string(body), "include ")
			}
			require.FileExists(t, overlay)
			require.FileExists(t, config.ImageOverride)
			entries, err := os.ReadDir(config.StateDir)
			require.NoError(t, err)
			require.Len(t, entries, 1)
			require.FileExists(t, filepath.Join(config.StateDir, entries[0].Name(), "nginx-original.conf"))
		})
	}
}
