package web

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/eidonix-ai/autobot/internal/github"
	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// The packet's section of possibly related items: candidates from the index, each found by one or
// more directions, ranked by how many.

const (
	maxTerms      = 30 // search terms from the diff or the issue text
	commonHits    = 20 // a term that finds this many items says nothing: it is dropped
	relatedShown  = 15
	minNameLength = 5 // the shortest identifier worth searching for
)

// cand is an item some direction found.
type cand struct {
	number int
	dirs   []string            // the directions, in the order found, each once
	detail map[string][]string // a direction's detail ("names": the terms), at most 3
}

// cands collects candidates by direction.
type cands map[int]*cand

func (c cands) add(number int, dir, detail string) {
	x := c[number]
	if x == nil {
		x = &cand{number: number, detail: map[string][]string{}}
		c[number] = x
	}
	if !slices.Contains(x.dirs, dir) {
		x.dirs = append(x.dirs, dir)
	}
	if d := x.detail[dir]; detail != "" && len(d) < 3 && !slices.Contains(d, detail) {
		x.detail[dir] = append(d, detail)
	}
}

// relatedSection finds candidates in the index from every direction, and lists the best.
func (p *packet) relatedSection() string {
	if p.ix == nil {
		return notAvailable(p.ixWhy)
	}
	c := cands{}
	p.byLinks(c)
	p.byNames(c)
	p.byTitle(c)
	p.byAuthor(c)
	p.byLabels(c)
	p.byFiles(c)
	delete(c, p.number)
	if len(c) == 0 {
		return "No candidates found in the local copy of pull requests and issues."
	}
	numbers := make([]int, 0, len(c))
	for n := range c {
		numbers = append(numbers, n)
	}
	briefs, _ := p.ix.Briefs(numbers)
	sort.Slice(numbers, func(i, j int) bool {
		a, b := c[numbers[i]], c[numbers[j]]
		if len(a.dirs) != len(b.dirs) {
			return len(a.dirs) > len(b.dirs)
		}
		if ua, ub := briefs[a.number].Updated, briefs[b.number].Updated; !ua.Equal(ub) {
			return ua.After(ub)
		}
		return a.number > b.number
	})
	var rows []string
	for _, n := range numbers {
		x := c[n]
		var how []string
		for _, d := range x.dirs {
			if det := x.detail[d]; len(det) > 0 {
				d += " (" + strings.Join(det, ", ") + ")"
			}
			how = append(how, d)
		}
		what := fmt.Sprintf("#%d (not in the local copy)", n)
		if b, ok := briefs[n]; ok {
			what = itemLine(b)
		}
		rows = append(rows, fmt.Sprintf("- %s — found by: %s", what, strings.Join(how, ", ")))
	}
	return "Candidates from the local copy of pull requests and issues, most directions first, then most recent:\n" + lines(rows, relatedShown)
}

// byLinks: what this item refers to, and what refers to it.
func (p *packet) byLinks(c cands) {
	if p.d == nil {
		return
	}
	for _, r := range p.d.LinksOut {
		c.add(r.Number, "links", "this "+r.How)
	}
	for _, r := range p.d.LinksIn {
		c.add(r.Number, "links", "it "+r.How+" this")
	}
}

// byNames: items whose text names a changed file or an identifier added or removed.
func (p *packet) byNames(c cands) {
	var terms []string
	if p.kind == workflow.PR {
		terms = diffTerms(p.diff, p.files)
	} else {
		terms = textTerms(p.title + "\n" + p.body)
	}
	for _, t := range terms {
		hits, err := p.ix.Search(store.ItemSearch{Query: t, Phrase: true, Limit: 50})
		if err != nil || len(hits) >= commonHits {
			continue
		}
		for _, h := range hits {
			c.add(h.Number, "names", t)
		}
	}
}

// byTitle: items about the title's words.
func (p *packet) byTitle(c cands) {
	words := titleTerms(p.title)
	if len(words) == 0 {
		return
	}
	hits, err := p.ix.Search(store.ItemSearch{Query: strings.Join(words, " "), Any: true, Limit: 20})
	if err != nil {
		return
	}
	for _, h := range hits {
		c.add(h.Number, "title words", "")
	}
}

// byAuthor: the author's own open items.
func (p *packet) byAuthor(c cands) {
	author := p.author
	if p.d != nil && author == "" {
		author = p.d.Author
	}
	if author == "" {
		return
	}
	list, _ := p.ix.List(store.ItemList{Author: author, OpenOnly: true, Limit: 10})
	for _, b := range list {
		c.add(b.Number, "same author", "")
	}
}

// byLabels: recent items that share a label.
func (p *packet) byLabels(c cands) {
	for _, l := range p.labels()[:min(5, len(p.labels()))] {
		list, _ := p.ix.List(store.ItemList{Label: l, Limit: 10})
		for _, b := range list {
			c.add(b.Number, "same label", l)
		}
	}
}

