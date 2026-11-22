package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeAPI: NVIDIA/hyperflare-inference is public; o/secret is private and visible only to the
// token "github_pat_good", which belongs to flare-autobot.
func fakeAPI(t *testing.T) *Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "" && auth != "Bearer github_pat_good" && auth != "Bearer ghp_classic" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/user" && auth == "Bearer github_pat_good":
			w.Header().Set("GitHub-Authentication-Token-Expiration", "2027-01-05 00:00:00 UTC")
			w.Write([]byte(`{"login":"flare-autobot"}`))
		case r.URL.Path == "/user" && auth == "Bearer ghp_classic":
			w.Header().Set("X-OAuth-Scopes", "public_repo")
			w.Write([]byte(`{"login":"someone-else"}`))
		case r.URL.Path == "/user":
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == "/repos/nvidia/hyperflare-inference":
			w.Write([]byte(`{"full_name":"NVIDIA/hyperflare-inference","private":false,"permissions":{"pull":true,"triage":true}}`))
		case r.URL.Path == "/repos/o/secret" && auth == "Bearer github_pat_good":
			w.Write([]byte(`{"full_name":"o/secret","private":true,"permissions":{"pull":true,"push":true}}`))
		case r.URL.Path == "/repos/busy/repo":
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL}
}

func TestRepoName(t *testing.T) {
	c := fakeAPI(t)
	ctx := context.Background()
	if got, err := c.RepoName(ctx, "", "nvidia/hyperflare-inference"); err != nil || got != "NVIDIA/hyperflare-inference" {
		t.Fatalf("public repo: %q, %v", got, err)
	}
	if _, err := c.RepoName(ctx, "", "o/secret"); !errors.Is(err, ErrNoRepo) {
		t.Fatalf("private repo without a token: %v, want ErrNoRepo", err)
	}
	if got, err := c.RepoName(ctx, "github_pat_good", "o/secret"); err != nil || got != "o/secret" {
		t.Fatalf("private repo with its token: %q, %v", got, err)
	}
	if _, err := c.RepoName(ctx, "", "busy/repo"); err == nil || !strings.Contains(err.Error(), "limiting") {
		t.Fatalf("rate limited: %v", err)
	}
	bad := &Client{BaseURL: "http://127.0.0.1:1"}
	if _, err := bad.RepoName(ctx, "", "a/b"); err == nil || errors.Is(err, ErrNoRepo) {
		t.Fatalf("unreachable GitHub must be an error, not a missing repo: %v", err)
	}
}

func TestCheckIdentity(t *testing.T) {
	c := fakeAPI(t)
	ctx := context.Background()

	id, err := c.CheckIdentity(ctx, "Flare-Autobot", "github_pat_good", "o/secret")
	if err != nil {
		t.Fatal(err)
	}
	want := Identity{Login: "flare-autobot", TokenKind: "fine-grained token", Repo: "o/secret", Private: true, Access: "write",
		Expires: time.Date(2027, 1, 5, 0, 0, 0, 0, time.UTC)}
	if *id != want {
		t.Fatalf("got %+v\nwant %+v", *id, want)
	}

	if _, err := c.CheckIdentity(ctx, "flare-autobot", "github_pat_wrong", "o/secret"); !errors.Is(err, ErrBadToken) {
		t.Errorf("bad token: %v", err)
	}
	if _, err := c.CheckIdentity(ctx, "flare-autobot", "ghp_classic", "o/secret"); err == nil || !strings.Contains(err.Error(), "belongs to the GitHub user someone-else") {
		t.Errorf("someone else's token: %v", err)
	}
	// A token that works but cannot see the repository.
	if _, err := c.CheckIdentity(ctx, "flare-autobot", "github_pat_good", "o/elsewhere"); err == nil || !strings.Contains(err.Error(), "cannot see o/elsewhere") {
		t.Errorf("repo out of reach: %v", err)
	}
}
