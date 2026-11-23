// Package search asks a web search API (Tavily or Brave Search) for results, for agents' web_search
// tool.
package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/eidonix-ai/autobot/internal/netguard"
)

// The search APIs Autobot speaks; a provider of one of these kinds has a key and no models.
const (
	Tavily = "tavily"
	Brave  = "brave"
)

// Result is one hit.
type Result struct {
	Title, URL, Snippet string
}

const maxBody = 4 << 20

// client does not follow redirects, so a key sent to the API cannot be passed on to another site.
var client = &http.Client{
	// Only public addresses, or hosts an admin trusts: the address comes from a person.
	Transport:     netguard.Transport(),
	Timeout:       30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// Search asks the API at endpoint (https://api.tavily.com, or https://api.search.brave.com/res/v1)
// for up to n results. Errors are written to be shown to a person and never contain the key.
func Search(ctx context.Context, kind, endpoint, key, query string, n int) ([]Result, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("the search has no query")
	}
	n = min(max(n, 1), 20)
	endpoint = strings.TrimRight(endpoint, "/")
	var req *http.Request
	var err error
	switch kind {
	case Tavily:
		body, _ := json.Marshal(map[string]any{"query": query, "max_results": n, "search_depth": "basic"})
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/search", strings.NewReader(string(body)))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+key)
		}
	case Brave:
		q := url.Values{"q": {query}, "count": {strconv.Itoa(n)}}
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/web/search?"+q.Encode(), nil)
		if err == nil {
			req.Header.Set("X-Subscription-Token", key)
		}
	default:
		return nil, fmt.Errorf("%q is not a search API", kind)
	}
	if err != nil {
		return nil, errors.New("the endpoint is not a usable web address")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "autobot")
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("could not reach %s: %v", req.URL.Host, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("could not read the answer from %s: %v", req.URL.Host, err)
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("%s answered %s: check the API key", req.URL.Host, resp.Status)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%s answered %s", req.URL.Host, resp.Status)
	}
	var out []Result
	if kind == Tavily {
		var r struct {
			Results []struct{ Title, URL, Content string }
		}
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("%s answered with something other than search results", req.URL.Host)
		}
		for _, h := range r.Results {
			out = append(out, Result{h.Title, h.URL, h.Content})
		}
	} else {
		var r struct {
			Web struct {
				Results []struct{ Title, URL, Description string }
			}
		}
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("%s answered with something other than search results", req.URL.Host)
		}
		for _, h := range r.Web.Results {
			out = append(out, Result{plain(h.Title), h.URL, plain(h.Description)})
		}
	}
	if len(out) > n {
		out = out[:n]
	}
	return out, nil
}

var tag = regexp.MustCompile(`<[^>]*>`)

// plain drops the highlighting markup Brave puts in titles and descriptions.
func plain(s string) string { return html.UnescapeString(tag.ReplaceAllString(s, "")) }
