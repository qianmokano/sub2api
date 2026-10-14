//go:build unit

package kanoupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
)

func TestGenerateManifestMatchesMigrationRunner(t *testing.T) {
	files := fstest.MapFS{"001.sql": {Data: []byte("\n SELECT 1; \n")}, "empty.sql": {Data: []byte("\n\t")}, "ignored.txt": {Data: []byte("ignored")}}
	base := validManifest()
	result, err := GenerateManifest(files, base.Validation, base.Version, base.ImageDigest, base.Revision)
	require.NoError(t, err)
	checksum := sha256.Sum256([]byte("SELECT 1;"))
	require.Equal(t, map[string]string{"001.sql": hex.EncodeToString(checksum[:])}, result.Migrations)
	_, err = GenerateManifest(fstest.MapFS{}, base.Validation, base.Version, base.ImageDigest, base.Revision)
	require.Error(t, err)
	_, err = GenerateManifest(files, base.Validation, "v0.2.16", "wrong", base.Revision)
	require.Error(t, err)
}
