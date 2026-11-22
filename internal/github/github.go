// Package github talks to the GitHub API.
package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// ErrNoRepo means GitHub has no repository by that name that the caller can see. Without a token
// that includes every private repository.
var ErrNoRepo = errors.New("no such repository on GitHub")

// ErrBadToken means GitHub did not accept the token: it is wrong, revoked or expired.
var ErrBadToken = errors.New("GitHub did not accept the token (wrong, revoked or expired)")

// Client is a GitHub API client. The zero value uses the public API.
type Client struct {
	BaseURL string       // default https://api.github.com
	HTTP    *http.Client // default: a client with a 10-second timeout
}

var defaultHTTP = &http.Client{Timeout: 10 * time.Second}

// get calls the API, with the token when there is one, and decodes a 200 answer into out.
func (c *Client) get(ctx context.Context, token, path string, out any) (http.Header, error) {
	base, hc := c.BaseURL, c.HTTP
	if base == "" {
		base = "https://api.github.com"
	}
	if hc == nil {
		hc = defaultHTTP
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "autobot")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach GitHub: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return nil, errors.New("GitHub sent an unreadable answer")
		}
		return resp.Header, nil
	case resp.StatusCode == http.StatusNotFound:
		return nil, ErrNoRepo
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, ErrBadToken
	case resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0":
		return nil, errors.New("GitHub is limiting how often we can ask; try again in a few minutes")
	default:
		var msg struct {
			Message string `json:"message"`
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if json.Unmarshal(body, &msg) == nil && msg.Message != "" {
			return nil, fmt.Errorf("GitHub refused: %s", msg.Message)
		}
		return nil, fmt.Errorf("GitHub answered %s", resp.Status)
	}
}

type repo struct {
	FullName    string `json:"full_name"`
	Private     bool   `json:"private"`
	Permissions struct {
		Admin, Maintain, Push, Triage, Pull bool
	} `json:"permissions"`
}

// RepoName checks that owner/repo exists and returns its name as GitHub spells it; a renamed
// repository resolves to its new name. With token "" only public repositories are visible. name
// must already be a valid owner/repo.
func (c *Client) RepoName(ctx context.Context, token, name string) (string, error) {
	var r repo
	if _, err := c.get(ctx, token, "/repos/"+name, &r); err != nil {
		return "", err
	}
	return r.FullName, nil
}

// Identity is what a GitHub user's token can do on one repository.
type Identity struct {
	Login     string    // the GitHub user the token belongs to
	TokenKind string    // "fine-grained token", "classic token", or "token"
	Scopes    string    // a classic token's scopes, as GitHub lists them
	Repo      string    // the repository as GitHub spells it
	Private   bool      // whether the repository is private
	Access    string    // the user's role on the repository: admin, maintain, write, triage or read
	Expires   time.Time // when the token expires; zero if it does not
}

// CheckIdentity confirms that token belongs to the GitHub user login and that the user can see the
// repository name, and reports what the token can do there.
func (c *Client) CheckIdentity(ctx context.Context, login, token, name string) (*Identity, error) {
	var user struct {
		Login string `json:"login"`
	}
	h, err := c.get(ctx, token, "/user", &user)
	if errors.Is(err, ErrNoRepo) {
		err = ErrBadToken
	}
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(user.Login, login) {
		return nil, fmt.Errorf("that token belongs to the GitHub user %s, not %s", user.Login, login)
	}
	id := &Identity{Login: user.Login, TokenKind: tokenKind(token), Scopes: h.Get("X-OAuth-Scopes")}
	for _, layout := range []string{"2006-01-02 15:04:05 MST", "2006-01-02 15:04:05 -0700"} {
		if t, err := time.Parse(layout, h.Get("Github-Authentication-Token-Expiration")); err == nil {
			id.Expires = t
			break
		}
	}

	var r repo
	if _, err := c.get(ctx, token, "/repos/"+name, &r); err != nil {
		if errors.Is(err, ErrNoRepo) {
			return nil, fmt.Errorf("the GitHub user %s cannot see %s: add %s to the repository, or give the token access to it", user.Login, name, user.Login)
		}
		return nil, err
	}
	id.Repo, id.Private = r.FullName, r.Private
	p := r.Permissions
	switch {
	case p.Admin:
		id.Access = "admin"
	case p.Maintain:
		id.Access = "maintain"
	case p.Push:
		id.Access = "write"
	case p.Triage:
		id.Access = "triage"
	default:
		id.Access = "read"
	}
	return id, nil
}

