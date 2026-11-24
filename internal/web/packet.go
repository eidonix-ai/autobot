package web

import (
	"context"
	"fmt"
	"log"
	"path"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/eidonix-ai/autobot/internal/github"
	"github.com/eidonix-ai/autobot/internal/index"
	"github.com/eidonix-ai/autobot/internal/repo"
	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// The orientation packet (Derek, 2026-11-23): what Autobot itself can tell about a pull request or
// issue from its own copies, with no model, put in the Gather Information agent's first message as
// {pr:packet} or {issue:packet}. It is a map of where to look, not conclusions: every line points
// at something (a commit, an item, a path, a charter line) that the agent then opens. A source that
// is not there (no code copy yet, no item index, GitHub down) leaves its section saying so; building
// the packet never fails the run.

const packetHeading = "## Orientation packet (computed by Autobot: a map of where to look, not conclusions)"

// The size limits, in characters: of the whole packet, whatever its sections add up to, and of one
// section, so that none crowds out the rest. Variables so that a test can lower them.
var (
	packetLimit  = 12000
	sectionLimit = 3000
)

const (
	packetTime    = 45 * time.Second
	ciTime        = 5 * time.Second
	recentDays    = 60  // how far back "recent activity" looks
	peopleDays    = 180 // how far back the authors and "who works here" look
	activityAreas = 8   // the areas that get recent activity and people
	areaLines     = 25  // the areas listed
)

// packetNow is the time the packet counts ages from; tests move it.
var packetNow = time.Now

// packet is one orientation packet being built: the item, and the sources it reads, each of which
// may be missing (and says why).
type packet struct {
	s      *Server
	ws     *store.Workspace
	token  string
	kind   string
	number int
	now    time.Time

	title, body, author string
	info                *github.PullRequest  // pull requests: what GitHub or the copy said
	files               []github.ChangedFile // pull requests: what it changes
	diff                string

	ix    *store.ItemIndex // nil: the copy of pull requests and issues is not ready
	ixWhy string
	d     *store.ItemDetail // nil: the item is not in the copy
	dWhy  string
	r     *repo.Repo // nil: no copy of the code
	rWhy  string
	tip   string // the default branch's head, "" when r is nil or cannot say

	areas []area // pull requests: the areas changed; issues: the areas of the paths its text names
}

// area is a folder (the first two segments of a changed file's folder) and what changed in it.
type area struct {
	Name      string   // "helios/src", "docs" or "(root)"
	Files     []string // changed paths in it
	Add, Del  int
	Specified bool // an issue's: the paths it names
}

const rootArea = "(root)"

// areaOf is the area of a file: its folder cut to two segments, or "(root)" for a file at the top.
func areaOf(file string) string {
	dir := path.Dir(file)
	if dir == "." {
		return rootArea
	}
	segs := strings.Split(dir, "/")
	return strings.Join(segs[:min(2, len(segs))], "/")
}

// pathspecs are what a git log is asked about for an area: its folder, or for the root its files.
func (a area) pathspecs() []string {
	if a.Name == rootArea {
		return a.Files
	}
	return []string{a.Name}
}

// groupAreas groups changed files by area, the biggest change first.
func groupAreas(files []github.ChangedFile) []area {
	by := map[string]*area{}
	for _, f := range files {
		name := areaOf(f.Path)
		a := by[name]
		if a == nil {
			a = &area{Name: name}
			by[name] = a
		}
		a.Files = append(a.Files, f.Path)
		a.Add += f.Additions
		a.Del += f.Deletions
	}
	list := make([]area, 0, len(by))
	for _, a := range by {
		list = append(list, *a)
	}
	sort.Slice(list, func(i, j int) bool {
		li, lj := list[i].Add+list[i].Del, list[j].Add+list[j].Del
		if li != lj {
			return li > lj
		}
		return list[i].Name < list[j].Name
	})
	return list
}

// orientation builds the packet of a pull request or issue. It never fails: what cannot be read is
// said in its section, and a bug in the packet itself costs only the packet.
func (s *Server) orientation(ctx context.Context, ws *store.Workspace, token, kind string, number int, values map[string]string,
	info *github.PullRequest, files []github.ChangedFile) (text string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("orientation packet of %s #%d: %v", ws.Name, number, r)
			text = packetHeading + "\n\n(not available: Autobot's own error while building it)"
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, packetTime)
	defer cancel()
	tag := workflow.ItemTag(kind)
	p := &packet{s: s, ws: ws, token: token, kind: kind, number: number, now: packetNow(),
		title: values[tag+":title"], body: values[tag+":body"], author: values[tag+":author"], diff: values["pr:diff"], info: info, files: files}
	if number <= 0 {
		return packetHeading + "\n\n(not available: no pull request or issue was given)"
	}
	p.open(ctx)
	return p.build(ctx)
}

