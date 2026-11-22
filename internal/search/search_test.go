package search

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Each API's request and answer must match its published shape, and a refused key must not echo.
func TestSearch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/tavily/search" && r.Method == http.MethodPost && r.Header.Get("Authorization") == "Bearer k":
			b, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(b), `"query":"go generics"`) || !strings.Contains(string(b), `"max_results":2`) {
				t.Errorf("tavily body %s", b)
			}
			io.WriteString(w, `{"results":[{"title":"T","url":"https://a.example","content":"about generics"}]}`)
		case r.URL.Path == "/brave/web/search" && r.Header.Get("X-Subscription-Token") == "k" && r.URL.Query().Get("q") == "go generics":
			io.WriteString(w, `{"web":{"results":[{"title":"<strong>Go</strong> &amp; more","url":"https://b.example","description":"x"}]}}`)
		default:
			http.Error(w, "no", http.StatusUnauthorized)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	r, err := Search(ctx, Tavily, srv.URL+"/tavily", "k", "go generics", 2)
	if err != nil || len(r) != 1 || r[0].URL != "https://a.example" || r[0].Snippet != "about generics" {
		t.Fatalf("tavily %+v %v", r, err)
	}
	r, err = Search(ctx, Brave, srv.URL+"/brave/", "k", "go generics", 5)
	if err != nil || len(r) != 1 || r[0].Title != "Go & more" {
		t.Fatalf("brave %+v %v", r, err)
	}
	_, err = Search(ctx, Brave, srv.URL+"/brave", "SECRET", "go generics", 5)
	if err == nil || !strings.Contains(err.Error(), "check the API key") || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("refused key: %v", err)
	}
}
