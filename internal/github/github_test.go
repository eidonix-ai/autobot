package github

import (
	"context"
	"errors"
	"io"
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

// A private repository's code downloads with the token through the API, which redirects to a
// download link; without the token it is not found.
func TestTarball(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/secret/tarball/abc":
			if r.Header.Get("Authorization") != "Bearer github_pat_good" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			http.Redirect(w, r, "/download/o-secret-abc.tar.gz?grant=short-lived", http.StatusFound)
		case "/download/o-secret-abc.tar.gz":
			w.Write([]byte("tar bytes"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}
	body, err := c.Tarball(context.Background(), "github_pat_good", "o/secret", "abc")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(body)
	body.Close()
	if string(b) != "tar bytes" {
		t.Fatalf("got %q", b)
	}
	if _, err := c.Tarball(context.Background(), "github_pat_wrong", "o/secret", "abc"); err == nil {
		t.Fatal("a wrong token downloaded a private repository")
	}
}

// A path a model gives stays a path in the repository.
func TestRepoPath(t *testing.T) {
	for _, bad := range []string{"", "../other/repo", "a/../../b", "a/./b", "x?ref=main", "x#y", "%2e%2e/x", "a\\b", "a//b", "a\nb"} {
		if p, err := repoPath(bad); err == nil {
			t.Errorf("repoPath(%q) = %q", bad, p)
		}
	}
	if p, err := repoPath("/internal/web/a b.go"); err != nil || p != "internal/web/a%20b.go" {
		t.Errorf("%q %v", p, err)
	}
}

// The lists are asked for again with the ETag GitHub gave, and an unchanged answer (304) is served
// from memory; a changed one replaces it. Another token never sees the first one's answer.
func TestListsUseETags(t *testing.T) {
	var asked, notModified int
	body := `[{"number":7,"state":"open","title":"T","draft":true,"updated_at":"2026-11-23T15:00:00Z","user":{"login":"alice"},
		"head":{"sha":"abc"},"base":{"ref":"main"},"labels":[{"name":"bug"}],"requested_reviewers":[{"login":"bot"}]}]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		if r.Header.Get("Authorization") == "Bearer other" && r.Header.Get("If-None-Match") != "" {
			t.Error("sent one token's ETag with another")
		}
		if r.Header.Get("If-None-Match") == `"v1"` {
			notModified++
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Write([]byte(body))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}
	for i := 0; i < 3; i++ {
		ps, err := c.ListPulls(context.Background(), "tok", "o/r", "all", 1)
		if err != nil || len(ps) != 1 || ps[0].Number != 7 || !ps[0].Draft || ps[0].HeadSHA != "abc" || ps[0].Base != "main" ||
			ps[0].Labels[0] != "bug" || ps[0].Reviewers[0] != "bot" || !ps[0].Open || ps[0].Updated.Year() != 2026 {
			t.Fatalf("round %d: %+v %v", i, ps, err)
		}
	}
	if asked != 3 || notModified != 2 {
		t.Fatalf("asked %d times, %d answered not modified", asked, notModified)
	}
	if _, err := c.ListPulls(context.Background(), "other", "o/r", "all", 1); err != nil {
		t.Fatal(err)
	}
}

func TestListIssuesMarksPullRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "since=2026-11-23T15%3A00%3A00Z") {
			t.Errorf("no since: %s", r.URL.RawQuery)
		}
		w.Write([]byte(`[{"number":1,"state":"open","title":"issue","user":{"login":"a"}},{"number":2,"state":"closed","pull_request":{},"user":{"login":"b"}}]`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}
	is, err := c.ListIssues(context.Background(), "", "o/r", "all", time.Date(2026, 11, 23, 15, 0, 0, 0, time.UTC), 1)
	if err != nil || len(is) != 2 || is[0].Kind != "issue" || is[1].Kind != "pr" || is[1].Open {
		t.Fatalf("%+v %v", is, err)
	}
}

func TestCIStateAndComments(t *testing.T) {
	runs := map[string]string{
		"pending": `[{"status":"completed","conclusion":"success"},{"status":"in_progress"}]`,
		"failed":  `[{"status":"completed","conclusion":"success"},{"status":"completed","conclusion":"failure"}]`,
		"passed":  `[{"status":"completed","conclusion":"success"},{"status":"completed","conclusion":"skipped"}]`,
		"none":    `[]`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			sha := strings.Split(r.URL.Path, "/")[5]
			w.Write([]byte(`{"check_runs":` + runs[sha] + `}`))
		case strings.HasSuffix(r.URL.Path, "/issues/comments"):
			w.Write([]byte(`[{"id":9,"body":"hi","issue_url":"https://api.github.com/repos/o/r/issues/42","updated_at":"2026-11-23T15:00:00Z","user":{"login":"carol"}}]`))
		case strings.HasSuffix(r.URL.Path, "/permission"):
			w.Write([]byte(`{"permission":"write","role_name":"maintain"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c, ctx := &Client{BaseURL: srv.URL}, context.Background()
	for want := range runs {
		if got, err := c.CIState(ctx, "", "o/r", want); err != nil || got != want {
			t.Errorf("checks of %s: %q %v", want, got, err)
		}
	}
	cs, err := c.RepoComments(ctx, "", "o/r", time.Time{}, 1)
	if err != nil || len(cs) != 1 || cs[0].Number != 42 || cs[0].ID != 9 || cs[0].Author != "carol" {
		t.Fatalf("%+v %v", cs, err)
	}
	if role, err := c.Role(ctx, "", "o/r", "carol"); err != nil || role != "maintain" {
		t.Fatalf("role %q %v", role, err)
	}
	if role, err := c.Role(ctx, "", "o/r", "../etc"); err != nil || role != "none" {
		t.Fatalf("role of a bad name %q %v", role, err)
	}
}

func TestTransientErrorClassification(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/issues/1/comments":
			w.WriteHeader(http.StatusInternalServerError)
		case "/repos/o/r/issues/2/comments":
			w.WriteHeader(http.StatusBadGateway)
		case "/repos/o/r/issues/3/comments":
			w.WriteHeader(http.StatusTooManyRequests)
		case "/repos/o/r/issues/4/comments":
			w.WriteHeader(http.StatusForbidden)
		case "/repos/o/r/issues/5/comments":
			w.WriteHeader(http.StatusUnauthorized)
		case "/repos/o/r/issues/6/comments":
			w.WriteHeader(http.StatusUnprocessableEntity)
		case "/repos/o/r/pulls/500":
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c, ctx := &Client{BaseURL: srv.URL}, context.Background()

	for _, num := range []int{1, 2, 3} {
		if _, _, err := c.Comment(ctx, "tok", "o/r", num, "hi"); !IsTransient(err) {
			t.Errorf("comment #%d: want transient error, got %v", num, err)
		}
	}
	for _, num := range []int{4, 5, 6, 99} {
		if _, _, err := c.Comment(ctx, "tok", "o/r", num, "hi"); err == nil || IsTransient(err) {
			t.Errorf("comment #%d: want permanent error, got %v (transient=%v)", num, err, IsTransient(err))
		}
	}
	if _, err := c.PullRequest(ctx, "tok", "o/r", 500); !IsTransient(err) {
		t.Errorf("GET 503: want transient error, got %v", err)
	}
	bad := &Client{BaseURL: "http://127.0.0.1:1"}
	if _, _, err := bad.Comment(ctx, "tok", "o/r", 1, "hi"); !IsTransient(err) {
		t.Errorf("network error: want transient error, got %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := c.Comment(canceled, "tok", "o/r", 1, "hi"); IsTransient(err) {
		t.Errorf("canceled context must not be transient, got %v", err)
	}
}
