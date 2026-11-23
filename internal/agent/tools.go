package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/eidonix-ai/autobot/internal/github"
	"github.com/eidonix-ai/autobot/internal/netguard"
	"github.com/eidonix-ai/autobot/internal/search"
)

func schema(s string) json.RawMessage { return json.RawMessage(s) }

func args[T any](raw json.RawMessage) (T, error) {
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, errors.New("the arguments are not valid JSON for this tool")
	}
	return v, nil
}

// page returns a window of a long text, with a note saying how to read on.
func page(text string, start, size int) string {
	if start < 0 || start > len(text) {
		start = 0
	}
	end := min(start+size, len(text))
	out := strings.ToValidUTF8(text[start:end], "")
	if end < len(text) {
		out += fmt.Sprintf("\n[%d of %d characters shown; call again with start=%d for more]", end-start, len(text), end)
	}
	return out
}

// Reader is what the pull request tools ask of GitHub; *github.Client is one.
type Reader interface {
	Comments(ctx context.Context, token, name string, number int) ([]github.Comment, error)
	FileAt(ctx context.Context, token, name, ref, path string) (string, error)
	Tree(ctx context.Context, token, name, ref string) ([]string, error)
}

// PR is what the pull request tools read: one pull request of a repository.
type PR struct {
	GitHub Reader
	Token  string // the workspace's GitHub token, or "" for a public repository
	Repo   string
	Info   *github.PullRequest
	Files  []github.ChangedFile
}

// PRTools read the pull request and its repository at the pull request's head, through GitHub.
func PRTools(p *PR) []Tool {
	return []Tool{
		{Spec: ToolSpec{Name: "pr_files", Description: "List the files the pull request changes, with lines added and removed.",
			Parameters: schema(`{"type":"object","properties":{}}`)},
			Run: func(ctx context.Context, _ json.RawMessage) (string, error) {
				var b strings.Builder
				for _, f := range p.Files {
					fmt.Fprintf(&b, "%s (%s, +%d -%d)\n", f.Path, f.Status, f.Additions, f.Deletions)
				}
				return b.String(), nil
			}},
		{Spec: ToolSpec{Name: "pr_diff", Description: "Read the pull request's diff, all of it or only the files whose paths contain a given text.",
			Parameters: schema(`{"type":"object","properties":{"path":{"type":"string","description":"only files whose path contains this"},"start":{"type":"integer"}}}`)},
			Reads: func(raw json.RawMessage) []string {
				a, _ := args[struct{ Path string }](raw)
				var out []string
				for _, f := range p.Files {
					if strings.Contains(f.Path, a.Path) {
						out = append(out, f.Path)
					}
				}
				return out
			},
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				a, err := args[struct {
					Path  string
					Start int
				}](raw)
				if err != nil {
					return "", err
				}
				var files []github.ChangedFile
				for _, f := range p.Files {
					if strings.Contains(f.Path, a.Path) {
						files = append(files, f)
					}
				}
				if len(files) == 0 {
					return "", fmt.Errorf("the pull request changes no file matching %q", a.Path)
				}
				return page(github.Diff(files), a.Start, 20_000), nil
			}},
		{Spec: ToolSpec{Name: "pr_comments", Description: "Read the conversation on the pull request, oldest first.",
			Parameters: schema(`{"type":"object","properties":{}}`)},
			Run: func(ctx context.Context, _ json.RawMessage) (string, error) {
				cs, err := p.GitHub.Comments(ctx, p.Token, p.Repo, p.Info.Number)
				if err != nil {
					return "", err
				}
				if len(cs) == 0 {
					return "No comments.", nil
				}
				var b strings.Builder
				for _, c := range cs {
					fmt.Fprintf(&b, "--- %s, %s\n%s\n", c.Author, c.At.Format("2006-01-02 15:04"), c.Body)
				}
				return page(b.String(), 0, 40_000), nil
			}},
		{Spec: ToolSpec{Name: "list_files", Description: "List the repository's files at the pull request's head, optionally only those matching a pattern such as internal/*.go or *_test.go.",
			Parameters: schema(`{"type":"object","properties":{"pattern":{"type":"string"}}}`)},
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				a, err := args[struct{ Pattern string }](raw)
				if err != nil {
					return "", err
				}
				all, err := p.GitHub.Tree(ctx, p.Token, p.Repo, p.Info.HeadSHA)
				if err != nil {
					return "", err
				}
				var out []string
				for _, f := range all {
					if a.Pattern == "" || match(a.Pattern, f) {
						out = append(out, f)
					}
				}
				sort.Strings(out)
				if len(out) == 0 {
					return "No files match.", nil
				}
				return page(strings.Join(out, "\n"), 0, 30_000), nil
			}},
		{Spec: ToolSpec{Name: "read_file", Description: "Read a file of the repository at the pull request's head, with line numbers, optionally only some lines.",
			Parameters: schema(`{"type":"object","properties":{"path":{"type":"string"},"from_line":{"type":"integer"},"to_line":{"type":"integer"}},"required":["path"]}`)},
			Reads: func(raw json.RawMessage) []string {
				a, _ := args[struct{ Path string }](raw)
				return []string{a.Path}
			},
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				a, err := args[struct {
					Path     string
					FromLine int `json:"from_line"`
					ToLine   int `json:"to_line"`
				}](raw)
				if err != nil {
					return "", err
				}
				text, err := p.GitHub.FileAt(ctx, p.Token, p.Repo, p.Info.HeadSHA, a.Path)
				if err != nil {
					return "", err
				}
				return numbered(text, a.FromLine, a.ToLine), nil
			}},
	}
}

