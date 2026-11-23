package github

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// The reads behind the local copy of a repository's pull requests and issues (package index).
// All are GETs. Lists are 100 to a page. A caller that asks for the answer to be cached sends the
// ETag it was last given, and an unchanged answer (304) costs nothing against the rate limit.

// IndexPage is how many entries one page of these lists holds.
const IndexPage = 100

// ItemRecord is an issue or pull request as the repository's issue list gives it.
type ItemRecord struct {
	Number      int
	Kind        string // "pr" or "issue"
	Title       string
	Body        string
	State       string // "open" or "closed"
	Author      string
	Association string
	Labels      []string
	Draft       bool
	Comments    int // conversation comments
	Created     time.Time
	Updated     time.Time
	Closed      time.Time // zero while open
	MergedAt    time.Time // zero unless a merged pull request
}

// ItemQuery says which page of the repository's items (issues and pull requests, any state) to list.
type ItemQuery struct {
	State string    // "open" or "all" (the default)
	Sort  string    // "created" or "updated"; oldest first either way, so that pages do not shift as items change
	Since time.Time // only items updated at or after this; zero for all
	Page  int       // from 1
	Cache bool
}

type itemJSON struct {
	Number      int       `json:"number"`
	State       string    `json:"state"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	Draft       bool      `json:"draft"`
	Association string    `json:"author_association"`
	Comments    int       `json:"comments"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	ClosedAt    time.Time `json:"closed_at"`
	PullRequest *struct {
		MergedAt time.Time `json:"merged_at"`
	} `json:"pull_request"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

func (x itemJSON) record() ItemRecord {
	r := ItemRecord{Number: x.Number, Kind: "issue", Title: x.Title, Body: x.Body, State: x.State, Author: x.User.Login,
		Association: x.Association, Labels: names(x.Labels), Draft: x.Draft, Comments: x.Comments,
		Created: x.CreatedAt, Updated: x.UpdatedAt, Closed: x.ClosedAt}
	if x.PullRequest != nil {
		r.Kind, r.MergedAt = "pr", x.PullRequest.MergedAt
	}
	return r
}

// ListItems lists a repository's issues and pull requests, open and closed.
func (c *Client) ListItems(ctx context.Context, token, name string, q ItemQuery) ([]ItemRecord, error) {
	var raw []itemJSON
	state := "all"
	if q.State == "open" {
		state = "open"
	}
	path := fmt.Sprintf("/repos/%s/issues?state=%s&sort=%s&direction=asc&per_page=%d&page=%d", name, state, url.QueryEscape(q.Sort), IndexPage, q.Page)
	if !q.Since.IsZero() {
		path += "&since=" + url.QueryEscape(q.Since.UTC().Format(time.RFC3339))
	}
	if _, err := c.fetch(ctx, token, path, &raw, q.Cache); err != nil {
		return nil, err
	}
	out := make([]ItemRecord, len(raw))
	for i, x := range raw {
		out[i] = x.record()
	}
	return out, nil
}

// GetItem reads one issue or pull request.
func (c *Client) GetItem(ctx context.Context, token, name string, number int) (*ItemRecord, error) {
	var x itemJSON
	if _, err := c.get(ctx, token, fmt.Sprintf("/repos/%s/issues/%d", name, number), &x); err != nil {
		if errors.Is(err, ErrNoRepo) {
			return nil, fmt.Errorf("%s has no issue or pull request #%d that Autobot can see", name, number)
		}
		return nil, err
	}
	r := x.record()
	return &r, nil
}

// PullRecord is what a pull request's own page adds to its entry in the issue list.
type PullRecord struct {
	State                  string
	Merged                 bool
	MergedAt               time.Time
	Draft                  bool
	Base, HeadRef, HeadSHA string
	Additions, Deletions   int
	ReviewComments         int // inline review comments
	Updated                time.Time
}

// PullDetail reads one pull request.
func (c *Client) PullDetail(ctx context.Context, token, name string, number int) (*PullRecord, error) {
	var p struct {
		State     string    `json:"state"`
		Merged    bool      `json:"merged"`
		MergedAt  time.Time `json:"merged_at"`
		Draft     bool      `json:"draft"`
		Additions int       `json:"additions"`
		Deletions int       `json:"deletions"`
		Reviews   int       `json:"review_comments"`
		UpdatedAt time.Time `json:"updated_at"`
		Head      struct {
			SHA string `json:"sha"`
			Ref string `json:"ref"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	if _, err := c.get(ctx, token, fmt.Sprintf("/repos/%s/pulls/%d", name, number), &p); err != nil {
		return nil, err
	}
	return &PullRecord{State: p.State, Merged: p.Merged, MergedAt: p.MergedAt, Draft: p.Draft, Base: p.Base.Ref, HeadRef: p.Head.Ref,
		HeadSHA: p.Head.SHA, Additions: p.Additions, Deletions: p.Deletions, ReviewComments: p.Reviews, Updated: p.UpdatedAt}, nil
}

