// Package casdoor implements the Casdoor session APIs used by in-page authentication.
package casdoor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
)

var (
	ErrCredentials = errors.New("invalid credentials")
	ErrFrozen      = errors.New("account temporarily locked")
	ErrCaptcha     = errors.New("use the identity provider login to complete captcha")
	ErrMFACode     = errors.New("invalid MFA code")
	ErrCode        = errors.New("invalid registration code")
	ErrEmailExists = errors.New("email already exists")
	ErrResendWait  = errors.New("wait before requesting another code")
	ErrUnavailable = errors.New("identity provider unavailable")
	ErrIdentity    = errors.New("invalid identity provider account")
)

type Config struct {
	Issuer       string
	Organization string
	Application  string
}

type Identity struct {
	Subject       string `json:"id"`
	Owner         string `json:"owner"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"emailVerified"`
	Username      string `json:"name"`
	DisplayName   string `json:"displayName"`
	AvatarURL     string `json:"avatar"`
	IsForbidden   bool   `json:"isForbidden"`
	IsDeleted     bool   `json:"isDeleted"`
}

type MFAMethod struct {
	Type string `json:"mfa_type"`
}

// Session is server-side state. It must never be sent to a browser or logged.
type Session struct {
	Config  Config         `json:"config"`
	Cookies []*http.Cookie `json:"cookies"`
	Methods []MFAMethod    `json:"methods"`
}

type Client struct {
	config    Config
	transport http.RoundTripper
}

func New(config Config, transport http.RoundTripper) *Client {
	config.Issuer = strings.TrimRight(strings.TrimSpace(config.Issuer), "/")
	return &Client{config: config, transport: transport}
}

func (c *Client) httpClient(session *Session) *http.Client {
	jar, _ := cookiejar.New(nil)
	if session != nil {
		issuer, _ := url.Parse(c.config.Issuer)
		jar.SetCookies(issuer, session.Cookies)
	}
	return &http.Client{
		Timeout: 10 * time.Second, Jar: jar, Transport: c.transport,
		// API redirects must not forward credentials to another host.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

type apiResponse struct {
	Status string          `json:"status"`
	Msg    string          `json:"msg"`
	Data   json.RawMessage `json:"data"`
}

func (c *Client) request(ctx context.Context, client *http.Client, method, path, contentType, body string) (*apiResponse, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.config.Issuer+path, strings.NewReader(body))
	if err != nil {
		return nil, ErrUnavailable
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, ErrUnavailable
	}
	var result apiResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return nil, ErrUnavailable
	}
	return &result, nil
}

func (c *Client) post(ctx context.Context, client *http.Client, path string, payload map[string]any) (*apiResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, ErrUnavailable
	}
	return c.request(ctx, client, http.MethodPost, path, "application/json", string(body))
}

func (c *Client) loginPayload() map[string]any {
	application := c.config.Application
	if idx := strings.LastIndexByte(application, '/'); idx >= 0 {
		application = application[idx+1:]
	}
	return map[string]any{"type": "login", "application": application, "organization": c.config.Organization}
}

func (c *Client) Login(ctx context.Context, account, password string) (*Identity, *Session, error) {
	client := c.httpClient(nil)
	payload := c.loginPayload()
	payload["username"], payload["password"], payload["autoSignin"] = strings.TrimSpace(account), password, true
	result, err := c.post(ctx, client, "/api/login", payload)
	if err != nil {
		return nil, nil, err
	}
	if result.Status != "ok" {
		return nil, nil, loginError(result.Msg)
	}
	if result.Msg == "NextMfa" {
		var props []struct {
			Type string `json:"mfaType"`
		}
		if err := json.Unmarshal(result.Data, &props); err != nil || len(props) == 0 {
			return nil, nil, ErrIdentity
		}
		issuer, _ := url.Parse(c.config.Issuer)
		session := &Session{Config: c.config, Cookies: client.Jar.Cookies(issuer)}
		for _, prop := range props {
			if prop.Type != "" {
				session.Methods = append(session.Methods, MFAMethod{Type: prop.Type})
			}
		}
		if len(session.Cookies) == 0 || len(session.Methods) == 0 {
			return nil, nil, ErrIdentity
		}
		return nil, session, nil
	}
	identity, err := c.account(ctx, client)
	return identity, nil, err
}

