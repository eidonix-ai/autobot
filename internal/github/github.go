// Package github talks to the GitHub API.
package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
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

	mu    sync.Mutex
	etags map[string]etagged // answers to getCached, by token and path
}

var defaultHTTP = &http.Client{Timeout: 10 * time.Second}

// get calls the API, with the token when there is one, and decodes a 200 answer into out.
func (c *Client) get(ctx context.Context, token, path string, out any) (http.Header, error) {
	return c.fetch(ctx, token, path, out, false)
}

// fetch is get; with cache it asks GitHub whether the answer changed since last time (If-None-Match),
// and a "not modified" answer, which GitHub does not count against the rate limit, is served from memory.
func (c *Client) fetch(ctx context.Context, token, path string, out any, cache bool) (http.Header, error) {
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
	key := etagKey(token, path)
	if cache {
		if e, ok := c.cached(key); ok {
			req.Header.Set("If-None-Match", e.etag)
		}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach GitHub: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotModified && cache:
		e, ok := c.cached(key)
		if !ok || json.Unmarshal(e.body, out) != nil {
			return nil, errors.New("GitHub sent an unreadable answer")
		}
		return resp.Header, nil
	case resp.StatusCode == http.StatusOK && cache:
		body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		if err != nil || json.NewDecoder(bytes.NewReader(body)).Decode(out) != nil {
			return nil, errors.New("GitHub sent an unreadable answer")
		}
		c.keep(key, etagged{resp.Header.Get("ETag"), body})
		return resp.Header, nil
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

var loginRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9\[\]-]{0,99}$`)

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
	Number      int
	Kind        string // "pr" or "issue"
	Title       string
	State       string // "open" or "closed"
	Body        string
	Author      string
	URL         string // its page on GitHub
	Association string
	Labels      []string
}

// Item reads issue or pull request number of owner/repo.
func (c *Client) Item(ctx context.Context, token, name string, number int) (*Item, error) {
	var it struct {
		Number      int    `json:"number"`
		Title       string `json:"title"`
		State       string `json:"state"`
		Body        string `json:"body"`
		HTMLURL     string `json:"html_url"`
		Association string `json:"author_association"`
		Labels      []struct {
			Name string `json:"name"`
		} `json:"labels"`
		PullRequest *struct{} `json:"pull_request"`
		User        struct {
			Login string `json:"login"`
		} `json:"user"`
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
	return &Item{Number: it.Number, Kind: kind, Title: it.Title, State: it.State, Body: it.Body, Author: it.User.Login, URL: it.HTMLURL,
		Association: it.Association, Labels: names(it.Labels)}, nil
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

// PullRequest is what an agent is told about a pull request.
type PullRequest struct {
	Number int
	URL    string // its page on GitHub
	Title  string
	Body   string
	Author string
	Draft  bool
	State  string // "open" or "closed"
	// Association is GitHub's word for the author's standing: OWNER, MEMBER, COLLABORATOR,
	// CONTRIBUTOR, FIRST_TIME_CONTRIBUTOR, FIRST_TIMER or NONE.
	Association string
	Labels      []string
	HeadSHA     string
	Base        string // the branch it merges into

	Additions, Deletions int // changed lines
}

// PullRequest reads pull request number of owner/repo.
func (c *Client) PullRequest(ctx context.Context, token, name string, number int) (*PullRequest, error) {
	var pr struct {
		Number      int    `json:"number"`
		HTMLURL     string `json:"html_url"`
		Title       string `json:"title"`
		Body        string `json:"body"`
		Draft       bool   `json:"draft"`
		State       string `json:"state"`
		Association string `json:"author_association"`
		Labels      []struct {
			Name string `json:"name"`
		} `json:"labels"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
		Additions int `json:"additions"`
		Deletions int `json:"deletions"`
	}
	if _, err := c.get(ctx, token, fmt.Sprintf("/repos/%s/pulls/%d", name, number), &pr); err != nil {
		if errors.Is(err, ErrNoRepo) {
			return nil, fmt.Errorf("%s has no pull request #%d that Autobot can see", name, number)
		}
		return nil, err
	}
	return &PullRequest{Number: pr.Number, URL: pr.HTMLURL, Title: pr.Title, Body: pr.Body, Author: pr.User.Login,
		Draft: pr.Draft, State: pr.State, Association: pr.Association, Labels: names(pr.Labels), HeadSHA: pr.Head.SHA, Base: pr.Base.Ref,
		Additions: pr.Additions, Deletions: pr.Deletions}, nil
}

func names(labels []struct {
	Name string `json:"name"`
}) []string {
	var out []string
	for _, l := range labels {
		out = append(out, l.Name)
	}
	return out
}

// ChangedFile is one file a pull request changes.
type ChangedFile struct {
	Path      string
	Status    string // added, modified, removed, renamed…
	Additions int
	Deletions int
	Patch     string // its part of the diff; "" when GitHub leaves it out (binary or very large)
}

// PullRequestFiles lists the files a pull request changes, up to 3,000 (GitHub's limit).
func (c *Client) PullRequestFiles(ctx context.Context, token, name string, number int) ([]ChangedFile, error) {
	var out []ChangedFile
	for page := 1; page <= 30; page++ {
		var files []struct {
			Filename  string `json:"filename"`
			Status    string `json:"status"`
			Additions int    `json:"additions"`
			Deletions int    `json:"deletions"`
			Patch     string `json:"patch"`
		}
		if _, err := c.get(ctx, token, fmt.Sprintf("/repos/%s/pulls/%d/files?per_page=100&page=%d", name, number, page), &files); err != nil {
			return nil, err
		}
		for _, f := range files {
			out = append(out, ChangedFile{f.Filename, f.Status, f.Additions, f.Deletions, f.Patch})
		}
		if len(files) < 100 {
			break
		}
	}
	return out, nil
}

