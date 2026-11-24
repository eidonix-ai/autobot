package web

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/eidonix-ai/autobot/internal/store"
)

// The packet's sections about the item itself and its author.

// stateWord is "open", "closed" or "merged".
func (p *packet) stateWord() string {
	switch {
	case p.d != nil && p.d.Merged:
		return "merged"
	case p.d != nil && p.d.State != "":
		return p.d.State
	case p.info != nil && p.info.State != "":
		return p.info.State
	}
	return "unknown"
}

func (p *packet) labels() []string {
	switch {
	case p.d != nil && len(p.d.Labels) > 0:
		return p.d.Labels
	case p.info != nil:
		return p.info.Labels
	}
	return nil
}

func (p *packet) dates() (created, updated time.Time) {
	if p.d != nil {
		return p.d.Created, p.d.Updated
	}
	return
}

func (p *packet) association() string {
	switch {
	case p.info != nil && p.info.Association != "":
		return p.info.Association
	case p.d != nil && p.d.Association != "":
		return p.d.Association
	}
	return "unknown"
}

// labelsLine is a label list as a line.
func labelsLine(labels []string) string {
	if len(labels) == 0 {
		return "none"
	}
	return strings.Join(labels, ", ")
}

// pullSection is the pull request's own facts: state, age, size, labels, reviews and CI.
func (p *packet) pullSection(ctx context.Context) string {
	var rows []string
	ready := "ready for review"
	if (p.d != nil && p.d.Draft) || (p.info != nil && p.info.Draft) {
		ready = "a draft"
	}
	rows = append(rows, fmt.Sprintf("- State: %s, %s", p.stateWord(), ready))
	created, updated := p.dates()
	if p.d == nil {
		rows = append(rows, "- Opened and last updated: "+notAvailable(p.dWhy))
	} else {
		rows = append(rows, fmt.Sprintf("- Opened %s; last updated %s", ago(p.now, created), ago(p.now, updated)))
	}
	base, head := "", ""
	if p.info != nil {
		base, head = p.info.Base, p.info.HeadSHA
	}
	if p.d != nil {
		base, head = cmp.Or(base, p.d.Base), cmp.Or(head, p.d.HeadSHA)
	}
	rows = append(rows, fmt.Sprintf("- Base branch: %s; head commit: %s", cmp.Or(base, "unknown"), cmp.Or(head[:min(7, len(head))], "unknown")))
	add, del := 0, 0
	for _, f := range p.files {
		add, del = add+f.Additions, del+f.Deletions
	}
	if len(p.files) == 0 && p.info != nil {
		add, del = p.info.Additions, p.info.Deletions
	}
	rows = append(rows, fmt.Sprintf("- Size: %d file%s, +%d/−%d", len(p.files), plural(len(p.files)), add, del))
	rows = append(rows, "- Labels: "+labelsLine(p.labels()))
	rows = append(rows, "- Reviews: "+p.reviewState(ctx))
	rows = append(rows, "- CI on the head commit: "+p.ciState(ctx, head))
	return strings.Join(rows, "\n")
}

// reviewState says who approved, who asked for changes and who only commented, by each person's
// latest review.
func (p *packet) reviewState(ctx context.Context) string {
	type review struct {
		who, state string
		at         time.Time
	}
	var all []review
	if p.d != nil && !p.d.Slim {
		for _, c := range p.d.Discussion {
			if c.Kind == "review" {
				all = append(all, review{c.Author, c.ReviewState, c.Created})
			}
		}
	} else {
		rctx, cancel := context.WithTimeout(ctx, ciTime)
		defer cancel()
		list, err := p.s.gh.Reviews(rctx, p.token, p.ws.Name, p.number)
		if err != nil {
			return notAvailable("GitHub did not answer: " + err.Error())
		}
		for _, r := range list {
			all = append(all, review{r.Author, r.State, r.Submitted})
		}
	}
	slices.SortStableFunc(all, func(a, b review) int { return a.at.Compare(b.at) })
	latest := map[string]review{}
	for _, r := range all {
		if old, ok := latest[r.who]; !ok || r.state != "COMMENTED" || old.state == "COMMENTED" { // a comment does not undo a verdict
			latest[r.who] = r
		}
	}
	by := map[string][]string{}
	for who, r := range latest {
		by[r.state] = append(by[r.state], who)
	}
	var parts []string
	for _, s := range []struct{ state, label string }{{"APPROVED", "approved by"}, {"CHANGES_REQUESTED", "changes requested by"},
		{"COMMENTED", "commented by"}, {"DISMISSED", "dismissed from"}} {
		if who := by[s.state]; len(who) > 0 {
			slices.Sort(who)
			parts = append(parts, s.label+" "+strings.Join(who, ", "))
		}
	}
	if len(parts) == 0 {
		return "none yet"
	}
	return strings.Join(parts, "; ")
}

