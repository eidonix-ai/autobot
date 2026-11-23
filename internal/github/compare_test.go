package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Compare reads the commits and files between two commits with a GET, and refuses anything that is
// not a commit id before asking.
func TestCompare(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		if r.URL.Path != "/repos/o/r/compare/aaaa1111...bbbb2222" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(`{"commits":[{"sha":"cccc3333","commit":{"message":"fix the typo\n\nlonger text","author":{"name":"alice"}}}],
			"files":[{"filename":"a.go","status":"modified","additions":1,"deletions":0,"patch":"+x"}]}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL}
	commits, files, err := c.Compare(context.Background(), "", "o/r", "aaaa1111", "bbbb2222")
	if err != nil || len(commits) != 1 || commits[0] != (Commit{"cccc3333", "alice", "fix the typo"}) ||
		len(files) != 1 || files[0] != (ChangedFile{"a.go", "modified", 1, 0, "+x"}) {
		t.Fatalf("%+v %+v %v", commits, files, err)
	}
	if _, _, err := c.Compare(context.Background(), "", "o/r", "aaaa1111", "gone0000"); !errors.Is(err, ErrNoRepo) {
		t.Fatalf("an unknown commit: %v", err)
	}
	asked = nil
	if _, _, err := c.Compare(context.Background(), "", "o/r", "a..b", "bbbb2222"); err == nil || len(asked) != 0 {
		t.Fatalf("a range as a commit was asked for: %v %v", err, asked)
	}
}
