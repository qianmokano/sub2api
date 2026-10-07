package casdoorcaptcha

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type memoryStore struct {
	mu     sync.Mutex
	values map[string]string
	err    error
	setOK  bool
	ttl    time.Duration
}

func (s *memoryStore) Set(_ context.Context, key, value string, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return false, s.err
	}
	if !s.setOK {
		return false, nil
	}
	s.values[key] = value
	s.ttl = ttl
	return true, nil
}

func (s *memoryStore) Take(_ context.Context, key string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return "", false, s.err
	}
	value, found := s.values[key]
	delete(s.values, key)
	return value, found, nil
}

func fixture(t *testing.T, required bool, kind string) (*Client, *memoryStore) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/get-captcha-status":
			if r.URL.Query().Get("application") != "shop" || r.URL.Query().Get("organization") != "users" || r.URL.Query().Get("userId") != "alice@example.test" {
				t.Error("request did not use the configured application and account")
			}
			_, _ = fmt.Fprintf(w, `{"status":"ok","data":%t}`, required)
		case "/api/get-captcha":
			if r.URL.Query().Get("applicationId") != "admin/shop" || r.URL.Query().Get("isCurrentProvider") != "false" {
				t.Error("request trusted a client-supplied application")
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "data": map[string]string{
				"type": kind, "captchaId": "image-id", "captchaImage": "aW1hZ2U=", "clientId": "public-site-key", "clientSecret": "must-not-return",
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	store := &memoryStore{values: map[string]string{}, setOK: true}
	return New(Config{Issuer: server.URL, Application: "admin/shop", Organization: "users"}, store, nil), store
}

func TestPrepareAndConsume(t *testing.T) {
	for _, kind := range []string{"Default", "Cloudflare Turnstile"} {
		t.Run(kind, func(t *testing.T) {
			client, store := fixture(t, true, kind)
			challenge, err := client.Prepare(context.Background(), ActionLogin, " alice@example.test ")
			if err != nil || !challenge.Required || len(challenge.Token) != 64 || store.ttl != 5*time.Minute {
				t.Fatalf("challenge: %#v, %v", challenge, err)
			}
			raw, _ := json.Marshal(challenge)
			if strings.Contains(string(raw), "must-not-return") || strings.Contains(string(raw), "image-id") {
				t.Fatal("provider secret or image identifier exposed")
			}
			if kind == "Default" && (challenge.Type != "image" || challenge.ImageBase64 != "data:image/png;base64,aW1hZ2U=") {
				t.Fatal("missing image")
			}
			if kind == "Cloudflare Turnstile" && (challenge.Type != "turnstile" || challenge.SiteKey != "public-site-key") {
				t.Fatal("missing public site key")
			}
			proof := &Proof{Challenge: challenge.Token, Answer: "answer"}
			fields, err := client.Resolve(context.Background(), ActionLogin, "alice@example.test", proof)
			if err != nil || fields.Type != kind || fields.Token != "answer" {
				t.Fatalf("fields: %#v, %v", fields, err)
			}
			if _, err = client.Resolve(context.Background(), ActionLogin, "alice@example.test", proof); !errors.Is(err, ErrChallenge) {
				t.Fatal("replay accepted")
			}
			payload := map[string]interface{}{}
			fields.ApplyJSON(payload)
			form := url.Values{}
			fields.ApplyForm(form)
			if payload["captchaToken"] != "answer" || form.Get("captchaType") != kind {
				t.Fatal("answer was not forwarded")
			}
			if kind == "Default" && (payload["clientSecret"] != "image-id" || form.Get("clientSecret") != "image-id") {
				t.Fatal("image identifier not forwarded")
			}
			if kind != "Default" && (payload["clientSecret"] != nil || form.Get("clientSecret") != "") {
				t.Fatal("external secret forwarded")
			}
		})
	}
}

func TestNoCaptchaAndRegistrationActions(t *testing.T) {
	client, _ := fixture(t, false, "none")
	for _, action := range []string{ActionLogin, ActionSendCode, ActionRegister} {
		challenge, err := client.Prepare(context.Background(), action, "alice@example.test")
		if err != nil || challenge.Required || challenge.Token != "" {
			t.Fatalf("unneeded captcha: %#v %v", challenge, err)
		}
		fields, err := client.Resolve(context.Background(), action, "alice@example.test", nil)
		if err != nil || fields.Type != "none" {
			t.Fatal("proof should be optional when Casdoor accepts it")
		}
	}
}

func TestChallengeBindings(t *testing.T) {
	for _, change := range []string{"account", "action", "issuer", "application", "organization", "expired", "malformed", "unsupported"} {
		t.Run(change, func(t *testing.T) {
			client, store := fixture(t, true, "Default")
			challenge, err := client.Prepare(context.Background(), ActionLogin, "alice@example.test")
			if err != nil {
				t.Fatal(err)
			}
			action, account := ActionLogin, "alice@example.test"
			switch change {
			case "account":
				account = "other@example.test"
			case "action":
				action = ActionRegister
			case "issuer":
				client.config.Issuer = "https://other.example.test"
			case "application":
				client.config.Application = "admin/other"
			case "organization":
				client.config.Organization = "other"
			case "expired":
				delete(store.values, challengePrefix+challenge.Token)
			case "malformed":
				store.values[challengePrefix+challenge.Token] = "broken"
			case "unsupported":
				var state state
				_ = json.Unmarshal([]byte(store.values[challengePrefix+challenge.Token]), &state)
				state.Type = "hCaptcha"
				raw, _ := json.Marshal(state)
				store.values[challengePrefix+challenge.Token] = string(raw)
			}
			if _, err = client.Resolve(context.Background(), action, account, &Proof{Challenge: challenge.Token, Answer: "12345"}); !errors.Is(err, ErrChallenge) {
				t.Fatalf("binding bypassed: %v", err)
			}
		})
	}
}

func TestFailures(t *testing.T) {
	t.Run("unsupported", func(t *testing.T) {
		client, _ := fixture(t, true, "hCaptcha")
		if _, err := client.Prepare(context.Background(), ActionLogin, "alice@example.test"); !errors.Is(err, ErrUnsupported) {
			t.Fatal(err)
		}
	})
	for _, failure := range []string{"store", "collision", "missing-store"} {
		t.Run(failure, func(t *testing.T) {
			client, store := fixture(t, true, "Default")
			switch failure {
			case "store":
				store.err = errors.New("offline")
			case "collision":
				store.setOK = false
			case "missing-store":
				client.store = nil
			}
			if _, err := client.Prepare(context.Background(), ActionLogin, "alice@example.test"); !errors.Is(err, ErrUnavailable) {
				t.Fatal(err)
			}
			if failure != "collision" {
				if _, err := client.Resolve(context.Background(), ActionLogin, "alice@example.test", &Proof{Challenge: strings.Repeat("a", 64), Answer: "1"}); !errors.Is(err, ErrUnavailable) {
					t.Fatal(err)
				}
			}
		})
	}
	client, _ := fixture(t, false, "none")
	for _, proof := range []*Proof{{}, {Challenge: strings.Repeat("z", 64), Answer: "1"}, {Challenge: strings.Repeat("a", 64), Answer: strings.Repeat("a", 4097)}} {
		if _, err := client.Resolve(context.Background(), ActionLogin, "alice@example.test", proof); !errors.Is(err, ErrChallenge) {
			t.Fatal(err)
		}
	}
	var missing *Client
	if _, err := missing.Prepare(context.Background(), ActionLogin, "alice@example.test"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := client.Resolve(context.Background(), "unknown", "alice@example.test", nil); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

func TestRemoteFailuresAndRedirects(t *testing.T) {
	for _, body := range []string{`{"status":"error","data":false}`, `{"status":"ok","data":null}`, `{"status":"ok","data":"wrong"}`, `{`} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer server.Close()
			client := New(Config{Issuer: server.URL, Application: "admin/shop", Organization: "users"}, nil, nil)
			if _, err := client.Prepare(context.Background(), ActionLogin, "alice@example.test"); !errors.Is(err, ErrUnavailable) {
				t.Fatal(err)
			}
		})
	}
	targetCalled := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { targetCalled = true }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	client := New(Config{Issuer: source.URL, Application: "admin/shop", Organization: "users"}, nil, source.Client())
	if _, err := client.Prepare(context.Background(), ActionLogin, "alice@example.test"); !errors.Is(err, ErrUnavailable) || targetCalled {
		t.Fatal("redirect followed")
	}
}

func TestInvalidConfigurationAndRequest(t *testing.T) {
	valid := Config{Issuer: "https://passport.example.test", Application: "admin/shop", Organization: "users"}
	for _, change := range []string{"scheme", "credentials", "query", "path", "application", "organization", "account", "action"} {
		t.Run(change, func(t *testing.T) {
			cfg, action, account := valid, ActionLogin, "alice@example.test"
			switch change {
			case "scheme":
				cfg.Issuer = "http://passport.example.test"
			case "credentials":
				cfg.Issuer = "https://user:pass@passport.example.test"
			case "query":
				cfg.Issuer += "?x=1"
			case "path":
				cfg.Issuer += "/other"
			case "application":
				cfg.Application = "shop"
			case "organization":
				cfg.Organization = ""
			case "account":
				account = ""
			case "action":
				action = "other"
			}
			if _, err := New(cfg, nil, nil).Prepare(context.Background(), action, account); !errors.Is(err, ErrInvalid) {
				t.Fatal(err)
			}
		})
	}
}
