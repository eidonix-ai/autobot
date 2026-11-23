package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Code is what the local code tools ask of a repository's local copy; *repo.Repo is one. A commit
// is always a full commit id, which Resolve makes from the ref a model gives.
type Code interface {
	Resolve(ctx context.Context, ref string) (string, error)
	Branch(ctx context.Context, name string) (string, error)
	ListFiles(ctx context.Context, commit string) ([]string, error)
	ReadFile(ctx context.Context, commit, path string) (string, error)
	Grep(ctx context.Context, commit, pattern, pathspec string, ignoreCase bool) (string, error)
	Log(ctx context.Context, commit, text, path string) (string, error)
	Diff(ctx context.Context, base, head string) (string, error)
	Overview(ctx context.Context, commit string) (string, error)
}

// LocalPR is the pull request the code tools work on, when there is one: they then read at its head.
type LocalPR struct {
	Number int
	Head   string   // its head commit
	Base   string   // the branch it merges into
	Files  []string // the paths it changes
}

const refHelp = `a commit id, main (or the default branch's name), or pr/<number> for a pull request's head`

// CodeTools read the repository from Autobot's own local copy: files, search, history and the pull
// request's diff, with no machine and no GitHub calls. With a pull request they read at its head,
// otherwise at the default branch; a "ref" argument reads somewhere else.
func CodeTools(c Code, pr *LocalPR) []Tool {
	where := "the default branch"
	if pr != nil {
		where = "the pull request's head"
	}
	commit := func(ctx context.Context, ref string) (string, error) {
		if ref == "" && pr != nil {
			return pr.Head, nil
		}
		return c.Resolve(ctx, ref)
	}
	refProp := `"ref":{"type":"string","description":"read at ` + refHelp + `; default ` + where + `"}`
	tools := []Tool{
		{Spec: ToolSpec{Name: "list_files", Description: "List the repository's files at " + where + ", optionally only those matching a pattern such as internal/*.go or *_test.go.",
			Parameters: schema(`{"type":"object","properties":{"pattern":{"type":"string"},"start":{"type":"integer"},` + refProp + `}}`)},
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				a, err := args[struct {
					Pattern string
					Start   int
					Ref     string
				}](raw)
				if err != nil {
					return "", err
				}
				sha, err := commit(ctx, a.Ref)
				if err != nil {
					return "", err
				}
				all, err := c.ListFiles(ctx, sha)
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
				return page(strings.Join(out, "\n"), a.Start, 30_000), nil
			}},
		{Spec: ToolSpec{Name: "read_file", Description: "Read a file of the repository at " + where + ", with line numbers, optionally only some lines.",
			Parameters: schema(`{"type":"object","properties":{"path":{"type":"string"},"from_line":{"type":"integer"},"to_line":{"type":"integer"},` + refProp + `},"required":["path"]}`)},
			Reads: func(raw json.RawMessage) []string {
				a, _ := args[struct{ Path string }](raw)
				return []string{a.Path}
			},
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				a, err := args[struct {
					Path     string
					FromLine int `json:"from_line"`
					ToLine   int `json:"to_line"`
					Ref      string
				}](raw)
				if err != nil {
					return "", err
				}
				sha, err := commit(ctx, a.Ref)
				if err != nil {
					return "", err
				}
				text, err := c.ReadFile(ctx, sha, a.Path)
				if err != nil {
					return "", err
				}
				return numbered(text, a.FromLine, a.ToLine), nil
			}},
		{Spec: ToolSpec{Name: "search_code", Description: "Search the repository's files at " + where + " for a regular expression (extended syntax, one line). " +
			"Returns path:line:text, at most 300 lines. Files over 1 MB and binary files are not searched. Optionally only files matching a path or glob such as internal/ or *.go.",
			Parameters: schema(`{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string","description":"a path or glob to search in"},"ignore_case":{"type":"boolean"},"start":{"type":"integer"},` + refProp + `},"required":["pattern"]}`)},
			Reads: func(raw json.RawMessage) []string {
				a, _ := args[struct{ Path string }](raw)
				if a.Path != "" && !strings.ContainsAny(a.Path, "*?[") && !strings.HasSuffix(a.Path, "/") {
					return []string{a.Path} // searching one file reads it
				}
				return nil
			},
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				a, err := args[struct {
					Pattern    string
					Path       string
					IgnoreCase bool `json:"ignore_case"`
					Start      int
					Ref        string
				}](raw)
				if err != nil {
					return "", err
				}
				sha, err := commit(ctx, a.Ref)
				if err != nil {
					return "", err
				}
				out, err := c.Grep(ctx, sha, a.Pattern, a.Path, a.IgnoreCase)
				if err != nil {
					return "", err
				}
				return page(out, a.Start, 30_000), nil
			}},
		{Spec: ToolSpec{Name: "search_history", Description: "List the repository's commits up to " + where + ", newest first, at most 50: those whose message contains a text (any case) " +
			"and/or that changed a file or folder. Returns the short commit id, date, author and subject.",
			Parameters: schema(`{"type":"object","properties":{"text":{"type":"string","description":"words in the commit message"},"path":{"type":"string","description":"a file or folder the commit changed"},` + refProp + `}}`)},
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				a, err := args[struct {
					Text string
					Path string
					Ref  string
				}](raw)
				if err != nil {
					return "", err
				}
				sha, err := commit(ctx, a.Ref)
				if err != nil {
					return "", err
				}
				return c.Log(ctx, sha, a.Text, a.Path)
			}},
		{Spec: ToolSpec{Name: "repo_overview", Description: "Describe the repository at " + where + ": its top-level folders with file counts, and its main file types.",
			Parameters: schema(`{"type":"object","properties":{` + refProp + `}}`)},
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				a, err := args[struct{ Ref string }](raw)
				if err != nil {
					return "", err
				}
				sha, err := commit(ctx, a.Ref)
				if err != nil {
					return "", err
				}
				return c.Overview(ctx, sha)
			}},
	}
	if pr == nil {
		return tools
	}
	return append(tools, Tool{Spec: ToolSpec{Name: "pr_diff", Description: "Read the pull request's diff against the branch it merges into, all of it or only the files whose paths contain a given text.",
		Parameters: schema(`{"type":"object","properties":{"path":{"type":"string","description":"only files whose path contains this"},"start":{"type":"integer"}}}`)},
		Reads: func(raw json.RawMessage) []string {
			a, _ := args[struct{ Path string }](raw)
			var out []string
			for _, f := range pr.Files {
				if strings.Contains(f, a.Path) {
					out = append(out, f)
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
			base, err := c.Branch(ctx, pr.Base)
			if err != nil {
				return "", fmt.Errorf("the branch %s is not in the local copy: %v", pr.Base, err)
			}
			diff, err := c.Diff(ctx, base, pr.Head)
			if err != nil {
				return "", err
			}
			diff = diffOf(diff, a.Path)
			if diff == "" {
				return "", fmt.Errorf("the pull request changes no file matching %q", a.Path)
			}
			return page(diff, a.Start, 20_000), nil
		}})
}

// diffOf keeps the parts of a git diff for files whose path contains a text (all of it for "").
func diffOf(diff, contains string) string {
	if contains == "" {
		return diff
	}
	var keep strings.Builder
	on := false
	for _, line := range strings.SplitAfter(diff, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			on = strings.Contains(line, contains)
		}
		if on {
			keep.WriteString(line)
		}
	}
	return keep.String()
}