func (c *Client) CompleteMFA(ctx context.Context, session *Session, method, code string) (*Identity, error) {
	if session == nil || session.Config != c.config {
		return nil, ErrIdentity
	}
	allowed := false
	for _, candidate := range session.Methods {
		if candidate.Type == method {
			allowed = true
		}
	}
	if !allowed {
		return nil, ErrMFACode
	}
	client := c.httpClient(session)
	defer func() {
		issuer, _ := url.Parse(c.config.Issuer)
		session.Cookies = client.Jar.Cookies(issuer)
	}()
	payload := c.loginPayload()
	payload["mfaType"], payload["passcode"] = method, code
	result, err := c.post(ctx, client, "/api/login", payload)
	if err != nil {
		return nil, err
	}
	if result.Status != "ok" || result.Msg == "NextMfa" {
		if strings.Contains(strings.ToLower(result.Msg), "code") || result.Msg == "NextMfa" {
			return nil, ErrMFACode
		}
		return nil, loginError(result.Msg)
	}
	return c.account(ctx, client)
}

func (c *Client) account(ctx context.Context, client *http.Client) (*Identity, error) {
	result, err := c.request(ctx, client, http.MethodGet, "/api/get-account", "", "")
	if err != nil {
		return nil, err
	}
	var identity Identity
	if result.Status != "ok" || json.Unmarshal(result.Data, &identity) != nil {
		return nil, ErrIdentity
	}
	if identity.Subject == "" || identity.Email == "" || identity.Owner != c.config.Organization || identity.IsDeleted || identity.IsForbidden {
		return nil, ErrIdentity
	}
	identity.Email = strings.TrimSpace(strings.ToLower(identity.Email))
	return &identity, nil
}

func (c *Client) SendCode(ctx context.Context, email string) error {
	form := url.Values{"dest": {email}, "type": {"email"}, "applicationId": {c.config.Application}, "method": {"signup"}, "captchaType": {"none"}}
	result, err := c.request(ctx, c.httpClient(nil), http.MethodPost, "/api/send-verification-code", "application/x-www-form-urlencoded", form.Encode())
	if err != nil {
		return err
	}
	if result.Status == "ok" {
		return nil
	}
	message := strings.ToLower(result.Msg)
	if strings.Contains(message, "already exists") {
		return ErrEmailExists
	}
	if strings.Contains(message, "only send one code") {
		return ErrResendWait
	}
	return ErrUnavailable
}

func (c *Client) Register(ctx context.Context, email, password, code, displayName, username string) (*Identity, error) {
	client := c.httpClient(nil)
	payload := c.loginPayload()
	delete(payload, "type")
	payload["email"], payload["password"], payload["emailCode"] = email, password, code
	payload["name"], payload["username"], payload["autoSignin"] = displayName, username, true
	result, err := c.post(ctx, client, "/api/signup", payload)
	if err != nil {
		return nil, err
	}
	if result.Status == "ok" {
		return c.account(ctx, client)
	}
	message := strings.ToLower(result.Msg)
	if strings.Contains(message, "code") {
		return nil, ErrCode
	}
	if strings.Contains(message, "already exists") {
		return nil, ErrEmailExists
	}
	return nil, ErrUnavailable
}

func loginError(message string) error {
	message = strings.ToLower(message)
	switch {
	case strings.Contains(message, "password or code is incorrect"), strings.Contains(message, "user does not exist"):
		return ErrCredentials
	case strings.Contains(message, "too many times"):
		return ErrFrozen
	case strings.Contains(message, "turing test"):
		return ErrCaptcha
	default:
		return ErrUnavailable
	}
}
