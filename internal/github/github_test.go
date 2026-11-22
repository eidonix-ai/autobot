package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRepoName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/nvidia/hyperflare-inference":
			w.Write([]byte(`{"full_name":"NVIDIA/hyperflare-inference"}`))
		case "/repos/busy/repo":
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}
	ctx := context.Background()

	if got, err := c.RepoName(ctx, "nvidia/hyperflare-inference"); err != nil || got != "NVIDIA/hyperflare-inference" {
		t.Fatalf("existing repo: %q, %v", got, err)
	}
	if _, err := c.RepoName(ctx, "nobody/nothing"); !errors.Is(err, ErrNoRepo) {
		t.Fatalf("missing repo: %v", err)
	}
	if _, err := c.RepoName(ctx, "busy/repo"); err == nil || !strings.Contains(err.Error(), "limiting") {
		t.Fatalf("rate limited: %v", err)
	}
	srv.Close()
	if _, err := c.RepoName(ctx, "nvidia/hyperflare-inference"); err == nil || errors.Is(err, ErrNoRepo) {
		t.Fatalf("unreachable GitHub must be an error, not a missing repo: %v", err)
	}
}