// CommentRecord is a conversation comment, a review, or an inline review comment.
type CommentRecord struct {
	Kind        string // "comment", "review" or "review_comment"
	ID          int64
	Number      int // the issue or pull request it is on
	Author      string
	Association string
	Body        string
	State       string // reviews: APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED
	Path        string // inline comments
	Line        int
	Created     time.Time
	Updated     time.Time
}

type commentJSON struct {
	ID          int64     `json:"id"`
	Body        string    `json:"body"`
	Association string    `json:"author_association"`
	State       string    `json:"state"`
	Path        string    `json:"path"`
	Line        int       `json:"line"`
	OrigLine    int       `json:"original_line"`
	IssueURL    string    `json:"issue_url"`
	PullURL     string    `json:"pull_request_url"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	SubmittedAt time.Time `json:"submitted_at"`
	User        struct {
		Login string `json:"login"`
	} `json:"user"`
}

// lastNumber is the number a GitHub address ends with.
func lastNumber(u string) int {
	var n int
	if i := strings.LastIndex(u, "/"); i >= 0 {
		fmt.Sscanf(u[i+1:], "%d", &n)
	}
	return n
}

func (x commentJSON) record(kind string, number int) CommentRecord {
	r := CommentRecord{Kind: kind, ID: x.ID, Number: number, Author: x.User.Login, Association: x.Association, Body: x.Body,
		State: x.State, Path: x.Path, Line: x.Line, Created: x.CreatedAt, Updated: x.UpdatedAt}
	if r.Line == 0 {
		r.Line = x.OrigLine // the line it was made on, when the code has since moved
	}
	return r
}

// CommentQuery says which page of a repository's comments to list.
type CommentQuery struct {
	Review bool      // inline review comments instead of conversation comments
	Since  time.Time // only those created or edited at or after this; zero for all
	Page   int       // from 1
	Cache  bool
}

// ListComments lists the comments of all of a repository's issues and pull requests (or all its
// inline review comments), the oldest change first.
func (c *Client) ListComments(ctx context.Context, token, name string, q CommentQuery) ([]CommentRecord, error) {
	end, kind := "issues/comments", "comment"
	if q.Review {
		end, kind = "pulls/comments", "review_comment"
	}
	path := fmt.Sprintf("/repos/%s/%s?sort=updated&direction=asc&per_page=%d&page=%d", name, end, IndexPage, q.Page)
	if !q.Since.IsZero() {
		path += "&since=" + url.QueryEscape(q.Since.UTC().Format(time.RFC3339))
	}
	var raw []commentJSON
	if _, err := c.fetch(ctx, token, path, &raw, q.Cache); err != nil {
		return nil, err
	}
	out := make([]CommentRecord, len(raw))
	for i, x := range raw {
		n := lastNumber(x.IssueURL)
		if q.Review {
			n = lastNumber(x.PullURL)
		}
		out[i] = x.record(kind, n)
	}
	return out, nil
}

// ItemComments lists the conversation comments of one issue or pull request, oldest first, one page.
func (c *Client) ItemComments(ctx context.Context, token, name string, number, page int) ([]CommentRecord, error) {
	var raw []commentJSON
	if _, err := c.get(ctx, token, fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=%d&page=%d", name, number, IndexPage, page), &raw); err != nil {
		return nil, err
	}
	out := make([]CommentRecord, len(raw))
	for i, x := range raw {
		out[i] = x.record("comment", number)
	}
	return out, nil
}

// PullComments lists the inline review comments of one pull request, oldest first, one page.
func (c *Client) PullComments(ctx context.Context, token, name string, number, page int) ([]CommentRecord, error) {
	var raw []commentJSON
	if _, err := c.get(ctx, token, fmt.Sprintf("/repos/%s/pulls/%d/comments?per_page=%d&page=%d", name, number, IndexPage, page), &raw); err != nil {
		return nil, err
	}
	out := make([]CommentRecord, len(raw))
	for i, x := range raw {
		out[i] = x.record("review_comment", number)
	}
	return out, nil
}

// ReviewRecords lists the reviews of a pull request (those submitted, not pending), oldest first.
func (c *Client) ReviewRecords(ctx context.Context, token, name string, number int) ([]CommentRecord, error) {
	var out []CommentRecord
	for page := 1; page <= 10; page++ {
		var raw []commentJSON
		if _, err := c.get(ctx, token, fmt.Sprintf("/repos/%s/pulls/%d/reviews?per_page=%d&page=%d", name, number, IndexPage, page), &raw); err != nil {
			return nil, err
		}
		for _, x := range raw {
			if x.SubmittedAt.IsZero() {
				continue
			}
			x.CreatedAt, x.UpdatedAt = x.SubmittedAt, x.SubmittedAt
			out = append(out, x.record("review", number))
		}
		if len(raw) < IndexPage {
			break
		}
	}
	return out, nil
}

// OpenCount is what GitHub says the repository has open: issues and pull requests together.
func (c *Client) OpenCount(ctx context.Context, token, name string) (int, error) {
	var r struct {
		Open int `json:"open_issues_count"`
	}
	if _, err := c.get(ctx, token, "/repos/"+name, &r); err != nil {
		return 0, err
	}
	return r.Open, nil
}
