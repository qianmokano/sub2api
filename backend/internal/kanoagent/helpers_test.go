//go:build unit

package kanoagent

import (
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/kanoupdate"
)

func validManifest() Manifest {
	return Manifest{SchemaVersion: 1, Version: "v0.2.16-2", Image: ImageRepository + ":v0.2.16-2", ImageDigest: "sha256:" + strings.Repeat("a", 64), Revision: strings.Repeat("b", 40), MinimumAgentVersion: AgentVersion, Migrations: map[string]string{"241_sso_issuer_independence.sql": strings.Repeat("c", 64)}, Validation: ValidationPolicy{MinimumSourceVersion: "v0.2.16-1"}}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

var NewClient = kanoupdate.NewClient

type Client = kanoupdate.Client
type RemoteError = kanoupdate.RemoteError
