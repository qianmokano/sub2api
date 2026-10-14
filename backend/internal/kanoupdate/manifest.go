package kanoupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"strings"
)

// GenerateManifest uses the same trimmed SQL checksum convention as the
// application's migration runner. Policy is reviewed alongside source changes.
func GenerateManifest(migrations fs.FS, policy ValidationPolicy, version, digest, revision string) (Manifest, error) {
	manifest := Manifest{SchemaVersion: ProtocolVersion, Version: version, Image: ImageRepository + ":" + version, ImageDigest: digest, Revision: revision, MinimumAgentVersion: AgentVersion, Validation: policy, Migrations: map[string]string{}}
	files, err := fs.Glob(migrations, "*.sql")
	if err != nil {
		return manifest, err
	}
	for _, name := range files {
		body, readErr := fs.ReadFile(migrations, name)
		if readErr != nil {
			return manifest, readErr
		}
		content := strings.TrimSpace(string(body))
		if content == "" {
			continue
		}
		checksum := sha256.Sum256([]byte(content))
		manifest.Migrations[name] = hex.EncodeToString(checksum[:])
	}
	return manifest, manifest.Validate()
}
