package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/eidonix-ai/autobot/internal/store"
)

// ItemIndex is the local copy of one repository's pull requests and issues, as the item tools
// read it. *store.ItemIndex is one (made for a single workspace, so an agent cannot see another's);
// tests use a fake.
type ItemIndex interface {
	Search(q store.ItemSearch) ([]store.ItemHit, error)
	Read(number int) (*store.ItemDetail, error)
	Related(number int) (*store.ItemRelated, error)
	Touching(path string, limit int) ([]store.SharedFiles, error)
	// Fetch reads one item of any age live from GitHub, for what the local copy does not keep.
	Fetch(ctx context.Context, number int) (*store.ItemDetail, error)
}

// ItemTools search and read the repository's pull requests and issues from the local copy, without
// asking GitHub. The copy is a working copy: open items and items closed in the last 30 days are
// kept in full; closed pull requests up to 180 days old are kept as short records with their files.
// Anything older is on GitHub, and read_item fetches it from there.
func ItemTools(ix ItemIndex) []Tool {
	return []Tool{
		{Spec: ToolSpec{Name: "search_items", Description: "Search the repository's open pull requests and issues and those closed or merged in the last 30 days (older ones are not searched; read_item can still fetch one by number), including their descriptions and every comment, review and inline review comment. Words are matched by stem (retries finds retry) and all must appear. Returns the best matches with the words around the match.",
			Parameters: schema(`{"type":"object","properties":{"query":{"type":"string","description":"the words to find"},"kind":{"type":"string","enum":["pr","issue"],"description":"only pull requests or only issues"},"state":{"type":"string","enum":["open","closed","merged"],"description":"closed means closed without merging"},"author":{"type":"string"},"label":{"type":"string"},"limit":{"type":"integer","description":"1 to 50, default 10"}},"required":["query"]}`)},
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				a, err := args[store.ItemSearch](raw)
				if err != nil {
					return "", err
				}
				hits, err := ix.Search(a)
				if err != nil {
					return "", itemError(err, 0)
				}
				if len(hits) == 0 {
					return "No matches.", nil
				}
				var b strings.Builder
				for _, h := range hits {
					fmt.Fprintf(&b, "#%d %s (%s) %s, by %s\n   %s: %s\n", h.Number, kindName(h.Kind), h.State, h.Title, h.Author, h.Where, strings.Join(strings.Fields(h.Snippet), " "))
				}
				return b.String(), nil
			}},
		{Spec: ToolSpec{Name: "read_item", Description: "Read one pull request or issue of the repository by number, of any age: description, labels, the files a pull request changes, then the whole discussion in time order (comments, reviews, inline review comments with file and line), and what it links to. Open items and items closed in the last 30 days are read from the local copy; an older one is fetched from GitHub (slower).",
			Parameters: schema(`{"type":"object","properties":{"number":{"type":"integer"},"start":{"type":"integer"}},"required":["number"]}`)},
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				a, err := args[struct {
					Number int
					Start  int
				}](raw)
				if err != nil {
					return "", err
				}
				d, err := ix.Read(a.Number)
				if errors.Is(err, store.ErrNotFound) || (err == nil && d.Slim) {
					// The copy does not keep it in full: ask GitHub, and fall back on the short record if that fails.
					if live, ferr := ix.Fetch(ctx, a.Number); ferr == nil {
						d, err = live, nil
					} else if err != nil {
						return "", fmt.Errorf("the repository's local copy does not have #%d and GitHub could not be asked: %v", a.Number, ferr)
					}
				}
				if err != nil {
					return "", itemError(err, a.Number)
				}
				return page(formatItem(d), a.Start, 30_000), nil
			}},
		{Spec: ToolSpec{Name: "related_items", Description: "For one pull request or issue kept in the local copy (open, or closed in the last 180 days): the items it refers to, the items that refer to it, and the other pull requests, open or closed in the last 180 days, that change any of the same files (with the files they share).",
			Parameters: schema(`{"type":"object","properties":{"number":{"type":"integer"}},"required":["number"]}`)},
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				a, err := args[struct{ Number int }](raw)
				if err != nil {
					return "", err
				}
				r, err := ix.Related(a.Number)
				if err != nil {
					return "", itemError(err, a.Number)
				}
				var b strings.Builder
				refs(&b, "It refers to", r.Out)
				refs(&b, "Referred to by", r.In)
				if len(r.Shared) > 0 {
					b.WriteString("Other pull requests changing the same files:\n")
					for _, s := range r.Shared {
						fmt.Fprintf(&b, "  #%d (%s) %s, by %s: %d files in common: %s\n", s.Number, s.State, s.Title, s.Author, s.Count, strings.Join(s.Files, ", "))
					}
				}
				if b.Len() == 0 {
					return "Nothing refers to it, it refers to nothing, and no other pull request shares its files.", nil
				}
				return page(b.String(), 0, 20_000), nil
			}},
		{Spec: ToolSpec{Name: "items_touching", Description: "List the pull requests, open or merged in the last 180 days, that changed a file or anything under a folder (for example internal/web or internal/web/server.go), open ones first, then the most recently updated. Older merged pull requests are not listed.",
			Parameters: schema(`{"type":"object","properties":{"path":{"type":"string","description":"a file, or a folder prefix"},"limit":{"type":"integer","description":"1 to 100, default 30"}},"required":["path"]}`)},
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				a, err := args[struct {
					Path  string
					Limit int
				}](raw)
				if err != nil {
					return "", err
				}
				out, err := ix.Touching(a.Path, a.Limit)
				if err != nil {
					return "", itemError(err, 0)
				}
				if len(out) == 0 {
					return "No open or merged pull request changes that.", nil
				}
				var b strings.Builder
				for _, s := range out {
					fmt.Fprintf(&b, "#%d (%s) %s, by %s: %s", s.Number, s.State, s.Title, s.Author, strings.Join(s.Files, ", "))
					if s.Count > len(s.Files) {
						fmt.Fprintf(&b, " and %d more", s.Count-len(s.Files))
					}
					b.WriteString("\n")
				}
				return page(b.String(), 0, 20_000), nil
			}},
	}
}

