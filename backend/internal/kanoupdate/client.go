package kanoupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// Client can talk only to a configured local Unix socket. It cannot download or
// execute updates itself, and has no access to Docker or backup directories.
type Client struct{ HTTP *http.Client }

func NewClient(socket string) *Client {
	return &Client{HTTP: &http.Client{Timeout: 40 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", socket)
		},
	}}}
}

type RemoteError struct {
	Status  int
	Message string
}

func (e *RemoteError) Error() string { return e.Message }

func (c *Client) call(ctx context.Context, method, path string, body, target any) error {
	var input io.Reader
	if body != nil {
		value, err := json.Marshal(body)
		if err != nil {
			return err
		}
		input = bytes.NewReader(value)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://updater"+path, input)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return errors.New("container updater is unavailable")
	}
	defer func() { _ = res.Body.Close() }()
	value, err := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if err != nil || len(value) > 1<<20 {
		return errors.New("invalid updater response")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		var detail struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(value, &detail) != nil || detail.Message == "" {
			detail.Message = "Container updater request failed"
		}
		return &RemoteError{Status: res.StatusCode, Message: detail.Message}
	}
	if err = json.Unmarshal(value, target); err != nil {
		return errors.New("invalid updater response")
	}
	return nil
}

func (c *Client) Health(ctx context.Context) (*Availability, error) {
	var value Availability
	err := c.call(ctx, http.MethodGet, "/health", nil, &value)
	return &value, err
}
func (c *Client) Release(ctx context.Context, force bool) (*Availability, error) {
	var value Availability
	path := "/release"
	if force {
		path += "?force=" + url.QueryEscape("true")
	}
	err := c.call(ctx, http.MethodGet, path, nil, &value)
	return &value, err
}
func (c *Client) Start(ctx context.Context, request StartRequest) (*Session, error) {
	var value Session
	err := c.call(ctx, http.MethodPost, "/jobs", request, &value)
	return &value, err
}
func (c *Client) Current(ctx context.Context) (*Session, error) {
	var value Session
	err := c.call(ctx, http.MethodGet, "/jobs/current", nil, &value)
	return &value, err
}
