// Package github talks to the GitHub API.
package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// ErrNoRepo means GitHub has no repository by that name that we can see.
var ErrNoRepo = errors.New("no such repository on GitHub")

// Client is a GitHub API client. The zero value uses the public API without a token, which sees
// public repositories only.
type Client struct {
	BaseURL string       // default https://api.github.com
	HTTP    *http.Client // default: a client with a 10-second timeout
}

var defaultHTTP = &http.Client{Timeout: 10 * time.Second}

// RepoName checks that owner/repo exists and returns its name as GitHub spells it. A renamed
// repository resolves to its new name. name must already be a valid owner/repo.
func (c *Client) RepoName(ctx context.Context, name string) (string, error) {
	base, hc := c.BaseURL, c.HTTP
	if base == "" {
		base = "https://api.github.com"
	}
	if hc == nil {
		hc = defaultHTTP
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/repos/"+name, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "autobot")
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("could not reach GitHub: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var repo struct {
			FullName string `json:"full_name"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&repo); err != nil || repo.FullName == "" {
			return "", fmt.Errorf("GitHub sent an unreadable answer for %s", name)
		}
		return repo.FullName, nil
	case http.StatusNotFound:
		return "", ErrNoRepo
	case http.StatusForbidden, http.StatusTooManyRequests:
		return "", errors.New("GitHub is limiting how often we can check; try again in a few minutes")
	default:
		return "", fmt.Errorf("GitHub answered %s", resp.Status)
	}
}