func tokenKind(token string) string {
	switch {
	case strings.HasPrefix(token, "github_pat_"):
		return "fine-grained token"
	case strings.HasPrefix(token, "ghp_"):
		return "classic token"
	default:
		return "token"
	}
}

var refRE = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)

// Private reports whether owner/repo is private, as the token sees it.
func (c *Client) Private(ctx context.Context, token, name string) (bool, error) {
	var r repo
	if _, err := c.get(ctx, token, "/repos/"+name, &r); err != nil {
		return false, err
	}
	return r.Private, nil
}

// CommitSHA resolves a branch, tag or commit of owner/repo to its full commit hash.
func (c *Client) CommitSHA(ctx context.Context, token, name, ref string) (string, error) {
	if !refRE.MatchString(ref) || strings.Contains(ref, "..") {
		return "", fmt.Errorf("%q is not a branch, tag or commit name", ref)
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	if _, err := c.get(ctx, token, "/repos/"+name+"/commits/"+ref, &commit); err != nil {
		if errors.Is(err, ErrNoRepo) {
			return "", fmt.Errorf("GitHub has no branch, tag or commit %q in %s", ref, name)
		}
		if strings.Contains(err.Error(), "422") || strings.Contains(err.Error(), "No commit found") {
			return "", fmt.Errorf("GitHub has no branch, tag or commit %q in %s", ref, name)
		}
		return "", err
	}
	return commit.SHA, nil
}

// Tarball streams owner/repo at a commit as a gzipped tar. With a token it uses the API, which can
// reach a private repository the token can see; without one it uses GitHub's public download
// host, which has no API rate limit. The caller closes the stream.
func (c *Client) Tarball(ctx context.Context, token, name, commit string) (io.ReadCloser, error) {
	url := "https://codeload.github.com/" + name + "/tar.gz/" + commit
	if token != "" {
		base := c.BaseURL
		if base == "" {
			base = "https://api.github.com"
		}
		url = base + "/repos/" + name + "/tarball/" + commit
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "autobot")
	if token != "" {
		// Go drops this header when GitHub redirects to its download host, whose link carries its
		// own short-lived grant.
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Minute}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach GitHub: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound && token == "" {
			return nil, fmt.Errorf("GitHub has no public %s at %s; a private repository needs the workspace's GitHub identity", name, commit[:min(12, len(commit))])
		}
		return nil, fmt.Errorf("GitHub answered %s for %s at %s", resp.Status, name, commit[:min(12, len(commit))])
	}
	return resp.Body, nil
}

// Item is an issue or pull request, as GitHub numbers both in one sequence.
type Item struct {
	Number int
	Kind   string // "pr" or "issue"
	Title  string
	State  string // "open" or "closed"
}

// Item reads issue or pull request number of owner/repo.
func (c *Client) Item(ctx context.Context, token, name string, number int) (*Item, error) {
	var it struct {
		Number      int       `json:"number"`
		Title       string    `json:"title"`
		State       string    `json:"state"`
		PullRequest *struct{} `json:"pull_request"`
	}
	if _, err := c.get(ctx, token, fmt.Sprintf("/repos/%s/issues/%d", name, number), &it); err != nil {
		if errors.Is(err, ErrNoRepo) {
			return nil, fmt.Errorf("%s has no issue or pull request #%d that Autobot can see", name, number)
		}
		return nil, err
	}
	kind := "issue"
	if it.PullRequest != nil {
		kind = "pr"
	}
	return &Item{Number: it.Number, Kind: kind, Title: it.Title, State: it.State}, nil
}

// File reads a file from owner/repo's default branch, up to 64 KB. A missing file is ErrNoRepo.
func (c *Client) File(ctx context.Context, token, name, path string) (string, error) {
	var f struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		Size     int    `json:"size"`
	}
	if _, err := c.get(ctx, token, "/repos/"+name+"/contents/"+path, &f); err != nil {
		return "", err
	}
	if f.Encoding != "base64" || f.Size > 64<<10 {
		return "", fmt.Errorf("%s in %s is not a small text file", path, name)
	}
	b, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(f.Content, "\n", ""))
	return string(b), err
}
