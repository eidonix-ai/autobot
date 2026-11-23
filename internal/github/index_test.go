package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The reads behind the local copy: what they ask for, and how they read the answers.
func TestIndexReads(t *testing.T) {
	var asked []string
	notModified := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.RequestURI())
		switch r.URL.Path {
		case "/repos/o/r/issues":
			if r.Header.Get("If-None-Match") == `"v1"` {
				notModified++
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"v1"`)
			w.Write([]byte(`[
				{"number":1,"state":"open","title":"Bug","body":null,"author_association":"NONE","comments":2,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-02T00:00:00Z","closed_at":null,"user":{"login":"alice"},"labels":[{"name":"bug"}]},
				{"number":2,"state":"closed","title":"Fix","body":"fixes #1","draft":false,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-03T00:00:00Z","closed_at":"2026-01-03T00:00:00Z","pull_request":{"merged_at":"2026-01-03T00:00:00Z"},"user":{"login":"bob"}}]`))
		case "/repos/o/r/issues/comments":
			w.Write([]byte(`[{"id":7,"body":"hi","issue_url":"https://api.github.com/repos/o/r/issues/12","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-02T00:00:00Z","user":{"login":"carol"}}]`))
		case "/repos/o/r/pulls/comments":
			w.Write([]byte(`[{"id":8,"body":"nit","path":"a.go","line":null,"original_line":40,"pull_request_url":"https://api.github.com/repos/o/r/pulls/13","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-02T00:00:00Z","user":{"login":"dana"}}]`))
		case "/repos/o/r/issues/12":
			w.Write([]byte(`{"number":12,"state":"closed","title":"Old","body":"b","comments":3,"created_at":"2025-01-01T00:00:00Z","updated_at":"2025-02-01T00:00:00Z","closed_at":"2025-02-01T00:00:00Z","user":{"login":"zed"}}`))
		case "/repos/o/r/pulls/13/comments":
			w.Write([]byte(`[{"id":11,"body":"inline","path":"b.go","line":7,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-02T00:00:00Z","user":{"login":"dana"}}]`))
		case "/repos/o/r/pulls/13/reviews":
			w.Write([]byte(`[{"id":9,"body":"ok","state":"APPROVED","submitted_at":"2026-01-04T00:00:00Z","user":{"login":"erin"}},{"id":10,"body":"","state":"PENDING","submitted_at":null,"user":{"login":"me"}}]`))
		case "/repos/o/r/pulls/13":
			w.Write([]byte(`{"state":"closed","merged":true,"merged_at":"2026-01-03T00:00:00Z","draft":false,"additions":5,"deletions":2,"updated_at":"2026-01-03T00:00:00Z","review_comments":4,"head":{"sha":"abc","ref":"feat"},"base":{"ref":"main"}}`))
		case "/repos/o/r":
			w.Write([]byte(`{"open_issues_count":17}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}
	ctx := context.Background()

	since := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	items, err := c.ListItems(ctx, "tok", "o/r", ItemQuery{Sort: "updated", Since: since, Page: 2, Cache: true})
	if err != nil || len(items) != 2 {
		t.Fatalf("items: %+v %v", items, err)
	}
	if q := asked[0]; !strings.Contains(q, "state=all") || !strings.Contains(q, "sort=updated") || !strings.Contains(q, "direction=asc") ||
		!strings.Contains(q, "per_page=100") || !strings.Contains(q, "page=2") || !strings.Contains(q, "since=2026-01-01T12%3A00%3A00Z") {
		t.Fatalf("the list was asked for as %s", q)
	}
	a, b := items[0], items[1]
	if a.Kind != "issue" || a.Body != "" || a.Comments != 2 || a.Labels[0] != "bug" || !a.Closed.IsZero() || !a.MergedAt.IsZero() {
		t.Fatalf("issue: %+v", a)
	}
	if b.Kind != "pr" || b.MergedAt.IsZero() || b.Closed.IsZero() || b.Author != "bob" {
		t.Fatalf("pull request: %+v", b)
	}
	// Asking again for the same page is a conditional request, and its "not modified" answer is read from memory.
	again, err := c.ListItems(ctx, "tok", "o/r", ItemQuery{Sort: "updated", Since: since, Page: 2, Cache: true})
	if err != nil || len(again) != 2 || notModified != 1 {
		t.Fatalf("cached (not-modified answers %d): %+v %v", notModified, again, err)
	}

	if _, err := c.ListItems(ctx, "tok", "o/r", ItemQuery{State: "open", Sort: "created", Page: 1}); err != nil || !strings.Contains(asked[len(asked)-1], "state=open") || strings.Contains(asked[len(asked)-1], "since=") {
		t.Fatalf("the open items were asked for as %v (%v)", asked[len(asked)-1], err)
	}
	if it, err := c.GetItem(ctx, "tok", "o/r", 12); err != nil || it.Title != "Old" || it.Comments != 3 || it.Closed.IsZero() || it.Kind != "issue" {
		t.Fatalf("get item: %+v %v", it, err)
	}
	if _, err := c.GetItem(ctx, "tok", "o/r", 99); err == nil || !strings.Contains(err.Error(), "#99") {
		t.Fatalf("a number GitHub does not have: %v", err)
	}
	if pc, err := c.PullComments(ctx, "tok", "o/r", 13, 1); err != nil || len(pc) != 1 || pc[0].Kind != "review_comment" || pc[0].Number != 13 || pc[0].Line != 7 {
		t.Fatalf("pull comments: %+v %v", pc, err)
	}
	cs, err := c.ListComments(ctx, "tok", "o/r", CommentQuery{Page: 1})
	if err != nil || len(cs) != 1 || cs[0].Number != 12 || cs[0].Kind != "comment" || cs[0].ID != 7 {
		t.Fatalf("comments: %+v %v", cs, err)
	}
	rcs, err := c.ListComments(ctx, "tok", "o/r", CommentQuery{Review: true, Page: 1})
	if err != nil || len(rcs) != 1 || rcs[0].Number != 13 || rcs[0].Kind != "review_comment" || rcs[0].Line != 40 || rcs[0].Path != "a.go" {
		t.Fatalf("review comments: %+v %v", rcs, err)
	}
	rv, err := c.ReviewRecords(ctx, "tok", "o/r", 13)
	if err != nil || len(rv) != 1 || rv[0].State != "APPROVED" || rv[0].Kind != "review" || rv[0].Created.IsZero() {
		t.Fatalf("reviews (a pending one is not kept): %+v %v", rv, err)
	}
	p, err := c.PullDetail(ctx, "tok", "o/r", 13)
	if err != nil || !p.Merged || p.HeadSHA != "abc" || p.HeadRef != "feat" || p.Base != "main" || p.Additions != 5 || p.ReviewComments != 4 {
		t.Fatalf("pull: %+v %v", p, err)
	}
	if n, err := c.OpenCount(ctx, "tok", "o/r"); err != nil || n != 17 {
		t.Fatalf("open count: %d %v", n, err)
	}
}
