package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// The calls the poller makes to see what changed. All are reads. The lists and the check runs ask
// GitHub "has this changed?" (an ETag), because an unchanged answer costs nothing against the
// rate limit.

// PageSize is how many pull requests or issues one list page holds.
const PageSize = 50

type etagged struct {
	etag string
	body []byte
}

const maxEtags = 500

func etagKey(token, path string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:6]) + path
}

func (c *Client) cached(key string) (etagged, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.etags[key]
	return e, ok && e.etag != ""
}

func (c *Client) keep(key string, e etagged) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.etags == nil || len(c.etags) >= maxEtags {
		c.etags = map[string]etagged{}
	}
	c.etags[key] = e
}

// Listed is one entry of a list of pull requests or issues.
type Listed struct {
	Number    int
	Kind      string // "pr" or "issue"
	Open      bool
	Title     string
	Body      string
	Author    string
	Draft     bool
	HeadSHA   string
	Base      string
	Labels    []string
	Reviewers []string // users whose review is requested (pull requests)
	Updated   time.Time
}

type listedJSON struct {
	Number      int       `json:"number"`
	State       string    `json:"state"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	Draft       bool      `json:"draft"`
	UpdatedAt   time.Time `json:"updated_at"`
	PullRequest *struct{} `json:"pull_request"`
	User        struct {
		Login string `json:"login"`
	} `json:"user"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Reviewers []struct {
		Login string `json:"login"`
	} `json:"requested_reviewers"`
	Head struct {
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (j listedJSON) listed(kind string) Listed {
	l := Listed{Number: j.Number, Kind: kind, Open: j.State == "open", Title: j.Title, Body: j.Body, Author: j.User.Login,
		Draft: j.Draft, HeadSHA: j.Head.SHA, Base: j.Base.Ref, Updated: j.UpdatedAt}
	for _, x := range j.Labels {
		l.Labels = append(l.Labels, x.Name)
	}
	for _, x := range j.Reviewers {
		l.Reviewers = append(l.Reviewers, x.Login)
	}
	return l
}

// ListPulls lists pull requests, the most recently updated first, one page at a time from 1.
// state is "open" or "all".
func (c *Client) ListPulls(ctx context.Context, token, name, state string, page int) ([]Listed, error) {
	var raw []listedJSON
	path := fmt.Sprintf("/repos/%s/pulls?state=%s&sort=updated&direction=desc&per_page=%d&page=%d", name, state, PageSize, page)
	if _, err := c.fetch(ctx, token, path, &raw, true); err != nil {
		return nil, err
	}
	out := make([]Listed, len(raw))
	for i, j := range raw {
		out[i] = j.listed("pr")
	}
	return out, nil
}

// ListIssues lists issues updated since a time, the most recently updated first, one page at a
// time from 1; state is "open" or "all". GitHub mixes pull requests in (their Kind is "pr"), and a
// full page means there may be more.
func (c *Client) ListIssues(ctx context.Context, token, name, state string, since time.Time, page int) ([]Listed, error) {
	var raw []listedJSON
	path := fmt.Sprintf("/repos/%s/issues?state=%s&sort=updated&direction=desc&per_page=%d&page=%d", name, state, PageSize, page)
	if !since.IsZero() {
		path += "&since=" + url.QueryEscape(since.UTC().Format(time.RFC3339))
	}
	if _, err := c.fetch(ctx, token, path, &raw, true); err != nil {
		return nil, err
	}
	out := make([]Listed, len(raw))
	for i, j := range raw {
		out[i] = j.listed("issue")
		if j.PullRequest != nil {
			out[i].Kind = "pr"
		}
	}
	return out, nil
}

// RepoComment is a comment on any issue or pull request of a repository.
type RepoComment struct {
	ID      int64
	Number  int // the issue or pull request it is on
	Author  string
	Body    string
	Updated time.Time
}

// RepoComments lists the comments on a repository's issues and pull requests that were created or
// edited since a time, oldest first, 100 to a page from page 1.
func (c *Client) RepoComments(ctx context.Context, token, name string, since time.Time, page int) ([]RepoComment, error) {
	path := fmt.Sprintf("/repos/%s/issues/comments?sort=created&direction=asc&per_page=100&page=%d", name, page)
	if !since.IsZero() {
		path += "&since=" + url.QueryEscape(since.UTC().Format(time.RFC3339))
	}
	return c.repoComments(ctx, token, path)
}

// LatestComment is the newest comment of a repository's issues and pull requests, or nil when there
// is none.
func (c *Client) LatestComment(ctx context.Context, token, name string) (*RepoComment, error) {
	cs, err := c.repoComments(ctx, token, "/repos/"+name+"/issues/comments?sort=created&direction=desc&per_page=1")
	if err != nil || len(cs) == 0 {
		return nil, err
	}
	return &cs[0], nil
}

func (c *Client) repoComments(ctx context.Context, token, path string) ([]RepoComment, error) {
	var raw []struct {
		ID        int64     `json:"id"`
		Body      string    `json:"body"`
		IssueURL  string    `json:"issue_url"`
		UpdatedAt time.Time `json:"updated_at"`
		User      struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if _, err := c.fetch(ctx, token, path, &raw, true); err != nil {
		return nil, err
	}
	var out []RepoComment
	for _, x := range raw {
		var n int
		if i := strings.LastIndex(x.IssueURL, "/"); i >= 0 {
			fmt.Sscanf(x.IssueURL[i+1:], "%d", &n)
		}
		out = append(out, RepoComment{x.ID, n, x.User.Login, x.Body, x.UpdatedAt})
	}
	return out, nil
}

// Review is a review submitted on a pull request.
type Review struct {
	Author    string
	State     string
	Submitted time.Time
}

// Reviews lists a pull request's reviews, oldest first, up to 100.
func (c *Client) Reviews(ctx context.Context, token, name string, number int) ([]Review, error) {
	var raw []struct {
		State       string    `json:"state"`
		SubmittedAt time.Time `json:"submitted_at"`
		User        struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if _, err := c.fetch(ctx, token, fmt.Sprintf("/repos/%s/pulls/%d/reviews?per_page=100", name, number), &raw, true); err != nil {
		return nil, err
	}
	out := make([]Review, len(raw))
	for i, x := range raw {
		out[i] = Review{x.User.Login, x.State, x.SubmittedAt}
	}
	return out, nil
}

// CIState reads the check runs of a commit: "pending" while any has not completed; once all have,
// "failed" if one failed, was cancelled or timed out, otherwise "passed"; "none" when the commit has
// no check runs. Commit statuses of the older kind are not read.
func (c *Client) CIState(ctx context.Context, token, name, sha string) (string, error) {
	if !refRE.MatchString(sha) {
		return "", fmt.Errorf("%q is not a commit", sha)
	}
	var r struct {
		CheckRuns []struct {
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"check_runs"`
	}
	if _, err := c.fetch(ctx, token, fmt.Sprintf("/repos/%s/commits/%s/check-runs?per_page=100", name, sha), &r, true); err != nil {
		return "", err
	}
	state := "none"
	for _, run := range r.CheckRuns {
		switch {
		case run.Status != "completed":
			return "pending", nil
		case run.Conclusion == "failure" || run.Conclusion == "cancelled" || run.Conclusion == "timed_out" ||
			run.Conclusion == "action_required" || run.Conclusion == "startup_failure":
			state = "failed"
		case state == "none":
			state = "passed"
		}
	}
	return state, nil
}

// Role is a user's role on a repository: admin, maintain, write, triage, read, or none for a user
// with no access (or no such user).
func (c *Client) Role(ctx context.Context, token, name, user string) (string, error) {
	if !loginRE.MatchString(user) {
		return "none", nil
	}
	var r struct {
		Permission string `json:"permission"`
		RoleName   string `json:"role_name"`
	}
	if _, err := c.get(ctx, token, fmt.Sprintf("/repos/%s/collaborators/%s/permission", name, url.PathEscape(user)), &r); err != nil {
		if errors.Is(err, ErrNoRepo) {
			return "none", nil
		}
		return "", err
	}
	if r.RoleName != "" {
		return r.RoleName, nil
	}
	return r.Permission, nil
}

// DefaultBranch is the branch a repository's pull requests target unless told otherwise.
func (c *Client) DefaultBranch(ctx context.Context, token, name string) (string, error) {
	var r struct {
		DefaultBranch string `json:"default_branch"`
	}
	if _, err := c.get(ctx, token, "/repos/"+name, &r); err != nil {
		return "", err
	}
	return r.DefaultBranch, nil
}
