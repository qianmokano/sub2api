package casdoor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeAccount(w http.ResponseWriter, identity Identity) {
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "data": identity})
}

func TestClientPasswordLoginAndRegistration(t *testing.T) {
	account := Identity{Subject: "stable-subject", Owner: "kano", Email: "USER@example.com", EmailVerified: true}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		require.Equal(t, "sub2api", payload["application"])
		require.Equal(t, "kano", payload["organization"])
		require.Equal(t, "user", payload["username"])
		require.Equal(t, " password ", payload["password"], "password spaces are significant")
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "authenticated", Path: "/"})
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/api/signup", func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		require.Equal(t, "123456", payload["emailCode"])
		require.Equal(t, "u_random", payload["username"])
		require.Equal(t, "User", payload["name"])
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "authenticated", Path: "/"})
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/api/get-account", func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("session")
		require.NoError(t, err)
		require.Equal(t, "authenticated", cookie.Value)
		writeAccount(w, account)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := New(Config{Issuer: srv.URL + "/", Organization: "kano", Application: "admin/sub2api"}, nil)
	identity, challenge, err := client.Login(context.Background(), " user ", " password ")
	require.NoError(t, err)
	require.Nil(t, challenge)
	require.Equal(t, "stable-subject", identity.Subject)
	require.Equal(t, "user@example.com", identity.Email)
	identity, err = client.Register(context.Background(), "user@example.com", "password", "123456", "User", "u_random")
	require.NoError(t, err)
	require.Equal(t, account.Subject, identity.Subject)
}

func TestClientMFARetryPreservesRotatedCookie(t *testing.T) {
	attempt := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		if payload["password"] != nil {
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "pending", Path: "/"})
			_, _ = w.Write([]byte(`{"status":"ok","msg":"NextMfa","data":[{"mfaType":"otp"}]}`))
			return
		}
		cookie, err := r.Cookie("session")
		require.NoError(t, err)
		attempt++
		if attempt == 1 {
			require.Equal(t, "pending", cookie.Value)
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "rotated", Path: "/"})
			_, _ = w.Write([]byte(`{"status":"error","msg":"mfa code is incorrect"}`))
			return
		}
		require.Equal(t, "rotated", cookie.Value)
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "authenticated", Path: "/"})
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/api/get-account", func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("session")
		require.NoError(t, err)
		require.Equal(t, "authenticated", cookie.Value)
		writeAccount(w, Identity{Subject: "subject", Owner: "kano", Email: "u@example.com"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := New(Config{Issuer: srv.URL, Organization: "kano", Application: "kano/sub2api"}, nil)
	identity, session, err := client.Login(context.Background(), "user", "password")
	require.NoError(t, err)
	require.Nil(t, identity)
	require.Equal(t, []MFAMethod{{Type: "otp"}}, session.Methods)
	_, err = client.CompleteMFA(context.Background(), session, "sms", "123456")
	require.ErrorIs(t, err, ErrMFACode)
	_, err = client.CompleteMFA(context.Background(), session, "otp", "wrong")
	require.ErrorIs(t, err, ErrMFACode)
	identity, err = client.CompleteMFA(context.Background(), session, "otp", "123456")
	require.NoError(t, err)
	require.Equal(t, "subject", identity.Subject)
	_, err = client.CompleteMFA(context.Background(), nil, "otp", "123456")
	require.ErrorIs(t, err, ErrIdentity)
}

func TestClientRejectsInvalidAccountsAndChallenges(t *testing.T) {
	for _, body := range []string{
		`{"status":"error","msg":"sensitive provider text"}`,
		`{"status":"ok","data":{"id":"subject","owner":"other","email":"u@example.com"}}`,
		`{"status":"ok","data":{"id":"subject","owner":"kano","email":"u@example.com","isForbidden":true}}`,
		`{"status":"ok","data":{"id":"subject","owner":"kano","email":"u@example.com","isDeleted":true}}`,
		`{"status":"ok","data":{"owner":"kano","email":"u@example.com"}}`,
	} {
		t.Run(body, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/login" {
					_, _ = w.Write([]byte(`{"status":"ok"}`))
					return
				}
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()
			_, _, err := New(Config{Issuer: srv.URL, Organization: "kano"}, nil).Login(context.Background(), "u", "pw")
			require.ErrorIs(t, err, ErrIdentity)
			require.NotContains(t, err.Error(), "sensitive")
		})
	}
	for _, body := range []string{`{"status":"ok","msg":"NextMfa","data":{}}`, `{"status":"ok","msg":"NextMfa","data":[]}`, `{"status":"ok","msg":"NextMfa","data":[{"mfaType":""}]}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
		_, _, err := New(Config{Issuer: srv.URL}, nil).Login(context.Background(), "u", "pw")
		require.ErrorIs(t, err, ErrIdentity)
		srv.Close()
	}
}

func TestClientSafeProviderErrors(t *testing.T) {
	for _, tc := range []struct {
		message string
		want    error
	}{
		{"The password or code is incorrect", ErrCredentials}, {"The user does not exist", ErrCredentials},
		{"too many times", ErrFrozen}, {"turing test", ErrCaptcha}, {"secret debugging details", ErrUnavailable},
	} {
		t.Run(tc.message, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]string{"status": "error", "msg": tc.message})
			}))
			defer srv.Close()
			_, _, err := New(Config{Issuer: srv.URL}, nil).Login(context.Background(), "u", "pw")
			require.ErrorIs(t, err, tc.want)
		})
	}
	for _, tc := range []struct {
		message string
		want    error
	}{{"email already exists", ErrEmailExists}, {"only send one code", ErrResendWait}, {"secret details", ErrUnavailable}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			require.NoError(t, r.ParseForm())
			require.Equal(t, "signup", r.Form.Get("method"))
			require.Equal(t, "admin/sub2api", r.Form.Get("applicationId"))
			require.Equal(t, "u@example.com", r.Form.Get("dest"))
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "error", "msg": tc.message})
		}))
		err := New(Config{Issuer: srv.URL, Application: "admin/sub2api"}, nil).SendCode(context.Background(), "u@example.com")
		require.ErrorIs(t, err, tc.want)
		srv.Close()
	}
	for _, tc := range []struct {
		message string
		want    error
	}{{"email code incorrect", ErrCode}, {"email already exists", ErrEmailExists}, {"secret details", ErrUnavailable}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "error", "msg": tc.message})
		}))
		_, err := New(Config{Issuer: srv.URL}, nil).Register(context.Background(), "u@example.com", "pw", "123", "U", "u")
		require.ErrorIs(t, err, tc.want)
		srv.Close()
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClientTransportFailureAndRedirect(t *testing.T) {
	client := New(Config{Issuer: "https://idp.example"}, roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("sensitive network error") }))
	_, _, err := client.Login(context.Background(), "u", "pw")
	require.ErrorIs(t, err, ErrUnavailable)
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusTemporaryRedirect, http.StatusOK} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "https://example.invalid/collect")
			w.WriteHeader(status)
			_, _ = fmt.Fprint(w, strings.Repeat("invalid", 200))
		}))
		_, _, err = New(Config{Issuer: srv.URL}, nil).Login(context.Background(), "u", "pw")
		require.ErrorIs(t, err, ErrUnavailable)
		srv.Close()
	}
	_, _, err = New(Config{Issuer: "://invalid"}, nil).Login(context.Background(), "u", "pw")
	require.ErrorIs(t, err, ErrUnavailable)
}