// open connects the sources: the item index, this item in it, the code copy and its default branch.
func (p *packet) open(ctx context.Context) {
	s := p.s
	if index.Ready(s.st, p.ws.ID) {
		p.ix = s.st.ItemIndex(p.ws.ID)
		if d, err := p.ix.Read(p.number); err != nil {
			p.dWhy = fmt.Sprintf("#%d is not in the local copy of pull requests and issues", p.number)
		} else {
			p.d = d
		}
	} else {
		p.ixWhy = "the local copy of pull requests and issues is not ready yet"
		p.dWhy = p.ixWhy
	}
	if s.repos == nil || !s.repos.Ready(p.ws.ID) {
		p.rWhy = "the local copy of the code is not ready yet"
	} else {
		p.r = s.repos.Repo(p.ws.ID, p.token)
		var err error
		if p.tip, err = p.r.DefaultTip(ctx); err != nil {
			p.r, p.rWhy = nil, "the local copy of the code has no default branch: "+err.Error()
		}
	}
	if p.kind == workflow.PR {
		if len(p.files) == 0 && p.d != nil {
			for _, f := range p.d.Files {
				p.files = append(p.files, github.ChangedFile{Path: f.Path, Status: f.Status, Additions: f.Additions, Deletions: f.Deletions})
			}
		}
		p.areas = groupAreas(p.files)
		return
	}
	p.areas = p.issueAreas(ctx)
}

// build puts the sections together and cuts the whole to the hard limit.
func (p *packet) build(ctx context.Context) string {
	var out []string
	add := func(heading, text string) {
		if text != "" {
			out = append(out, "### "+heading+"\n"+capText(text, sectionLimit))
		}
	}
	if p.kind == workflow.PR {
		add("The pull request", p.pullSection(ctx))
		add("Areas changed", p.areasSection())
		add("Recent activity in those areas", p.activitySection(ctx))
		add("Possibly related items", p.relatedSection())
		add("The author", p.authorSection())
		add("People in these areas", p.peopleSection(ctx))
	} else {
		add("The issue", p.issueSection())
		if len(p.areas) > 0 {
			add("Repository paths the issue names", p.areasSection())
			add("Recent activity in those areas", p.activitySection(ctx))
		}
		add("Possibly related items", p.relatedSection())
		add("The author", p.authorSection())
		if len(p.areas) > 0 {
			add("People in these areas", p.peopleSection(ctx))
		}
	}
	add("The charter", p.charterSection())
	// Later: a "Community (Discord)" section goes here, from what the project's server says about the
	// item (the Discord bot's gateway reads, once it exists). Nothing is written for it yet.
	return capText(packetHeading+"\n\n"+strings.Join(out, "\n\n"), packetLimit)
}

// capText cuts a text to at most limit characters at a line end, saying so.
func capText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	note := "\n…more (cut at " + fmt.Sprint(limit) + " characters)"
	cut := text[:max(0, limit-len(note))]
	if i := strings.LastIndex(cut, "\n"); i >= 0 {
		cut = cut[:i]
	}
	return strings.ToValidUTF8(cut, "") + note
}

// notAvailable is a section's text when its source is missing.
func notAvailable(why string) string { return "(not available: " + why + ")" }

// lines joins lines, keeping the first n and saying how many more there were.
func lines(list []string, n int) string {
	if len(list) > n {
		list = append(slices.Clone(list[:n]), fmt.Sprintf("- …%d more", len(list)-n))
	}
	return strings.Join(list, "\n")
}

// ago says how long before now a time was, in days (or "today").
func ago(now, t time.Time) string {
	days := int(now.Sub(t).Hours() / 24)
	switch {
	case t.IsZero():
		return "unknown"
	case days <= 0:
		return "today (" + t.Format("2006-01-02") + ")"
	case days == 1:
		return "1 day ago (" + t.Format("2006-01-02") + ")"
	}
	return fmt.Sprintf("%d days ago (%s)", days, t.Format("2006-01-02"))
}

