//go:build unit

package kanoupdate

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestScheduleClientOnlyUsesRestrictedControlRoutes(t *testing.T) {
	ctx := context.Background()
	id := "up-" + strings.Repeat("a", 32)
	request := StartRequest{Version: "v0.2.16-2", ExpectedDigest: "sha256:" + strings.Repeat("b", 64), Actor: "admin:1", IdempotencyKey: "one-reservation"}
	var routes []string
	client := &Client{HTTP: &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		routes = append(routes, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodPost {
			var actual StartRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&actual))
			require.Equal(t, request, actual)
		}
		body := `{"job":{"operation_id":"` + id + `","status":"scheduled"}}`
		if r.Method == http.MethodDelete {
			body = `{"cancelled":true}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}}
	session, err := client.CreateSchedule(ctx, request)
	require.NoError(t, err)
	require.Equal(t, Scheduled, session.Job.Status)
	session, err = client.Schedule(ctx)
	require.NoError(t, err)
	require.Equal(t, id, session.Job.ID)
	require.NoError(t, client.CancelSchedule(ctx, id))
	require.Equal(t, []string{"POST /schedule", "GET /schedule", "DELETE /schedule/" + id}, routes)
}