// match reports whether a path matches a pattern: a glob on the whole path, or on its file name
// when the pattern has no slash.
func match(pattern, p string) bool {
	if !strings.Contains(pattern, "/") {
		ok, _ := path.Match(pattern, path.Base(p))
		return ok
	}
	ok, _ := path.Match(pattern, p)
	return ok || strings.HasPrefix(p, strings.TrimSuffix(pattern, "*"))
}

// numbered shows lines from..to (1-based, inclusive; 0 for the start or the end) with their numbers.
func numbered(text string, from, to int) string {
	lines := strings.Split(text, "\n")
	if from < 1 {
		from = 1
	}
	if to < 1 || to > len(lines) {
		to = len(lines)
	}
	if to-from > 2000 {
		to = from + 2000
	}
	var b strings.Builder
	for i := from; i <= to && i <= len(lines); i++ {
		fmt.Fprintf(&b, "%6d  %s\n", i, lines[i-1])
	}
	if to < len(lines) {
		fmt.Fprintf(&b, "[lines %d to %d of %d; ask for more with from_line]\n", from, to, len(lines))
	}
	return b.String()
}

// WebTools search the web (when a search API is given) and fetch pages. A fetch may reach only
// public addresses: never Autobot's own network.
func WebTools(searcher func(ctx context.Context, query string, n int) ([]search.Result, error)) []Tool {
	var tools []Tool
	if searcher != nil {
		tools = append(tools, Tool{Spec: ToolSpec{Name: "web_search", Description: "Search the web. Returns titles, addresses and snippets.",
			Parameters: schema(`{"type":"object","properties":{"query":{"type":"string"},"count":{"type":"integer","description":"1 to 10, default 5"}},"required":["query"]}`)},
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				a, err := args[struct {
					Query string
					Count int
				}](raw)
				if err != nil {
					return "", err
				}
				if a.Count < 1 || a.Count > 10 {
					a.Count = 5
				}
				rs, err := searcher(ctx, a.Query, a.Count)
				if err != nil {
					return "", err
				}
				var b strings.Builder
				for i, r := range rs {
					fmt.Fprintf(&b, "%d. %s\n   %s\n   %s\n", i+1, r.Title, r.URL, r.Snippet)
				}
				if len(rs) == 0 {
					return "No results.", nil
				}
				return b.String(), nil
			}})
	}
	return append(tools, Tool{Spec: ToolSpec{Name: "fetch_url", Description: "Fetch a public web page or file and return its text.",
		Parameters: schema(`{"type":"object","properties":{"url":{"type":"string"},"start":{"type":"integer"}},"required":["url"]}`)},
		Reads: func(raw json.RawMessage) []string {
			a, _ := args[struct{ URL string }](raw)
			if u, err := url.Parse(a.URL); err == nil && u.Path != "" {
				return []string{u.Path} // a raw file's address ends with its path in the repository
			}
			return nil
		},
		Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
			a, err := args[struct {
				URL   string
				Start int
			}](raw)
			if err != nil {
				return "", err
			}
			text, err := Fetch(ctx, a.URL)
			if err != nil {
				return "", err
			}
			return page(text, a.Start, 20_000), nil
		}})
}

// Fetches go only to public addresses, never Autobot's own network: netguard's rule, with no
// exceptions (hosts an admin trusts for providers are not for agents).
var fetchClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, Control: netguard.PublicOnly}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		MaxIdleConns:          4,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		return nil
	},
}

var (
	dropBlocks = regexp.MustCompile(`(?is)<(script|style|noscript|svg|head)\b.*?</(script|style|noscript|svg|head)>`)
	breaks     = regexp.MustCompile(`(?i)<(br|/p|/div|/li|/h[1-6]|/tr|/pre)\b[^>]*>`)
	tags       = regexp.MustCompile(`<[^>]*>`)
	blankLines = regexp.MustCompile(`\n[ \t]*(\n[ \t]*)+`)
	spaces     = regexp.MustCompile(`[ \t]+`)
)

// Fetch reads a public web address, up to 4 MB, as text: a page's markup is reduced to its words.
func Fetch(ctx context.Context, raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("give a full http or https address")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "autobot")
	resp, err := fetchClient.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return "", fmt.Errorf("could not fetch %s: %v", u.Host, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("reading %s: %v", u.Host, err)
	}
	text := string(b)
	if strings.Contains(resp.Header.Get("Content-Type"), "html") {
		text = dropBlocks.ReplaceAllString(text, " ")
		text = breaks.ReplaceAllString(text, "\n")
		text = html.UnescapeString(tags.ReplaceAllString(text, " "))
		text = blankLines.ReplaceAllString(spaces.ReplaceAllString(text, " "), "\n\n")
	}
	return fmt.Sprintf("%s (%s)\n\n%s", resp.Request.URL, resp.Status, strings.TrimSpace(text)), nil
}
