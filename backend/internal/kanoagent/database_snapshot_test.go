//go:build unit

package kanoagent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDatabaseSnapshotRejectsIncompleteOrInvalidEvidence(t *testing.T) {
	for _, fault := range []string{"query", "tables", "columns", "fingerprint", "count", "migrations", "settings"} {
		t.Run(fault, func(t *testing.T) {
			engine := &Engine{Runner: runnerFunc(func(_ context.Context, args []string, _ io.Reader, _ io.Writer) ([]byte, error) {
				query := args[len(args)-1]
				switch {
				case strings.Contains(query, "FROM pg_tables"):
					if fault == "query" {
						return nil, errors.New("connection lost")
					}
					if fault == "tables" {
						return []byte(`invalid`), nil
					}
					return []byte(`["users"]`), nil
				case strings.Contains(query, "information_schema.columns"):
					if fault == "columns" {
						return []byte(`[]`), nil
					}
					return []byte(`[{"name":"id","type":"int8","nullable":false,"default":""}]`), nil
				case strings.Contains(query, "md5(row_to_json"):
					if fault == "fingerprint" {
						return []byte("1|2"), nil
					}
					if fault == "count" {
						return []byte("invalid|2|3"), nil
					}
					return []byte("1|2|3"), nil
				case strings.Contains(query, "jsonb_object_agg(filename"):
					if fault == "migrations" {
						return []byte(`invalid`), nil
					}
					body, _ := json.Marshal(validManifest().Migrations)
					return body, nil
				default:
					if fault == "settings" {
						return []byte(`invalid`), nil
					}
					return []byte(`{}`), nil
				}
			})}
			container := Container{ID: "postgres"}
			container.Config.Env = []string{"POSTGRES_USER=postgres", "POSTGRES_DB=sub2api"}
			_, err := engine.database(context.Background(), container, nil)
			require.Error(t, err)
		})
	}
}

func TestRecoveryRejectsRootOutsideCapturedDeployment(t *testing.T) {
	e, _, job, _ := newDeploymentFixture(t, "")
	state := &UpgradeState{Snapshot: &RuntimeSnapshot{}, BackupPath: e.Config.BackupDir + "/test", PrivateProject: "kano-verify-" + strings.TrimPrefix(job.ID, "up-")}
	state.PrivateCompose = state.BackupPath + "/private.json"
	state.Snapshot.Roots = []SavedRoot{{Source: "/opt/another-application", Saved: state.BackupPath + "/original"}}
	require.ErrorContains(t, e.validateCheckpoint(job, state), "not a captured")
	state.BackupPath = e.Config.BackupDir
	require.Error(t, e.validateCheckpoint(job, state))
	job.State = json.RawMessage(`invalid`)
	require.Error(t, e.Execute(context.Background(), nil, job))
}
