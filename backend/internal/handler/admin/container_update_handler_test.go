//go:build unit

package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/kanoupdate"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type containerHandlerStub struct {
	systemHandlerUpdateServiceStub
	request kanoupdate.StartRequest
	session *kanoupdate.Session
	err     error
}

func (s *containerHandlerStub) ContainerMode() bool { return true }
func (s *containerHandlerStub) ContainerStart(_ context.Context, request kanoupdate.StartRequest) (*kanoupdate.Session, error) {
	s.request = request
	return s.session, s.err
}
func (s *containerHandlerStub) ContainerCurrent(context.Context) (*kanoupdate.Session, error) {
	return s.session, s.err
}

func containerHandlerRequest(t *testing.T, handler gin.HandlerFunc, authenticated bool, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Any("/", func(c *gin.Context) {
		if authenticated {
			c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 42})
		}
		handler(c)
	})
	r := httptest.NewRequest("POST", "/", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	return w
}

func TestContainerHandlerAcceptsAsyncScopedProgressWithoutDatabaseLock(t *testing.T) {
	id := "up-" + strings.Repeat("a", 32)
	token := strings.Repeat("b", 64)
	stub := &containerHandlerStub{session: &kanoupdate.Session{Job: &kanoupdate.Job{ID: id}, Capability: token, ProgressURL: "/kano-updates/" + id}}
	handler := NewSystemHandler(stub, nil)
	response := containerHandlerRequest(t, handler.PerformUpdate, true, "same-request", `{"version":"v0.2.16-2","expected_digest":"sha256:test"}`)
	require.Equal(t, 202, response.Code)
	require.Equal(t, "admin:42", stub.request.Actor)
	require.Equal(t, "same-request", stub.request.IdempotencyKey)
	require.NotContains(t, response.Body.String(), token)
	require.Contains(t, response.Body.String(), id)
	cookie := response.Result().Cookies()[0]
	require.True(t, cookie.Secure)
	require.True(t, cookie.HttpOnly)
	require.Equal(t, http.SameSiteStrictMode, cookie.SameSite)
	require.Equal(t, stub.session.ProgressURL, cookie.Path)
	require.Equal(t, token, cookie.Value)
	require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	response = containerHandlerRequest(t, handler.CurrentUpdate, true, "", "")
	require.Equal(t, 200, response.Code)
	require.NotContains(t, response.Body.String(), token)
	for _, method := range []gin.HandlerFunc{handler.Rollback, handler.RestartService} {
		response = containerHandlerRequest(t, method, true, "", "")
		require.Equal(t, 403, response.Code)
	}
	require.Zero(t, stub.performCall)
	require.Zero(t, stub.rollbackCall)
}

func TestContainerHandlerRejectsMissingIdentityKeyAndInvalidProgress(t *testing.T) {
	stub := &containerHandlerStub{}
	handler := NewSystemHandler(stub, nil)
	response := containerHandlerRequest(t, handler.PerformUpdate, false, "key", `{}`)
	require.Equal(t, 401, response.Code)
	response = containerHandlerRequest(t, handler.PerformUpdate, true, "", `{}`)
	require.Equal(t, 400, response.Code)
	response = containerHandlerRequest(t, handler.PerformUpdate, true, "key", `bad`)
	require.Equal(t, 400, response.Code)
	response = containerHandlerRequest(t, handler.PerformUpdate, true, "key", `{}`)
	require.Equal(t, 503, response.Code)
	stub.err = &kanoupdate.RemoteError{Status: 409, Message: "Upgrade already in progress"}
	response = containerHandlerRequest(t, handler.PerformUpdate, true, "key", `{}`)
	require.Equal(t, 409, response.Code)
	stub.err = nil
	stub.err = errors.New("private transport details")
	response = containerHandlerRequest(t, handler.PerformUpdate, true, "key", `{}`)
	require.Equal(t, 503, response.Code)
	require.NotContains(t, response.Body.String(), "private transport details")
	stub.err = nil
	stub.session = &kanoupdate.Session{}
	response = containerHandlerRequest(t, handler.CurrentUpdate, true, "", "")
	require.Equal(t, 200, response.Code)
	require.Contains(t, response.Body.String(), `"job":null`)
	stub.session = &kanoupdate.Session{Job: &kanoupdate.Job{ID: "invalid"}, Capability: strings.Repeat("c", 64), ProgressURL: "https://evil.test"}
	response = containerHandlerRequest(t, handler.CurrentUpdate, true, "", "")
	require.Equal(t, 503, response.Code)
	require.Empty(t, response.Result().Cookies())
}
