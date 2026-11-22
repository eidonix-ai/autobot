// Package providers talks to LLM inference providers: for now, asking one which models it offers.
package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	timeout   = 15 * time.Second
	maxBody   = 8 << 20 // bytes read from one answer
	maxPages  = 10      // pages followed for a provider that pages its list
	pageSize  = "1000"
	anthropic = "anthropic"
)

// Model is a model a provider offers.
type Model struct {
	ID   string // what a request names it
	Name string // a friendlier name, or "" when the provider gives none
}

// client does not follow redirects, so a key sent to the endpoint cannot be passed on to another site.
var client = &http.Client{
	Timeout:       timeout,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// Discover asks the provider's endpoint which models it offers, sorted by ID. kind is "openai"
// (any OpenAI-compatible API) or "anthropic". key may be empty. Errors are written to be shown to
// a person, and never contain the key.
func Discover(ctx context.Context, kind, endpoint, key string) ([]Model, error) {
	endpoint = strings.TrimRight(endpoint, "/")
	if kind != anthropic {
		body, err := get(ctx, kind, endpoint+"/models", key)
		if err != nil {
			return nil, err
		}
		page, err := parse(body)
		if err != nil {
			return nil, err
		}
		return tidy(page.models), nil
	}
	var all []Model
	next := endpoint + "/v1/models?limit=" + pageSize
	for range maxPages {
		body, err := get(ctx, kind, next, key)
		if err != nil {
			return nil, err
		}
		page, err := parse(body)
		if err != nil {
			return nil, err
		}
		all = append(all, page.models...)
		if !page.hasMore || page.lastID == "" {
			break
		}
		next = endpoint + "/v1/models?limit=" + pageSize + "&after_id=" + url.QueryEscape(page.lastID)
	}
	return tidy(all), nil
}

// get fetches one page of the model list and returns its body.
func get(ctx context.Context, kind, target, key string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, errors.New("The endpoint is not a usable web address.")
	}
	req.Header.Set("User-Agent", "autobot")
	req.Header.Set("Accept", "application/json")
	switch {
	case kind == anthropic:
		req.Header.Set("anthropic-version", "2023-06-01")
		if key != "" {
			req.Header.Set("x-api-key", key)
		}
	case key != "":
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, unreachable(req.URL, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("The endpoint answered %s: check the API key.", resp.Status)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("The endpoint answered %s when asked for its models. It may not list them; model IDs can be entered by hand.", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, unreachable(req.URL, err)
	}
	if len(body) > maxBody {
		return nil, errors.New("The endpoint's list of models is larger than 8 MB, so it was not read.")
	}
	return body, nil
}

// unreachable describes a failed connection without echoing the request, whose URL may carry secrets.
func unreachable(u *url.URL, err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s did not answer within %d seconds.", u.Host, int(timeout.Seconds()))
	}
	return fmt.Errorf("Could not reach %s: %v.", u.Host, err)
}

// page is what one answer holds.
type page struct {
	models  []Model
	hasMore bool   // Anthropic: another page follows
	lastID  string // Anthropic: where the next page starts
}

// item is one model as providers write it: OpenAI and most use id, Gemini's own API uses name.
type item struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	CamelName   string `json:"displayName"` // System One
}

// parse reads the shapes providers use: {"data": [...]}, a bare [...], and {"models": [...]}.
func parse(body []byte) (*page, error) {
	var p page
	var bare []json.RawMessage
	var wrapped struct {
		Data    []json.RawMessage `json:"data"`
		Models  []json.RawMessage `json:"models"`
		HasMore bool              `json:"has_more"`
		LastID  string            `json:"last_id"`
	}
	var raw []json.RawMessage
	switch {
	case json.Unmarshal(body, &bare) == nil:
		raw = bare
	case json.Unmarshal(body, &wrapped) == nil && (wrapped.Data != nil || wrapped.Models != nil):
		raw = append(wrapped.Data, wrapped.Models...)
		p.hasMore, p.lastID = wrapped.HasMore, wrapped.LastID
	default:
		return nil, errors.New("The endpoint answered, but not with a list of models Autobot can read.")
	}
	for _, r := range raw {
		var it item
		if json.Unmarshal(r, &it) != nil {
			if json.Unmarshal(r, &it.ID) != nil { // a plain list of ID strings
				continue
			}
		}
		id, name := it.ID, it.DisplayName
		if name == "" {
			name = it.CamelName
		}
		if id == "" {
			id = it.Name
		} else if name == "" && it.Name != id {
			name = it.Name
		}
		id = strings.TrimPrefix(id, "models/")
		if id == "" {
			continue
		}
		if strings.TrimPrefix(name, "models/") == id {
			name = ""
		}
		p.models = append(p.models, Model{ID: id, Name: name})
	}
	return &p, nil
}

// tidy sorts models by ID and drops repeats, keeping a friendly name when any copy has one.
func tidy(models []Model) []Model {
	byID := map[string]Model{}
	for _, m := range models {
		if cur, ok := byID[m.ID]; !ok || cur.Name == "" {
			byID[m.ID] = m
		}
	}
	out := make([]Model, 0, len(byID))
	for _, m := range byID {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
