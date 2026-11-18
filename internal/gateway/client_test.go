package gateway_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/arturmkr/trino-maintenance-operator/internal/fakes"
	"github.com/arturmkr/trino-maintenance-operator/internal/gateway"
)

func newTestGateway(t *testing.T, fg *fakes.Gateway, creds gateway.Credentials) *gateway.HTTPClient {
	t.Helper()
	srv := httptest.NewServer(fg)
	t.Cleanup(srv.Close)
	c, err := gateway.NewHTTPClient(srv.URL+"/", creds)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewHTTPClient_RejectsInvalidURL(t *testing.T) {
	for _, u := range []string{"", "not-a-url", "localhost:8080"} {
		if _, err := gateway.NewHTTPClient(u, gateway.Credentials{}); err == nil {
			t.Fatalf("expected error for %q", u)
		}
	}
}

func TestListActivateDeactivate(t *testing.T) {
	fg := fakes.NewGateway(
		gateway.Backend{Name: "general-1", ProxyTo: "http://g1:8080", Active: true, RoutingGroup: "general"},
		gateway.Backend{Name: "general-2", ProxyTo: "http://g2:8080", Active: true, RoutingGroup: "general"},
	)
	c := newTestGateway(t, fg, gateway.Credentials{})
	ctx := context.Background()

	backends, err := c.ListBackends(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(backends) != 2 {
		t.Fatalf("got %d backends", len(backends))
	}
	b, ok := gateway.FindBackend(backends, "general-2")
	if !ok || !b.Active || b.RoutingGroup != "general" || b.ProxyTo != "http://g2:8080" {
		t.Fatalf("unexpected backend %+v", b)
	}

	if err := c.DeactivateBackend(ctx, "general-1"); err != nil {
		t.Fatal(err)
	}
	if fg.IsActive("general-1") {
		t.Fatal("general-1 should be inactive")
	}
	if err := c.ActivateBackend(ctx, "general-1"); err != nil {
		t.Fatal(err)
	}
	if !fg.IsActive("general-1") {
		t.Fatal("general-1 should be active")
	}
}

func TestErrors(t *testing.T) {
	fg := fakes.NewGateway()
	c := newTestGateway(t, fg, gateway.Credentials{})

	err := c.DeactivateBackend(context.Background(), "missing")
	var ge *gateway.Error
	if !errors.As(err, &ge) || ge.StatusCode != http.StatusNotFound || !gateway.IsNotFound(err) {
		t.Fatalf("expected 404 gateway error, got %v", err)
	}

	fg.SetFail(true)
	_, err = c.ListBackends(context.Background())
	if !errors.As(err, &ge) || ge.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 gateway error, got %v", err)
	}
}

func TestBasicAuth(t *testing.T) {
	fg := fakes.NewGateway(gateway.Backend{Name: "general-1", Active: true})
	fg.Username, fg.Password = "api", "secret"

	unauth := newTestGateway(t, fg, gateway.Credentials{})
	if _, err := unauth.ListBackends(context.Background()); err == nil {
		t.Fatal("expected 401 without credentials")
	}

	auth := newTestGateway(t, fg, gateway.Credentials{Username: "api", Password: "secret"})
	if _, err := auth.ListBackends(context.Background()); err != nil {
		t.Fatalf("expected success with credentials: %v", err)
	}
}

func TestBearerToken(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"code":200,"data":[]}`))
	}))
	t.Cleanup(srv.Close)
	c, err := gateway.NewHTTPClient(srv.URL, gateway.Credentials{Token: "jwt-token", Username: "ignored"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListBackends(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got != "Bearer jwt-token" {
		t.Fatalf("Authorization = %q", got)
	}
}