// Diff writes a pull request's changes as one unified diff, from its files' patches.
func Diff(files []ChangedFile) string {
	var b strings.Builder
	for _, f := range files {
		fmt.Fprintf(&b, "diff --git a/%s b/%s (%s, +%d -%d)\n", f.Path, f.Path, f.Status, f.Additions, f.Deletions)
		if f.Patch == "" {
			b.WriteString("(no text diff: binary or too large)\n")
			continue
		}
		b.WriteString(f.Patch)
		b.WriteString("\n")
	}
	return b.String()
}

// Comment is one comment on an issue or pull request.
type Comment struct {
	Author string
	Body   string
	At     time.Time
}

// Comments lists the conversation comments on an issue or pull request, oldest first, up to 300.
func (c *Client) Comments(ctx context.Context, token, name string, number int) ([]Comment, error) {
	var out []Comment
	for page := 1; page <= 3; page++ {
		var cs []struct {
			Body string `json:"body"`
			User struct {
				Login string `json:"login"`
			} `json:"user"`
			CreatedAt time.Time `json:"created_at"`
		}
		if _, err := c.get(ctx, token, fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100&page=%d", name, number, page), &cs); err != nil {
			return nil, err
		}
		for _, x := range cs {
			out = append(out, Comment{x.User.Login, x.Body, x.CreatedAt})
		}
		if len(cs) < 100 {
			break
		}
	}
	return out, nil
}

// FileAt reads a file of owner/repo at a commit, up to 1 MB.
func (c *Client) FileAt(ctx context.Context, token, name, ref, path string) (string, error) {
	var f struct {
		Type     string `json:"type"`
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		Size     int    `json:"size"`
	}
	clean, err := repoPath(path)
	if err != nil {
		return "", err
	}
	p := "/repos/" + name + "/contents/" + clean + "?ref=" + url.QueryEscape(ref)
	if _, err := c.get(ctx, token, p, &f); err != nil {
		if errors.Is(err, ErrNoRepo) {
			return "", fmt.Errorf("no file %s at %.12s", path, ref)
		}
		return "", err
	}
	if f.Type != "file" || f.Encoding != "base64" {
		return "", fmt.Errorf("%s is not a file", path)
	}
	if f.Size > 1<<20 {
		return "", fmt.Errorf("%s is larger than 1 MB", path)
	}
	b, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(f.Content, "\n", ""))
	return string(b), err
}

// repoPath checks a path inside a repository and escapes it for the contents API. The path may
// come from a model, so it must stay a path in this repository: no "..", and nothing (?, #, %)
// that would change which request is made.
func repoPath(p string) (string, error) {
	p = strings.Trim(p, "/")
	if p == "" || strings.ContainsAny(p, "?#%\\") || strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", fmt.Errorf("%q is not a path in the repository", p)
	}
	var parts []string
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("%q is not a path in the repository", p)
		}
		parts = append(parts, url.PathEscape(seg))
	}
	return strings.Join(parts, "/"), nil
}

// Tree lists the files of owner/repo at a commit, up to GitHub's limit of about 100,000.
func (c *Client) Tree(ctx context.Context, token, name, ref string) ([]string, error) {
	var t struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
	}
	if _, err := c.get(ctx, token, "/repos/"+name+"/git/trees/"+ref+"?recursive=1", &t); err != nil {
		return nil, err
	}
	var out []string
	for _, e := range t.Tree {
		if e.Type == "blob" {
			out = append(out, e.Path)
		}
	}
	return out, nil
}

// send calls the API with a JSON body (a POST or a PATCH) as token, and decodes a 2xx answer into out.
func (c *Client) send(ctx context.Context, method, token, path string, body, out any) error {
	base, hc := c.BaseURL, c.HTTP
	if base == "" {
		base = "https://api.github.com"
	}
	if hc == nil {
		hc = defaultHTTP
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "autobot")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach GitHub: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode/100 == 2:
		if out != nil {
			return json.Unmarshal(data, out)
		}
		return nil
	case resp.StatusCode == http.StatusNotFound:
		return ErrNoRepo
	case resp.StatusCode == http.StatusUnauthorized:
		return ErrBadToken
	case resp.StatusCode == http.StatusForbidden:
		return errors.New("GitHub refused: the token may not write comments on this repository (it needs Issues and Pull requests: read and write)")
	}
	var msg struct{ Message string }
	json.Unmarshal(data, &msg)
	return fmt.Errorf("GitHub answered %s: %s", resp.Status, msg.Message)
}

// Comment adds a comment to an issue or pull request and returns its id and address.
func (c *Client) Comment(ctx context.Context, token, name string, number int, body string) (int64, string, error) {
	var out struct {
		ID      int64  `json:"id"`
		HTMLURL string `json:"html_url"`
	}
	err := c.send(ctx, http.MethodPost, token, fmt.Sprintf("/repos/%s/issues/%d/comments", name, number), map[string]string{"body": body}, &out)
	return out.ID, out.HTMLURL, err
}

// EditComment replaces one of the identity's comments; ErrNoRepo when it is gone.
func (c *Client) EditComment(ctx context.Context, token, name string, id int64, body string) (string, error) {
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	err := c.send(ctx, http.MethodPatch, token, fmt.Sprintf("/repos/%s/issues/comments/%d", name, id), map[string]string{"body": body}, &out)
	return out.HTMLURL, err
}
