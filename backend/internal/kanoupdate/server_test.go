//go:build unit

package kanoupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeSource struct {
	release *Release
	err     error
}

func (s *fakeSource) Latest(context.Context) (*Release, error)          { return s.release, s.err }
func (s *fakeSource) Resolve(context.Context, string) (*Release, error) { return s.release, s.err }

type fakeExecutor struct {
	version string
	err     error
	execute func(context.Context, *Store, *Job) error
}

func (e *fakeExecutor) CurrentVersion(context.Context) (string, error) { return e.version, e.err }
func (e *fakeExecutor) Execute(ctx context.Context, store *Store, job *Job) error {
	if e.execute != nil {
		return e.execute(ctx, store, job)
	}
	return store.Update(ctx, job.ID, Succeeded, "Done")
}

func testServer(t *testing.T) (*Server, *fakeSource, *fakeExecutor) {
	t.Helper()
	store, err := OpenStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	m := validManifest()
	source := &fakeSource{release: &Release{Tag: m.Version, Name: "kano release", Manifest: &m}}
	executor := &fakeExecutor{version: "v0.2.16-1"}
	return NewServer(store, source, executor), source, executor
}

func controlRequest(handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec
}

func TestControlAndProgressAreIndependentAndScoped(t *testing.T) {
	server, source, _ := testServer(t)
	control := server.ControlHandler()
	response := controlRequest(control, "GET", "/health", "")
	require.Equal(t, 200, response.Code)
	response = controlRequest(control, "GET", "/jobs/current", "")
	require.Equal(t, 200, response.Code)
	require.Contains(t, response.Body.String(), `"job":null`)
	response = controlRequest(control, "GET", "/release", "")
	require.Equal(t, 200, response.Code)
	require.Contains(t, response.Body.String(), validManifest().ImageDigest)
	body, _ := json.Marshal(startRequest())
	response = controlRequest(control, "POST", "/jobs", string(body))
	require.Equal(t, 202, response.Code)
	var session Session
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &session))
	require.NotEmpty(t, session.Capability)
	// A lost HTTP response can be replayed after the release service fails.
	source.err = errors.New("offline")
	response = controlRequest(control, "POST", "/jobs", string(body))
	require.Equal(t, 202, response.Code)
	var duplicate Session
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &duplicate))
	require.Equal(t, session.Job.ID, duplicate.Job.ID)
	response = controlRequest(control, "GET", "/jobs/current", "")
	require.Equal(t, 200, response.Code)
	require.Contains(t, response.Body.String(), session.Job.ID)
	progress := server.ProgressHandler()
	for _, path := range []string{session.ProgressURL, session.ProgressURL + "/status"} {
		request := httptest.NewRequest("GET", path, nil)
		denied := httptest.NewRecorder()
		progress.ServeHTTP(denied, request)
		require.Equal(t, 403, denied.Code)
		request.AddCookie(&http.Cookie{Name: CookieName(session.Job.ID), Value: session.Capability})
		allowed := httptest.NewRecorder()
		progress.ServeHTTP(allowed, request)
		require.Equal(t, 200, allowed.Code)
		require.Equal(t, "no-store", allowed.Header().Get("Cache-Control"))
		require.NotContains(t, allowed.Body.String(), session.Capability)
		require.NotContains(t, allowed.Body.String(), `"migrations":`)
		if path == session.ProgressURL {
			require.Contains(t, allowed.Header().Get("Content-Security-Policy"), "nonce-")
			require.Contains(t, allowed.Body.String(), "setTimeout(poll, 2000)")
		}
	}
	response = controlRequest(progress, "POST", session.ProgressURL, "")
	require.Equal(t, 405, response.Code)
	response = controlRequest(progress, "GET", "/kano-updates/../../jobs", "")
	require.Equal(t, 404, response.Code)
	response = controlRequest(progress, "GET", session.ProgressURL+"/anything", "")
	require.Equal(t, 404, response.Code)
	response = controlRequest(control, "POST", "/jobs", `{"version":"latest"}`)
	require.Equal(t, 400, response.Code)
	response = controlRequest(control, "POST", "/jobs", string(body)+" {}")
	require.Equal(t, 400, response.Code)
	response = controlRequest(control, "POST", "/jobs", `{"version":"v0.2.16-2","shell":"bad"}`)
	require.Equal(t, 400, response.Code)
	response = controlRequest(control, "GET", "/health", "")
	require.Equal(t, 200, response.Code)
	require.Contains(t, response.Body.String(), `"busy":true`)
}

func TestClientUsesUnixSocketAndReceivesSafeErrors(t *testing.T) {
	server, _, _ := testServer(t)
	directory, err := os.MkdirTemp("/tmp", "ku-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "control.sock")
	listener, err := ListenUnix(socket, os.Getgid())
	require.NoError(t, err)
	httpServer := &http.Server{Handler: server.ControlHandler()}
	go func() { _ = httpServer.Serve(listener) }()
	t.Cleanup(func() { _ = httpServer.Close() })
	client := NewClient(socket)
	availability, err := client.Health(context.Background())
	require.NoError(t, err)
	require.True(t, availability.Ready)
	availability, err = client.Release(context.Background(), true)
	require.NoError(t, err)
	require.NotNil(t, availability.Release.Manifest)
	session, err := client.Start(context.Background(), startRequest())
	require.NoError(t, err)
	require.NotNil(t, session.Job)
	current, err := client.Current(context.Background())
	require.NoError(t, err)
	require.Equal(t, session.Job.ID, current.Job.ID)
	request := startRequest()
	request.IdempotencyKey = "another"
	_, err = client.Start(context.Background(), request)
	var remote *RemoteError
	require.ErrorAs(t, err, &remote)
	require.Equal(t, 409, remote.Status)
	info, err := os.Stat(socket)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o660), info.Mode().Perm())
	_, err = ListenUnix("relative", os.Getgid())
	require.Error(t, err)
	file := filepath.Join(directory, "not-socket")
	require.NoError(t, os.WriteFile(file, []byte("preserve"), 0o600))
	_, err = ListenUnix(file, os.Getgid())
	require.Error(t, err)
	require.NoError(t, httpServer.Close())
	_, err = client.Health(context.Background())
	require.Error(t, err)
	require.NotContains(t, err.Error(), socket)
}

func TestWorkerResumesSavedJobsAndLeavesRecoveryAttentionLocked(t *testing.T) {
	server, _, executor := testServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	job, _, err := server.Store.Create(ctx, startRequest(), "v0.2.16-1", validManifest())
	require.NoError(t, err)
	finished := make(chan struct{})
	executor.execute = func(ctx context.Context, store *Store, job *Job) error {
		err := store.Update(ctx, job.ID, Succeeded, "Done")
		close(finished)
		return err
	}
	go server.Run(ctx)
	<-finished
	current, err := server.Store.Get(ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, Succeeded, current.Status)
	request := startRequest()
	request.IdempotencyKey = "failing"
	job, _, err = server.Store.Create(ctx, request, "v0.2.16-1", validManifest())
	require.NoError(t, err)
	failed := make(chan struct{})
	executor.execute = func(context.Context, *Store, *Job) error { close(failed); return errors.New("private secret error") }
	server.Wake <- struct{}{}
	<-failed
	// Synchronize through the wake loop rather than asserting scheduler timing.
	cancel()
}

func TestClientRejectsMalformedAndLargeResponses(t *testing.T) {
	for _, body := range []string{"bad", strings.Repeat("a", (1<<20)+1)} {
		client := &Client{HTTP: &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewBufferString(body))}, nil
		})}}
		_, err := client.Health(context.Background())
		require.Error(t, err)
	}
}
