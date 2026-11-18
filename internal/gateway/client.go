// Package gateway is a minimal client for the Trino Gateway backend API
// (https://trinodb.github.io/trino-gateway/gateway-api/).
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Backend is a Trino cluster registered in Trino Gateway.
type Backend struct {
	Name         string   `json:"name"`
	ProxyTo      string   `json:"proxyTo"`
	Active       bool     `json:"active"`
	RoutingGroup string   `json:"routingGroup"`
	Status       string   `json:"status"`
	ExternalURL  string   `json:"externalUrl,omitempty"`
	Tags         []string `json:"tags,omitempty"`
}

// Client is the subset of the Trino Gateway API the operator needs.
type Client interface {
	// ListBackends returns every backend known to the gateway, active or not.
	ListBackends(ctx context.Context) ([]Backend, error)
	// ActivateBackend makes the gateway route new queries to the backend again.
	ActivateBackend(ctx context.Context, name string) error
	// DeactivateBackend stops the gateway from routing new queries to the backend.
	// Queries already running on the backend are not affected.
	DeactivateBackend(ctx context.Context, name string) error
}

// Credentials authenticate API calls. Username/Password use HTTP basic auth
// (Trino Gateway form/basic authentication with a preset user holding the API
// privilege); Token is sent as a bearer token (JWT authentication).
// Empty credentials disable authentication.
type Credentials struct {
	Username string
	Password string
	Token    string
}

// Error is returned for non-2xx gateway responses.
type Error struct {
	StatusCode int
	Body       string
}

func (e *Error) Error() string {
	return fmt.Sprintf("trino gateway returned HTTP %d: %s", e.StatusCode, strings.TrimSpace(e.Body))
}

// HTTPClient talks to a Trino Gateway over HTTP.
type HTTPClient struct {
	baseURL string
	creds   Credentials
	http    *http.Client
}

// Option customises an HTTPClient.
type Option func(*HTTPClient)

// WithHTTPClient replaces the underlying *http.Client.
func WithHTTPClient(c *http.Client) Option { return func(h *HTTPClient) { h.http = c } }

// NewHTTPClient creates a client for the gateway at baseURL (scheme + host[:port]).
func NewHTTPClient(baseURL string, creds Credentials, opts ...Option) (*HTTPClient, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, fmt.Errorf("invalid trino gateway url %q", baseURL)
	}
	c := &HTTPClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		creds:   creds,
		http:    &http.Client{Timeout: 15 * time.Second},
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

func (c *HTTPClient) ListBackends(ctx context.Context) ([]Backend, error) {
	// /gateway/backend/all only returns administrative configuration. This
	// read-only WebApp call also reports the Gateway monitor's routing health.
	body, err := c.do(ctx, http.MethodPost, "/webapp/getAllBackends", strings.NewReader("{}"))
	if err != nil {
		return nil, err
	}
	var result struct {
		Code int
		Data []struct {
			Backend
			Active *bool `json:"active"`
		}
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decoding trino gateway backend list: %w", err)
	}
	if result.Code != http.StatusOK || result.Data == nil {
		return nil, fmt.Errorf("invalid Gateway backend snapshot (code %d)", result.Code)
	}
	seen := map[string]bool{}
	backends := make([]Backend, 0, len(result.Data))
	for _, row := range result.Data {
		if row.Name == "" || seen[row.Name] || row.Active == nil {
			return nil, fmt.Errorf("incomplete or duplicate Gateway backend %q", row.Name)
		}
		row.Backend.Active = *row.Active
		backends = append(backends, row.Backend)
		seen[row.Name] = true
	}
	return backends, nil
}

func (c *HTTPClient) ActivateBackend(ctx context.Context, name string) error {
	_, err := c.do(ctx, http.MethodPost, "/gateway/backend/activate/"+url.PathEscape(name), nil)
	return err
}

func (c *HTTPClient) DeactivateBackend(ctx context.Context, name string) error {
	_, err := c.do(ctx, http.MethodPost, "/gateway/backend/deactivate/"+url.PathEscape(name), nil)
	return err
}

func (c *HTTPClient) do(ctx context.Context, method, path string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	switch {
	case c.creds.Token != "":
		req.Header.Set("Authorization", "Bearer "+c.creds.Token)
	case c.creds.Username != "":
		req.SetBasicAuth(c.creds.Username, c.creds.Password)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling trino gateway %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("reading trino gateway response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &Error{StatusCode: resp.StatusCode, Body: string(data)}
	}
	return data, nil
}

// FindBackend returns the backend with the given name, or false.
func FindBackend(backends []Backend, name string) (Backend, bool) {
	for _, b := range backends {
		if b.Name == name {
			return b, true
		}
	}
	return Backend{}, false
}

// IsNotFound reports whether err is a gateway 404.
func IsNotFound(err error) bool {
	var ge *Error
	return errors.As(err, &ge) && ge.StatusCode == http.StatusNotFound
}
