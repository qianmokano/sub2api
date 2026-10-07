package casdoor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/casdoorcaptcha"
	"github.com/stretchr/testify/require"
)

type captchaMemory struct {
	sync.Mutex
	values map[string]string
}

func (s *captchaMemory) Set(_ context.Context, key, value string, _ time.Duration) (bool, error) {
	s.Lock()
	defer s.Unlock()
	if _, exists := s.values[key]; exists {
		return false, nil
	}
	s.values[key] = value
	return true, nil
}
func (s *captchaMemory) Take(_ context.Context, key string) (string, bool, error) {
	s.Lock()
	defer s.Unlock()
	value, exists := s.values[key]
	delete(s.values, key)
	return value, exists, nil
}

func TestClientCaptchaProofsForEveryCredentialAction(t *testing.T) {
	for _, kind := range []string{"Default", "Cloudflare Turnstile"} {
		t.Run(kind, func(t *testing.T) {
			requests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/get-captcha-status":
					_, _ = w.Write([]byte(`{"status":"ok","data":true}`))
				case "/api/get-captcha":
					_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "data": map[string]any{"type": kind, "captchaId": "image-id", "captchaImage": "aW1hZ2U=", "clientId": "site-key", "clientSecret": "secret"}})
				case "/api/login", "/api/signup", "/api/send-verification-code":
					requests++
					var fields map[string]any
					if r.URL.Path == "/api/send-verification-code" {
						require.NoError(t, r.ParseForm())
						fields = make(map[string]any)
						for key := range r.Form {
							fields[key] = r.Form.Get(key)
						}
					} else {
						require.NoError(t, json.NewDecoder(r.Body).Decode(&fields))
					}
					require.Equal(t, "answer", fields["captchaToken"])
					if kind == "Default" {
						require.Equal(t, "Default", fields["captchaType"])
						require.Equal(t, "image-id", fields["clientSecret"])
					} else {
						require.Equal(t, "Cloudflare Turnstile", fields["captchaType"])
						require.NotContains(t, fields, "clientSecret")
					}
					_, _ = w.Write([]byte(`{"status":"error","msg":"turing test failed"}`))
				default:
					t.Errorf("unexpected credential request %s", r.URL.Path)
				}
			}))
			defer srv.Close()
			client := New(Config{Issuer: srv.URL, Organization: "kano", Application: "admin/sub2api"}, nil, &captchaMemory{values: make(map[string]string)})
			ctx := context.Background()
			for _, action := range []string{casdoorcaptcha.ActionLogin, casdoorcaptcha.ActionSendCode, casdoorcaptcha.ActionRegister} {
				challenge, err := client.PrepareCaptcha(ctx, action, "u@example.com")
				require.NoError(t, err)
				proof := &casdoorcaptcha.Proof{Challenge: challenge.Token, Answer: "answer"}
				var invoke func() error
				switch action {
				case casdoorcaptcha.ActionLogin:
					invoke = func() error { _, _, err := client.Login(ctx, "u@example.com", " password ", proof); return err }
				case casdoorcaptcha.ActionSendCode:
					invoke = func() error { return client.SendCode(ctx, "u@example.com", proof) }
				case casdoorcaptcha.ActionRegister:
					invoke = func() error {
						_, err := client.Register(ctx, "u@example.com", "password", "123456", "User", "random", proof)
						return err
					}
				}
				require.ErrorIs(t, invoke(), ErrCaptcha)
				require.ErrorIs(t, invoke(), casdoorcaptcha.ErrChallenge)
			}
			require.Equal(t, 3, requests)
		})
	}
}
