// Package casdoorcaptcha handles application-bound CAPTCHA proofs for Casdoor session APIs.
package casdoorcaptcha

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	ActionLogin     = "login"
	ActionSendCode  = "register-send-code"
	ActionRegister  = "register"
	challengePrefix = "sso:captcha:v1:"
	challengeTTL    = 5 * time.Minute
)

var (
	ErrInvalid     = errors.New("invalid captcha request")
	ErrUnavailable = errors.New("passport captcha unavailable")
	ErrUnsupported = errors.New("passport captcha provider is not supported")
	ErrChallenge   = errors.New("passport captcha expired or invalid")
)

type Config struct {
	Issuer       string
	Application  string
	Organization string
}

type Store interface {
	Set(context.Context, string, string, time.Duration) (bool, error)
	Take(context.Context, string) (string, bool, error)
}

type Proof struct {
	Challenge string `json:"challenge"`
	Answer    string `json:"answer"`
}

type Challenge struct {
	Required    bool   `json:"required"`
	Token       string `json:"challenge,omitempty"`
	Type        string `json:"type,omitempty"`
	ImageBase64 string `json:"image_base64,omitempty"`
	SiteKey     string `json:"site_key,omitempty"`
	ExpiresIn   int    `json:"expires_in,omitempty"`
}

// Fields are forwarded only to the configured Casdoor application.
type Fields struct {
	Type    string
	Token   string
	ImageID string
}

type state struct {
	Config  Config `json:"config"`
	Action  string `json:"action"`
	Account string `json:"account"`
	Type    string `json:"type"`
	ImageID string `json:"image_id,omitempty"`
}

type Client struct {
	config Config
	store  Store
	http   *http.Client
}

func New(config Config, store Store, client *http.Client) *Client {
	config.Issuer = strings.TrimRight(strings.TrimSpace(config.Issuer), "/")
	config.Application = strings.TrimSpace(config.Application)
	config.Organization = strings.TrimSpace(config.Organization)
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{config: config, store: store, http: &copyClient}
}

func validRequest(config Config, action, account string) bool {
	if action != ActionLogin && action != ActionSendCode && action != ActionRegister {
		return false
	}
	if account == "" || len(account) > 255 || config.Organization == "" {
		return false
	}
	parts := strings.Split(config.Application, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return false
	}
	u, err := url.Parse(config.Issuer)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return false
	}
	return u.Scheme == "https" || (u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"))
}

func (c *Client) get(ctx context.Context, path string, query url.Values, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.config.Issuer+path+"?"+query.Encode(), nil)
	if err != nil {
		return ErrUnavailable
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return ErrUnavailable
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ErrUnavailable
	}
	var envelope struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&envelope); err != nil || envelope.Status != "ok" || len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return ErrUnavailable
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return ErrUnavailable
	}
	return nil
}

// Prepare asks Casdoor whether this action requires human verification.
// No password, provider secret or Casdoor cookie is stored or returned.
func (c *Client) Prepare(ctx context.Context, action, account string) (*Challenge, error) {
	account = strings.TrimSpace(account)
	if c == nil || !validRequest(c.config, action, account) {
		return nil, ErrInvalid
	}
	parts := strings.Split(c.config.Application, "/")
	var required bool
	if err := c.get(ctx, "/api/get-captcha-status", url.Values{
		"application": {parts[1]}, "organization": {c.config.Organization}, "userId": {account},
	}, &required); err != nil {
		return nil, err
	}
	if !required {
		return &Challenge{Required: false}, nil
	}
	var metadata struct {
		Type    string `json:"type"`
		ImageID string `json:"captchaId"`
		Image   string `json:"captchaImage"`
		SiteKey string `json:"clientId"`
	}
	if err := c.get(ctx, "/api/get-captcha", url.Values{
		"applicationId": {c.config.Application}, "isCurrentProvider": {"false"},
	}, &metadata); err != nil {
		return nil, err
	}
	result := &Challenge{Required: true, ExpiresIn: int(challengeTTL.Seconds())}
	switch metadata.Type {
	case "Default":
		if metadata.ImageID == "" || metadata.Image == "" {
			return nil, ErrUnavailable
		}
		result.Type = "image"
		result.ImageBase64 = "data:image/png;base64," + metadata.Image
	case "Cloudflare Turnstile":
		if metadata.SiteKey == "" {
			return nil, ErrUnavailable
		}
		result.Type, result.SiteKey = "turnstile", metadata.SiteKey
	default:
		return nil, ErrUnsupported
	}
	if c.store == nil {
		return nil, ErrUnavailable
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, ErrUnavailable
	}
	result.Token = hex.EncodeToString(buf)
	raw, err := json.Marshal(state{Config: c.config, Action: action, Account: account, Type: metadata.Type, ImageID: metadata.ImageID})
	if err != nil {
		return nil, ErrUnavailable
	}
	ok, err := c.store.Set(ctx, challengePrefix+result.Token, string(raw), challengeTTL)
	if err != nil || !ok {
		return nil, ErrUnavailable
	}
	return result, nil
}

// Resolve atomically consumes a challenge before forwarding its answer to Casdoor.
// With no proof, Casdoor remains authoritative and rejects any required CAPTCHA.
func (c *Client) Resolve(ctx context.Context, action, account string, proof *Proof) (Fields, error) {
	account = strings.TrimSpace(account)
	if c == nil || !validRequest(c.config, action, account) {
		return Fields{}, ErrInvalid
	}
	if proof == nil {
		return Fields{Type: "none"}, nil
	}
	if len(proof.Challenge) != 64 || proof.Answer == "" || len(proof.Answer) > 4096 {
		return Fields{}, ErrChallenge
	}
	if _, err := hex.DecodeString(proof.Challenge); err != nil {
		return Fields{}, ErrChallenge
	}
	if c.store == nil {
		return Fields{}, ErrUnavailable
	}
	raw, found, err := c.store.Take(ctx, challengePrefix+proof.Challenge)
	if err != nil {
		return Fields{}, ErrUnavailable
	}
	if !found {
		return Fields{}, ErrChallenge
	}
	var record state
	if err := json.Unmarshal([]byte(raw), &record); err != nil || record.Config != c.config || record.Action != action || record.Account != account {
		return Fields{}, ErrChallenge
	}
	if record.Type != "Default" && record.Type != "Cloudflare Turnstile" {
		return Fields{}, ErrChallenge
	}
	return Fields{Type: record.Type, Token: proof.Answer, ImageID: record.ImageID}, nil
}

func (f Fields) ApplyJSON(payload map[string]interface{}) {
	payload["captchaType"] = f.Type
	payload["captchaToken"] = f.Token
	if f.Type == "Default" {
		payload["clientSecret"] = f.ImageID
	}
}

func (f Fields) ApplyForm(payload url.Values) {
	payload.Set("captchaType", f.Type)
	payload.Set("captchaToken", f.Token)
	if f.Type == "Default" {
		payload.Set("clientSecret", f.ImageID)
	}
}