// byFiles: pull requests that change the same files, open or merged.
func (p *packet) byFiles(c cands) {
	if p.kind != workflow.PR {
		return
	}
	rel, err := p.ix.Related(p.number)
	if err != nil {
		return
	}
	for _, s := range rel.Shared {
		if s.State == "open" || s.State == "merged" {
			c.add(s.Number, "same files", fmt.Sprintf("%d in common", s.Count))
		}
	}
}

var (
	declRE   = regexp.MustCompile(`\b(?:fn|func|def|class|struct|enum)\s+(?:\([^)]*\)\s*)?([A-Za-z_][A-Za-z0-9_]{4,})|\btype\s+([A-Za-z_][A-Za-z0-9_]{4,})\s+(?:struct|interface)\b`)
	flagRE   = regexp.MustCompile(`--[a-z][a-z0-9-]{2,}`)
	metricRE = regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9]*(?:_[A-Za-z0-9]+)+\b`)
	tickRE   = regexp.MustCompile("`([^`\\s]{4,60})`")
	pathRE   = regexp.MustCompile(`[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)+`)
)

// rankTerms keeps each distinct term once, longest first, at most maxTerms.
func rankTerms(terms []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range terms {
		if k := strings.ToLower(t); !seen[k] {
			seen[k] = true
			out = append(out, t)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out[:min(maxTerms, len(out))]
}

// codeNames are the flags and the metric-like names (an underscore, at least 12 characters) in a text.
func codeNames(text string) []string {
	out := flagRE.FindAllString(text, -1)
	for _, m := range metricRE.FindAllString(text, -1) {
		if len(m) >= 12 {
			out = append(out, m)
		}
	}
	return out
}

// diffTerms are what to look for in other items' text: the base names of the changed files, and
// the names of functions and types, command-line flags and metrics on the diff's added and removed
// lines.
func diffTerms(diff string, files []github.ChangedFile) []string {
	var terms []string
	for _, f := range files {
		base := path.Base(f.Path)
		if len(strings.TrimSuffix(base, path.Ext(base))) >= 4 {
			terms = append(terms, base)
		}
	}
	for _, line := range strings.Split(diff, "\n") {
		if len(line) < 2 || (line[0] != '+' && line[0] != '-') || strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") {
			continue
		}
		for _, m := range declRE.FindAllStringSubmatch(line, -1) {
			if n := m[1] + m[2]; len(n) >= minNameLength {
				terms = append(terms, n)
			}
		}
		terms = append(terms, codeNames(line)...)
	}
	return rankTerms(terms)
}

// textTerms are the names an issue's text uses: things in backticks, file names and paths, flags and
// metric-like names.
func textTerms(text string) []string {
	var terms []string
	for _, m := range tickRE.FindAllStringSubmatch(text, -1) {
		terms = append(terms, strings.Trim(m[1], ".,;:()"))
	}
	for _, p := range pathRE.FindAllString(text, -1) {
		if strings.Contains(path.Base(p), ".") {
			terms = append(terms, path.Base(p))
		}
	}
	terms = append(terms, codeNames(text)...)
	terms = slices.DeleteFunc(terms, func(t string) bool { return len(t) < minNameLength })
	return rankTerms(terms)
}

// issueAreas are the areas of the repository paths an issue's text names that exist at the default branch.
func (p *packet) issueAreas(ctx context.Context) []area {
	if p.r == nil {
		return nil
	}
	var found []string
	for _, cand := range pathRE.FindAllString(p.title+"\n"+p.body, -1) {
		cand = strings.Trim(cand, "./")
		if strings.Contains(cand, "/") && !slices.Contains(found, cand) && p.r.HasPath(ctx, p.tip, cand) {
			found = append(found, cand)
		}
		if len(found) == 10 {
			break
		}
	}
	by := map[string]*area{}
	var order []string
	for _, f := range found {
		name := areaOf(f) // a file: its folder is the area
		if !strings.Contains(path.Base(f), ".") {
			name = areaOf(f + "/x") // a folder: itself, cut to two segments
		}
		a := by[name]
		if a == nil {
			a = &area{Name: name, Specified: true}
			by[name] = a
			order = append(order, name)
		}
		a.Files = append(a.Files, f)
	}
	var list []area
	for _, n := range order {
		list = append(list, *by[n])
	}
	return list
}

var stopWords = map[string]bool{"the": true, "and": true, "for": true, "with": true, "from": true, "that": true, "this": true, "into": true,
	"when": true, "what": true, "are": true, "not": true, "add": true, "fix": true, "use": true, "make": true, "support": true,
	"update": true, "should": true, "would": true, "could": true, "after": true, "before": true, "have": true, "does": true}

// titleTerms are a title's significant words: four letters or more, not common ones, no more than 8.
func titleTerms(title string) []string {
	var out []string
	for _, w := range wordSplit(strings.ToLower(title)) {
		if len(w) >= 4 && !stopWords[w] && !slices.Contains(out, w) {
			out = append(out, w)
		}
	}
	return out[:min(8, len(out))]
}

var wordRE = regexp.MustCompile(`[\p{L}\p{N}]+`)

func wordSplit(s string) []string { return wordRE.FindAllString(s, -1) }