func itemError(err error, number int) error {
	var in *store.InputError
	switch {
	case errors.As(err, &in):
		return in
	case errors.Is(err, store.ErrNotFound):
		return fmt.Errorf("the repository has no pull request or issue #%d in the local copy (open items and those closed in the last 180 days are kept)", number)
	}
	return err
}

func kindName(kind string) string {
	if kind == "pr" {
		return "pull request"
	}
	return "issue"
}

func refs(b *strings.Builder, head string, rs []store.ItemRef) {
	if len(rs) == 0 {
		return
	}
	b.WriteString(head + ":\n")
	for _, r := range rs {
		if r.Title == "" {
			fmt.Fprintf(b, "  #%d (%s; not in the local copy)\n", r.Number, r.How)
			continue
		}
		fmt.Fprintf(b, "  #%d %s (%s) %s [%s]\n", r.Number, kindName(r.Kind), r.State, r.Title, r.How)
	}
}

func day(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format("2006-01-02 15:04")
}

func formatItem(d *store.ItemDetail) string {
	var b strings.Builder
	state := d.State
	if d.Merged {
		state = "merged"
	}
	fmt.Fprintf(&b, "#%d %s (%s%s) %s\n", d.Number, kindName(d.Kind), state, map[bool]string{true: ", draft"}[d.Draft], d.Title)
	fmt.Fprintf(&b, "by %s", d.Author)
	if d.Association != "" && d.Association != "NONE" {
		fmt.Fprintf(&b, " (%s)", d.Association)
	}
	fmt.Fprintf(&b, ", opened %s, updated %s", day(d.Created), day(d.Updated))
	if d.Merged {
		fmt.Fprintf(&b, ", merged %s", day(d.MergedAt))
	} else if !d.Closed.IsZero() {
		fmt.Fprintf(&b, ", closed %s", day(d.Closed))
	}
	b.WriteString("\n")
	if len(d.Labels) > 0 {
		fmt.Fprintf(&b, "labels: %s\n", strings.Join(d.Labels, ", "))
	}
	if d.Kind == "pr" && d.Base != "" {
		fmt.Fprintf(&b, "%s into %s, head %s (%.7s), +%d -%d\n", d.HeadRef, d.Base, d.HeadSHA, d.HeadSHA, d.Additions, d.Deletions)
	}
	switch {
	case d.Remote:
		b.WriteString("(read live from GitHub: older than the local copy keeps)\n")
	case d.Slim:
		b.WriteString("(only the short record is kept locally: no description, discussion or links; the changed files are)\n")
	}
	fmt.Fprintf(&b, "\n%s\n", strings.TrimSpace(d.Body))
	if len(d.Files) > 0 {
		fmt.Fprintf(&b, "\nFiles changed (%d):\n", len(d.Files))
		for i, f := range d.Files {
			if i == 300 {
				fmt.Fprintf(&b, "  ... and %d more\n", len(d.Files)-i)
				break
			}
			fmt.Fprintf(&b, "  %s (%s, +%d -%d)\n", f.Path, f.Status, f.Additions, f.Deletions)
		}
	}
	fmt.Fprintf(&b, "\nDiscussion (%d):\n", len(d.Discussion))
	for _, c := range d.Discussion {
		switch c.Kind {
		case "review":
			fmt.Fprintf(&b, "--- %s, review by %s: %s\n", day(c.Created), c.Author, c.ReviewState)
		case "review_comment":
			fmt.Fprintf(&b, "--- %s, inline comment by %s on %s:%d\n", day(c.Created), c.Author, c.Path, c.Line)
		default:
			fmt.Fprintf(&b, "--- %s, %s\n", day(c.Created), c.Author)
		}
		if body := strings.TrimSpace(c.Body); body != "" {
			b.WriteString(body + "\n")
		}
	}
	if len(d.LinksOut)+len(d.LinksIn) > 0 {
		b.WriteString("\n")
		refs(&b, "It refers to", d.LinksOut)
		refs(&b, "Referred to by", d.LinksIn)
	}
	return b.String()
}
