//go:build unit

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/kanoupdate"
	"github.com/stretchr/testify/require"
)

func TestCLIManifestAndVersion(t *testing.T) {
	var output bytes.Buffer
	require.NoError(t, run(context.Background(), []string{"version"}, &output))
	require.Equal(t, kanoupdate.AgentVersion+"\n", output.String())
	root := t.TempDir()
	policy := filepath.Join(root, "policy.json")
	require.NoError(t, os.WriteFile(policy, []byte(`{"minimum_source_version":"v0.2.16-1"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "001.sql"), []byte("SELECT 1;"), 0o600))
	output.Reset()
	args := []string{"manifest", "--policy", policy, "--migrations", root, "--version", "v0.2.16-2", "--digest", "sha256:" + strings.Repeat("a", 64), "--revision", strings.Repeat("b", 40)}
	require.NoError(t, run(context.Background(), args, &output))
	var manifest kanoupdate.Manifest
	require.NoError(t, json.Unmarshal(output.Bytes(), &manifest))
	require.NoError(t, manifest.Validate())
	require.NoError(t, os.WriteFile(policy, []byte(`{"unexpected":true}`), 0o600))
	require.Error(t, run(context.Background(), args, &output))
	for _, arguments := range [][]string{nil, {"invalid"}, {"manifest", "--unknown"}, {"manifest", "positional"}, {"manifest", "--policy", "/missing-policy"}} {
		require.Error(t, run(context.Background(), arguments, &output))
	}
}