// ciState is the head commit's check state from GitHub, or "unknown" when it does not answer in time.
func (p *packet) ciState(ctx context.Context, head string) string {
	if head == "" {
		return "unknown"
	}
	cctx, cancel := context.WithTimeout(ctx, ciTime)
	defer cancel()
	state, err := p.s.gh.CIState(cctx, p.token, p.ws.Name, head)
	if err != nil || state == "" {
		return "unknown"
	}
	if state == "none" {
		return "no checks"
	}
	return state
}

// comments counts the conversation comments the copy holds.
func (p *packet) comments() int {
	n := 0
	for _, c := range p.d.Discussion {
		if c.Kind == "comment" {
			n++
		}
	}
	return n
}

// issueSection is the issue's own facts.
func (p *packet) issueSection() string {
	rows := []string{"- State: " + p.stateWord()}
	created, updated := p.dates()
	if p.d == nil {
		rows = append(rows, "- Opened and last updated: "+notAvailable(p.dWhy))
	} else {
		rows = append(rows, fmt.Sprintf("- Opened %s; last updated %s", ago(p.now, created), ago(p.now, updated)))
		rows = append(rows, fmt.Sprintf("- Comments: %d", p.comments()))
	}
	rows = append(rows, "- Labels: "+labelsLine(p.labels()))
	rows = append(rows, "- Reported by: "+cmp.Or(p.author, "unknown"))
	return strings.Join(rows, "\n")
}

// itemLine is an item as a line, with its state and kind.
func itemLine(b store.ItemBrief) string {
	return fmt.Sprintf("#%d (%s, %s) %s", b.Number, b.State, b.Kind, short(b.Title, 100))
}

// authorSection is the author's standing and recent work in the project.
func (p *packet) authorSection() string {
	author := p.author
	if p.d != nil {
		author = cmp.Or(author, p.d.Author)
	}
	if author == "" {
		return notAvailable("the author is not known")
	}
	rows := []string{"- " + author + ", GitHub's association with the repository: " + p.association()}
	if p.ix == nil {
		return strings.Join(append(rows, "- History: "+notAvailable(p.ixWhy)), "\n")
	}
	since := p.now.AddDate(0, 0, -peopleDays)
	list, err := p.ix.List(store.ItemList{Author: author, CreatedSince: since, Limit: 500})
	if err != nil {
		return strings.Join(append(rows, "- History: "+notAvailable(err.Error())), "\n")
	}
	list = slices.DeleteFunc(list, func(b store.ItemBrief) bool { return b.Number == p.number })
	merged, closed, open := 0, 0, 0
	for _, b := range list {
		switch b.State {
		case "merged":
			merged++
		case "closed":
			closed++
		default:
			open++
		}
	}
	rows = append(rows, fmt.Sprintf("- Their other pull requests and issues in the last %d days: %d merged, %d closed, %d open", peopleDays, merged, closed, open))
	for _, b := range list[:min(10, len(list))] {
		rows = append(rows, fmt.Sprintf("  - %s, %s", itemLine(b), b.Created.Format("2006-01-02")))
	}
	if len(list) > 10 {
		rows = append(rows, fmt.Sprintf("  - …%d more", len(list)-10))
	}
	if line := p.authorAreas(author, since); line != "" {
		rows = append(rows, line)
	}
	return strings.Join(rows, "\n")
}

// authorAreas are the areas their recently merged pull requests touched, by how many pull requests.
func (p *packet) authorAreas(author string, since time.Time) string {
	files, err := p.ix.MergedFiles(author, since)
	if err != nil || len(files) == 0 {
		return ""
	}
	count := map[string]int{}
	for _, paths := range files {
		seen := map[string]bool{}
		for _, f := range paths {
			if a := areaOf(f); !seen[a] {
				seen[a] = true
				count[a]++
			}
		}
	}
	names := make([]string, 0, len(count))
	for a := range count {
		names = append(names, a)
	}
	slices.SortFunc(names, func(a, b string) int { return cmp.Or(count[b]-count[a], strings.Compare(a, b)) })
	var parts []string
	for _, a := range names[:min(5, len(names))] {
		parts = append(parts, fmt.Sprintf("%s (%d)", a, count[a]))
	}
	return "- Areas their merged pull requests touched: " + strings.Join(parts, ", ")
}