// short cuts a text to n characters.
func short(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// hasWord reports whether text has word as a whole word, in any case (both are lower case already).
func hasWord(text, word string) bool {
	if word == "" {
		return false
	}
	for from := 0; ; {
		i := strings.Index(text[from:], word)
		if i < 0 {
			return false
		}
		i += from
		end := i + len(word)
		before, _ := utf8.DecodeLastRuneInString(text[:i])
		after, _ := utf8.DecodeRuneInString(text[end:])
		if (i == 0 || !wordRune(before)) && (end == len(text) || !wordRune(after)) {
			return true
		}
		from = i + 1
	}
}

func wordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' }

// genericFolders are folder names too common to say anything about an area.
var genericFolders = map[string]bool{"src": true, "lib": true, "internal": true, "pkg": true, "cmd": true, "test": true,
	"tests": true, "include": true, "app": true, "main": true, "core": true, "common": true, "util": true, "utils": true}

// folderNames are the names of an area's folders worth looking for in a text.
func folderNames(a area) []string {
	var out []string
	for _, seg := range strings.Split(a.Name, "/") {
		if seg != rootArea && len(seg) >= 3 && !genericFolders[strings.ToLower(seg)] {
			out = append(out, seg)
		}
	}
	if len(out) == 0 && a.Name != rootArea {
		out = append(out, a.Name)
	}
	return out
}

// mentioned reports whether the text names the area: one of its folders, or any of its files by
// name or by name without its extension (any case, whole words).
func mentioned(text string, a area) bool {
	text = strings.ToLower(text)
	for _, n := range folderNames(a) {
		if hasWord(text, strings.ToLower(n)) {
			return true
		}
	}
	for _, f := range a.Files {
		base := path.Base(f)
		stem := strings.TrimSuffix(base, path.Ext(base))
		for _, n := range []string{base, stem} {
			if len(n) >= 3 && hasWord(text, strings.ToLower(n)) {
				return true
			}
		}
	}
	return false
}

// areasSection lists the areas with their size and whether the description mentions each.
func (p *packet) areasSection() string {
	if len(p.areas) == 0 {
		return "(no changed files known)"
	}
	var rows, missing []string
	for _, a := range p.areas {
		said := "yes"
		if !mentioned(p.body, a) {
			said, missing = "no", append(missing, a.Name)
		}
		names := make([]string, 0, 4)
		for _, f := range a.Files[:min(4, len(a.Files))] {
			names = append(names, path.Base(f))
		}
		more := ""
		if len(a.Files) > len(names) {
			more = ", …"
		}
		size := fmt.Sprintf("%d file%s", len(a.Files), plural(len(a.Files)))
		if !a.Specified {
			size += fmt.Sprintf(", +%d/−%d", a.Add, a.Del)
		}
		rows = append(rows, fmt.Sprintf("- %s: %s (%s%s); in the description: %s", a.Name, size, strings.Join(names, ", "), more, said))
	}
	text := lines(rows, areaLines)
	if len(missing) > 0 {
		list := strings.Join(missing[:min(len(missing), 12)], ", ")
		if len(missing) > 12 {
			list += fmt.Sprintf(", …%d more", len(missing)-12)
		}
		text += "\nNot mentioned in the description: " + list
	}
	if p.kind != workflow.PR {
		text = strings.ReplaceAll(text, "in the description", "in the issue")
		text = strings.ReplaceAll(text, "Not mentioned in the description", "Not named in the issue's text")
	}
	return text
}

// commitLine is a commit as the packet shows it.
func commitLine(c repo.Commit) string {
	return fmt.Sprintf("- %s %s %s: %s", c.SHA, c.Date, c.Author, short(c.Subject, 140))
}

// activitySection is what changed lately in the areas, and for a pull request what landed on the
// default branch under it.
func (p *packet) activitySection(ctx context.Context) string {
	if p.r == nil {
		return notAvailable(p.rWhy)
	}
	if len(p.areas) == 0 {
		return "(no areas)"
	}
	since := p.now.AddDate(0, 0, -recentDays)
	var b strings.Builder
	fmt.Fprintf(&b, "On the default branch, the last up to 8 commits per area within %d days:\n", recentDays)
	for _, a := range p.areas[:min(activityAreas, len(p.areas))] {
		commits, err := p.r.PathLog(ctx, p.tip, since, a.pathspecs(), 8)
		switch {
		case err != nil:
			fmt.Fprintf(&b, "%s: %s\n", a.Name, notAvailable(err.Error()))
		case len(commits) == 0:
			fmt.Fprintf(&b, "%s: none in the last %d days\n", a.Name, recentDays)
		default:
			b.WriteString(a.Name + ":\n")
			for _, c := range commits {
				b.WriteString(commitLine(c) + "\n")
			}
		}
	}
	if more := len(p.areas) - activityAreas; more > 0 {
		fmt.Fprintf(&b, "…%d more area%s not shown\n", more, plural(more))
	}
	if p.kind == workflow.PR {
		b.WriteString("\n" + p.landedUnder(ctx))
	}
	return strings.TrimSpace(b.String())
}

// landedUnder lists the commits on the default branch since the pull request branched that touch
// the files it changes: work that landed under it.
func (p *packet) landedUnder(ctx context.Context) string {
	head := ""
	if p.info != nil {
		head = p.info.HeadSHA
	}
	head1 := "Commits on the default branch since this pull request branched, touching its changed files:\n"
	if head == "" {
		return head1 + notAvailable("the pull request's head commit is not known")
	}
	ectx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := p.s.repos.Ensure(ectx, p.ws.ID, p.ws.Name, p.token, head, p.number); err != nil {
		return head1 + notAvailable("the pull request's head is not in the local copy of the code")
	}
	base, err := p.r.MergeBase(ctx, p.tip, head)
	if err != nil {
		return head1 + notAvailable(err.Error())
	}
	paths := make([]string, 0, len(p.files))
	for _, f := range p.files {
		paths = append(paths, f.Path)
	}
	if len(paths) > 200 { // a very large change: its folders say where it reaches
		paths = paths[:0]
		for _, a := range p.areas {
			paths = append(paths, a.pathspecs()...)
		}
	}
	commits, err := p.r.RangeLog(ctx, base, p.tip, paths, 15)
	if err != nil {
		return head1 + notAvailable(err.Error())
	}
	head1 = fmt.Sprintf("Commits on the default branch since this pull request branched (%s), touching its changed files:\n", base[:7])
	if len(commits) == 0 {
		return head1 + "none"
	}
	var rows []string
	for _, c := range commits {
		rows = append(rows, commitLine(c))
	}
	return head1 + lines(rows, 15)
}

// peopleSection is who has committed most in each area lately.
func (p *packet) peopleSection(ctx context.Context) string {
	if p.r == nil {
		return notAvailable(p.rWhy)
	}
	since := p.now.AddDate(0, 0, -peopleDays)
	var rows []string
	for _, a := range p.areas[:min(activityAreas, len(p.areas))] {
		if a.Name == rootArea { // loose files at the top say little about who works where
			continue
		}
		people, err := p.r.Authors(ctx, p.tip, since, a.Name, 3)
		if err != nil || len(people) == 0 {
			continue
		}
		var who []string
		for _, c := range people {
			who = append(who, fmt.Sprintf("%s (%d)", c.Name, c.Commits))
		}
		rows = append(rows, fmt.Sprintf("- %s: %s", a.Name, strings.Join(who, ", ")))
	}
	if len(rows) == 0 {
		return fmt.Sprintf("(no commits in these areas in the last %d days)", peopleDays)
	}
	return fmt.Sprintf("Commits on the default branch in the last %d days:\n%s", peopleDays, strings.Join(rows, "\n"))
}

// charterSection is the charter's lines that mention an area or a key word of the title.
func (p *packet) charterSection() string {
	c, err := p.s.st.Charter(p.ws.ID, 0)
	if err != nil {
		return notAvailable("the charter could not be read: " + err.Error())
	}
	if strings.TrimSpace(c.Body) == "" {
		return notAvailable("the charter is empty")
	}
	var terms []string
	for _, a := range p.areas {
		for _, n := range folderNames(a) {
			terms = append(terms, strings.ToLower(n))
		}
	}
	terms = append(terms, titleTerms(p.title)...)
	var rows []string
	total := 0
	for i, line := range strings.Split(c.Body, "\n") {
		line = strings.TrimSpace(line)
		low := strings.ToLower(line)
		if line == "" || !slices.ContainsFunc(terms, func(t string) bool { return hasWord(low, t) }) {
			continue
		}
		total++
		if len(rows) < 15 {
			rows = append(rows, fmt.Sprintf("- (charter line %d) %s", i+1, short(line, 300)))
		}
	}
	text := strings.Join(rows, "\n")
	switch {
	case total == 0:
		text = "No charter line mentions these areas or the title's key words."
	case total > len(rows):
		text += fmt.Sprintf("\n- …%d more matching lines", total-len(rows))
	}
	return text + "\nThe full charter is available as {charter}."
}
