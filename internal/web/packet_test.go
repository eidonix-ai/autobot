package web

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eidonix-ai/autobot/internal/github"
	"github.com/eidonix-ai/autobot/internal/repo"
	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// packetGH is a GitHub that knows a commit's checks and a pull request's reviews.
type packetGH struct {
	fakeGitHub
	ci    string
	ciErr error
}

func (g packetGH) CIState(context.Context, string, string, string) (string, error) {
	return g.ci, g.ciErr
}

func (packetGH) Reviews(context.Context, string, string, int) ([]github.Review, error) {
	return nil, errors.New("GitHub is down")
}

// The project in the fixture, as git commits (who, when, what), the way GitHub would hold it. The
// pull request, #500, branches from "Add compression" and changes four files in four areas; two
// commits land on main after it branched, one of them on a file the pull request changes.
type packetFixture struct {
	e      *env
	ws     *store.Workspace
	head   string // the pull request's head commit
	values map[string]string
	info   *github.PullRequest
	files  []github.ChangedFile
	now    time.Time
}

func day(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t.Add(12 * time.Hour)
}

// gitAt runs git as a person at a time.
func gitAt(t *testing.T, dir, who, date string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	stamp := date + "T12:00:00Z"
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME="+who, "GIT_AUTHOR_EMAIL="+who+"@example.com", "GIT_COMMITTER_NAME="+who, "GIT_COMMITTER_EMAIL="+who+"@example.com",
		"GIT_AUTHOR_DATE="+stamp, "GIT_COMMITTER_DATE="+stamp, "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newPacketFixture builds the project, its copy of the code, its index of pull requests and issues,
// and its charter. The time is 2026-11-23.
func newPacketFixture(t *testing.T, gh GitHub) *packetFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	e := newEnvWith(t, gh)
	derek := e.user("derek", true)
	ws, err := e.st.CreateWorkspace("o/r", derek, "flare-autobot", "tok-good")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	bare, work := filepath.Join(root, "o", "r.git"), filepath.Join(root, "work")
	os.MkdirAll(bare, 0o755)
	os.MkdirAll(work, 0o755)
	gitAt(t, bare, "T", "2026-01-01", "init", "--bare", "-b", "main")
	gitAt(t, bare, "T", "2026-01-01", "config", "uploadpack.allowFilter", "true")
	gitAt(t, work, "T", "2026-01-01", "init", "-b", "main")
	commit := func(who, date, msg string, files map[string]string) {
		for name, body := range files {
			p := filepath.Join(work, name)
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.WriteFile(p, []byte(body), 0o644)
		}
		gitAt(t, work, who, date, "add", "-A")
		gitAt(t, work, who, date, "commit", "-q", "-m", msg)
	}
	commit("alice", "2026-04-01", "Initial import", map[string]string{"helios/src/compress.rs": "fn compress() {}\n", "helios/src/mod.rs": "mod compress;\n",
		"selene-manager/src/main.rs": "fn main() {}\n", "docs/guide.md": "guide\n", "README.md": "readme\n", "scripts/run.sh": "echo run\n"})
	commit("dave", "2026-07-01", "Early helios work", map[string]string{"helios/src/mod.rs": "mod compress;\nmod early;\n"})
	commit("alice", "2026-10-05", "Add compression to helios (#101)", map[string]string{"helios/src/compress.rs": "fn compress() {}\nfn flush_all() {}\n"})
	gitAt(t, work, "alice", "2026-10-05", "checkout", "-q", "-b", "feature")
	commit("alice", "2026-11-10", "Reuse compression buffers", map[string]string{"helios/src/compress.rs": "fn take_reused() {}\n", "selene-manager/src/main.rs": "fn main() { batch(\"--max-batch\") }\n",
		"docs/guide.md": "guide v2\n", "scripts/run.sh": "echo run2\n"})
	head := gitAt(t, work, "alice", "2026-11-10", "rev-parse", "HEAD")
	gitAt(t, work, "alice", "2026-11-10", "push", "-q", bare, "HEAD:refs/pull/500/head")
	gitAt(t, work, "alice", "2026-11-10", "checkout", "-q", "main")
	commit("bob", "2026-11-15", "Pool streaming buffers (#476)", map[string]string{"helios/src/stream.rs": "fn pool() {}\n"})
	commit("bob", "2026-11-20", "Revert streaming compression (#478)", map[string]string{"helios/src/compress.rs": "fn compress() {}\nfn flush_all() {}\n// reverted\n"})
	commit("carol", "2026-11-21", "Tidy docs (#480)", map[string]string{"docs/other.md": "other\n"})
	gitAt(t, work, "alice", "2026-11-21", "push", "-q", bare, "main")

	e.ui.UseRepos(filepath.Join(e.dir, "test.db"))
	e.ui.repos.BaseURL = "file://" + root
	if err := e.ui.repos.Sync(context.Background(), ws.ID, "o/r", "tok-good", repo.Pulls{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.SaveCharter(ws.ID, 1, "# Flare\nHelios is the compression engine and stays the priority.\nDocs must change with every feature.\nWe do not accept new storage backends.\n", derek); err != nil {
		t.Fatal(err)
	}
	f := &packetFixture{e: e, ws: ws, head: head, now: day("2026-11-23")}
	f.items()
	e.st.IndexSet(ws.ID, "backfill_phase", "done")
	packetNow = func() time.Time { return f.now }
	t.Cleanup(func() { packetNow = time.Now })

	title := "Reuse compression buffers (take_reused) in helios"
	body := "Adds buffer reuse to compress.rs in helios, so uploads are faster. Fixes #470. Follows up #300."
	f.files = []github.ChangedFile{
		{Path: "helios/src/compress.rs", Status: "modified", Additions: 30, Deletions: 5},
		{Path: "selene-manager/src/main.rs", Status: "modified", Additions: 4},
		{Path: "docs/guide.md", Status: "modified", Additions: 10, Deletions: 2},
		{Path: "scripts/run.sh", Status: "modified", Additions: 1},
	}
	f.info = &github.PullRequest{Number: 500, Title: title, Body: body, Author: "alice", State: "open", Association: "MEMBER", HeadSHA: head, Base: "main",
		Labels: []string{"performance"}, Additions: 45, Deletions: 7}
	f.values = map[string]string{"pr:title": title, "pr:body": body, "pr:author": "alice",
		"pr:diff": "+++ b/helios/src/compress.rs\n+fn take_reused(buf: &mut Vec<u8>) {\n-fn old_buffer_path() {}\n+    // a comment\n" +
			"+++ b/selene-manager/src/main.rs\n+    batch(\"--max-batch\");\n+let metric_name = \"compress_buffer_reuse_total\";\n context line fn untouched_name()\n"}
	return f
}

// items fills the index: the pull request itself, what relates to it, and its author's history.
func (f *packetFixture) items() {
	st, ws := f.e.st, f.ws.ID
	save := func(n int, kind, title, body, state, author string, merged bool, created, updated string, labels []string, links []store.ItemLink, files ...string) {
		it := store.Item{Number: n, Kind: kind, Title: title, Body: body, State: state, Merged: merged, Author: author, Labels: labels,
			Created: day(created), Updated: day(updated), Association: "MEMBER", Comments: 3}
		if err := st.SaveItem(ws, it, links); err != nil {
			f.e.t.Fatal(err)
		}
		if len(files) > 0 {
			var fs []store.ItemFile
			for _, p := range files {
				fs = append(fs, store.ItemFile{Path: p, Status: "modified", Additions: 1})
			}
			st.SaveFiles(ws, n, fs)
		}
	}
	// The pull request: open, ready, with links to #470 and #300 and one that points back from #478.
	save(500, "pr", "Reuse compression buffers (take_reused) in helios", "Adds buffer reuse to compress.rs in helios, so uploads are faster. Fixes #470. Follows up #300.",
		"open", "alice", false, "2026-11-10", "2026-11-22", []string{"performance"}, []store.ItemLink{{To: 470, How: "fixes"}, {To: 300, How: "mentions"}},
		"helios/src/compress.rs", "selene-manager/src/main.rs", "docs/guide.md", "scripts/run.sh")
	st.SavePull(ws, 500, store.PullInfo{State: "open", Base: "main", HeadSHA: f.head, Updated: day("2026-11-22")})
	st.SaveComments(ws, []store.ItemComment{
		{Kind: "review", ID: 1, Number: 500, Author: "bob", ReviewState: "APPROVED", Created: day("2026-11-12"), Updated: day("2026-11-12")},
		{Kind: "review", ID: 2, Number: 500, Author: "bob", ReviewState: "COMMENTED", Created: day("2026-11-13"), Updated: day("2026-11-13")},
		{Kind: "review", ID: 3, Number: 500, Author: "carol", ReviewState: "CHANGES_REQUESTED", Created: day("2026-11-14"), Updated: day("2026-11-14")},
		{Kind: "review", ID: 4, Number: 500, Author: "dave", ReviewState: "COMMENTED", Created: day("2026-11-14"), Updated: day("2026-11-14")},
	})
	// The candidates, each reachable by one direction only except the first three.
	save(470, "issue", "Compression is slow on big uploads", "Takes minutes.", "open", "alice", false, "2026-10-20", "2026-11-18", nil, nil)
	st.SaveComments(ws, []store.ItemComment{
		{Kind: "comment", ID: 11, Number: 470, Author: "bob", Body: "me too", Created: day("2026-10-21"), Updated: day("2026-10-21")},
		{Kind: "comment", ID: 12, Number: 470, Author: "carol", Body: "same here", Created: day("2026-10-22"), Updated: day("2026-10-22")},
	})
	save(477, "pr", "Streaming compression for helios", "Introduces a take_reused buffer pool and a --max-batch flag.", "closed", "bob", true,
		"2026-11-01", "2026-11-16", []string{"performance"}, nil, "helios/src/compress.rs")
	save(478, "pr", "Revert streaming compression", "Reverts the pool; see also #500.", "closed", "bob", true, "2026-11-20", "2026-11-20", nil,
		[]store.ItemLink{{To: 500, How: "mentions"}}, "helios/src/compress.rs")
	save(300, "pr", "Quarterly cleanup", "Housekeeping.", "closed", "dave", true, "2026-06-01", "2026-10-01", nil, nil)
	save(310, "issue", "Settler ignores option", "The --max-batch flag is ignored by the settler.", "open", "dave", false, "2026-11-05", "2026-11-10", nil, nil)
	save(320, "issue", "Buffers need docs", "", "open", "dave", false, "2026-11-06", "2026-11-15", nil, nil)
	save(330, "issue", "Unrelated tooling thing", "Nothing to do with it.", "open", "alice", false, "2026-11-07", "2026-11-22", nil, nil)
	save(340, "pr", "Rework build script", "", "open", "dave", false, "2026-11-08", "2026-11-18", nil, nil, "scripts/run.sh")
	save(460, "issue", "Slow settling", "", "closed", "carol", false, "2026-09-01", "2026-11-01", []string{"performance"}, nil)
	// The author's history: a merged pull request in two areas, a closed one, and one too old to count.
	save(101, "pr", "Add compression to helios", "The first compression.", "closed", "alice", true, "2026-10-01", "2026-10-05", nil, nil, "helios/src/compress.rs", "docs/guide.md")
	save(90, "pr", "Try a thing", "", "closed", "alice", false, "2026-09-15", "2026-09-16", nil, nil)
	save(10, "pr", "Ancient", "", "closed", "alice", true, "2026-01-01", "2026-01-02", nil, nil, "other/x.go")
}

// run builds the packet of #500.
func (f *packetFixture) run() string {
	f.e.t.Helper()
	return f.e.ui.orientation(context.Background(), f.ws, "tok-good", workflow.PR, 500, f.values, f.info, f.files)
}

// part is one section of a packet, by its heading.
func part(text, heading string) string {
	_, rest, ok := strings.Cut(text, "### "+heading+"\n")
	if !ok {
		return ""
	}
	rest, _, _ = strings.Cut(rest, "\n### ")
	return rest
}

func mustHave(t *testing.T, where, text string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(text, w) {
			t.Errorf("%s lacks %q:\n%s", where, w, text)
		}
	}
}

func mustNot(t *testing.T, where, text string, not ...string) {
	t.Helper()
	for _, w := range not {
		if strings.Contains(text, w) {
			t.Errorf("%s has %q:\n%s", where, w, text)
		}
	}
}

// The packet opens with its heading, then the sections in order, and says what it is.
func TestPacketShape(t *testing.T) {
	f := newPacketFixture(t, packetGH{ci: "failed"})
	text := f.run()
	if !strings.HasPrefix(text, "## Orientation packet (computed by Autobot: a map of where to look, not conclusions)\n\n### The pull request\n") {
		t.Fatalf("the start: %.200s", text)
	}
	last := -1
	for _, h := range []string{"The pull request", "Areas changed", "Recent activity in those areas", "Possibly related items", "The author", "People in these areas", "The charter"} {
		i := strings.Index(text, "### "+h+"\n")
		if i <= last {
			t.Errorf("section %q is missing or out of order (%d after %d)", h, i, last)
		}
		last = i
	}
	t.Log("\n" + text)
}

// The pull request's own facts: state, age, size, labels, who approved or asked for changes (a
// later comment does not undo a verdict), and the CI of the head.
func TestPacketPullSection(t *testing.T) {
	f := newPacketFixture(t, packetGH{ci: "failed"})
	sec := part(f.run(), "The pull request")
	mustHave(t, "pull section", sec, "State: open, ready for review", "Opened 13 days ago (2026-11-10)", "last updated 1 day ago (2026-11-22)",
		"Base branch: main; head commit: "+f.head[:7], "Size: 4 files, +45/−7", "Labels: performance",
		"approved by bob; changes requested by carol; commented by dave", "CI on the head commit: failed")
	f.info.Draft = true
	mustHave(t, "a draft", part(f.run(), "The pull request"), "open, a draft")
}

// A GitHub that does not answer leaves the CI "unknown" and the packet whole.
func TestPacketCIUnknownWhenGitHubFails(t *testing.T) {
	f := newPacketFixture(t, packetGH{ciErr: errors.New("timeout")})
	mustHave(t, "pull section", part(f.run(), "The pull request"), "CI on the head commit: unknown")
}

// The changed files by area, with a sentence on whether the description mentions each one; the ones it
// never mentions are listed last.
func TestPacketAreas(t *testing.T) {
	f := newPacketFixture(t, packetGH{ci: "passed"})
	sec := part(f.run(), "Areas changed")
	mustHave(t, "areas", sec,
		"- helios/src: 1 file, +30/−5 (compress.rs); in the description: yes",
		"- selene-manager/src: 1 file, +4/−0 (main.rs); in the description: no",
		"- docs: 1 file, +10/−2 (guide.md); in the description: no",
		"- scripts: 1 file, +1/−0 (run.sh); in the description: no",
		"Not mentioned in the description: docs, selene-manager/src, scripts")
	if strings.Index(sec, "helios/src") > strings.Index(sec, "docs:") {
		t.Errorf("the biggest change comes first:\n%s", sec)
	}
	// A file is mentioned by its name with or without extension, in any case, as a whole word.
	f.values["pr:body"], f.info.Body = "Also touches GUIDE and the Selene-Manager.", "Also touches GUIDE and the Selene-Manager."
	sec = part(f.run(), "Areas changed")
	mustHave(t, "mentioned by file stem and folder", sec, "- docs: 1 file, +10/−2 (guide.md); in the description: yes",
		"- selene-manager/src: 1 file, +4/−0 (main.rs); in the description: yes", "Not mentioned in the description: helios/src, scripts")
	f.values["pr:body"], f.info.Body = "The guidelines changed.", "The guidelines changed."
	mustHave(t, "only whole words", part(f.run(), "Areas changed"), "- docs: 1 file, +10/−2 (guide.md); in the description: no")
}

// The recent commits of each area on the default branch (the old one is left out), and the commits
// that landed after the pull request branched on the files it changes.
func TestPacketRecentActivity(t *testing.T) {
	f := newPacketFixture(t, packetGH{ci: "passed"})
	sec := part(f.run(), "Recent activity in those areas")
	area, landed, _ := strings.Cut(sec, "Commits on the default branch since this pull request branched")
	mustHave(t, "area activity", area,
		"helios/src:\n- ", " 2026-11-20 bob: Revert streaming compression (#478)", " 2026-11-15 bob: Pool streaming buffers (#476)", " 2026-10-05 alice: Add compression to helios (#101)",
		"docs:\n- ", " 2026-11-21 carol: Tidy docs (#480)",
		"selene-manager/src: none in the last 60 days", "scripts: none in the last 60 days")
	mustNot(t, "area activity", area, "Early helios work", "Initial import")
	mustHave(t, "landed under the PR", landed, "touching its changed files", " 2026-11-20 bob: Revert streaming compression (#478)")
	mustNot(t, "landed under the PR", landed, "Tidy docs", "Pool streaming buffers", "Add compression to helios")
	// The merge-base is named, so the agent can look at it.
	mustHave(t, "merge-base", landed, "("+gitAtMergeBase(t, f)+")")
}

// gitAtMergeBase is the short id of the commit "Add compression to helios", where the branch began.
func gitAtMergeBase(t *testing.T, f *packetFixture) string {
	t.Helper()
	r := f.e.ui.repos.Repo(f.ws.ID, "")
	tip, _ := r.DefaultTip(context.Background())
	mb, err := r.MergeBase(context.Background(), tip, f.head)
	if err != nil {
		t.Fatal(err)
	}
	return mb[:7]
}

// The areas that get recent activity are at most 8.
func TestPacketActivityCoversEightAreas(t *testing.T) {
	f := newPacketFixture(t, packetGH{ci: "passed"})
	f.files = nil
	for i := 0; i < 10; i++ {
		f.files = append(f.files, github.ChangedFile{Path: fmt.Sprintf("area%02d/code/x.go", i), Additions: 100 - i})
	}
	sec := part(f.run(), "Recent activity in those areas")
	for i := 0; i < 8; i++ {
		mustHave(t, "activity", sec, fmt.Sprintf("area%02d/code: none", i))
	}
	mustNot(t, "activity", sec, "area08/code", "area09/code")
	mustHave(t, "activity", sec, "…2 more areas not shown")
}

// Who commits most in each area, over the last 180 days.
func TestPacketPeople(t *testing.T) {
	f := newPacketFixture(t, packetGH{ci: "passed"})
	sec := part(f.run(), "People in these areas")
	mustHave(t, "people", sec, "- helios/src: bob (2), alice (1), dave (1)", "- docs: carol (1)")
	mustNot(t, "people", sec, "selene-manager", "scripts") // nobody there in 180 days
}

// Each direction finds candidates, and the list is ranked by how many directions found one, then by
// recency.
func TestPacketRelatedItems(t *testing.T) {
	f := newPacketFixture(t, packetGH{ci: "passed"})
	sec := part(f.run(), "Possibly related items")
	line := func(n int) string {
		for _, l := range strings.Split(sec, "\n") {
			if strings.HasPrefix(l, fmt.Sprintf("- #%d ", n)) {
				return l
			}
		}
		return ""
	}
	mustHave(t, "#470 (links, title words, same author)", line(470), "(open, issue) Compression is slow on big uploads", "links (this fixes)", "title words", "same author")
	mustHave(t, "#477 (names, same files, same label, title words)", line(477), "(merged, pr) Streaming compression for helios", "names (", "take_reused", "same files (1 in common)", "same label (performance)", "title words")
	mustHave(t, "#478 (links in, same files)", line(478), "links (it mentions this)", "same files")
	mustHave(t, "#300 (links out only)", line(300), "(merged, pr) Quarterly cleanup — found by: links (this mentions)")
	mustHave(t, "#310 (names only)", line(310), "found by: names (--max-batch)")
	mustHave(t, "#320 (title words only)", line(320), "found by: title words")
	mustHave(t, "#330 (author only)", line(330), "found by: same author")
	mustHave(t, "#340 (files only)", line(340), "found by: same files (1 in common)")
	mustHave(t, "#460 (label only)", line(460), "found by: same label (performance)")
	if line(500) != "" {
		t.Errorf("the item is its own candidate: %s", line(500))
	}
	// Most directions first; then by recency among the equals (updated on the 22nd, 18th, 15th, 10th, 1st of November, and October 1st).
	pos := func(n int) int { return strings.Index(sec, fmt.Sprintf("- #%d ", n)) }
	for _, n := range []int{477, 478, 470, 101} { // three or four directions
		if pos(n) < 0 || pos(n) > pos(330) {
			t.Errorf("#%d has more directions than #330 and must come before it:\n%s", n, sec)
		}
	}
	if !(pos(477) < pos(470) && pos(470) < pos(101) && pos(478) < pos(470)) { // four directions by recency, then three
		t.Errorf("the multi-direction order:\n%s", sec)
	}
	for i, n := range []int{330, 340, 320, 310, 460, 300} {
		if i > 0 && pos(n) < pos([]int{330, 340, 320, 310, 460, 300}[i-1]) {
			t.Errorf("#%d should come after the more recent one before it:\n%s", n, sec)
		}
	}
}

// A long list is cut at 15 with a note.
func TestPacketRelatedIsCut(t *testing.T) {
	f := newPacketFixture(t, packetGH{ci: "passed"})
	for i := 0; i < 20; i++ {
		f.e.st.SaveItem(f.ws.ID, store.Item{Number: 600 + i, Kind: "issue", Title: fmt.Sprintf("Perf %d", i), State: "open", Author: "zed", Labels: []string{"performance"},
			Created: day("2026-11-01"), Updated: day("2026-11-02")}, nil)
	}
	sec := part(f.run(), "Possibly related items")
	if n := strings.Count(sec, "\n- #"); n != 15 {
		t.Errorf("%d candidates shown:\n%s", n, sec)
	}
	mustHave(t, "the cut", sec, "- …")
	mustHave(t, "the cut", sec, " more")
}

// A term that finds a crowd of items says nothing and is not used.
func TestPacketCommonTermsAreDropped(t *testing.T) {
	f := newPacketFixture(t, packetGH{ci: "passed"})
	for i := 0; i < 25; i++ {
		f.e.st.SaveItem(f.ws.ID, store.Item{Number: 700 + i, Kind: "issue", Title: fmt.Sprintf("Noise %d", i), Body: "the settle_everything_now call", State: "open", Author: "zed",
			Created: day("2026-11-01"), Updated: day("2026-11-02")}, nil)
	}
	f.values["pr:diff"] += "+fn settle_everything_now() {}\n"
	if sec := part(f.run(), "Possibly related items"); strings.Contains(sec, "settle_everything_now") || strings.Contains(sec, "#700") {
		t.Errorf("a common term was used:\n%s", sec)
	}
}

// The search terms from the diff: names after fn, func, def, class, struct and enum; flags; metric-like
// names; the changed files' base names; longest first, at most 30, nothing from context or header lines.
func TestDiffTerms(t *testing.T) {
	diff := "+++ b/x.go\n--- a/x.go\n" +
		"+fn take_reused(a: u8) {\n-func (r *Repo) ResolveRef(x int) {\n+def parse_header(self):\n+class WidgetFactory:\n+pub struct ByteSpan {\n-enum LinkKind {\n+type Mirrors struct {\n" +
		"+    --max-batch and --no\n+let compress_buffer_reuse_total = 1;\n+let short_name = 2;\n context fn ignored_context()\n"
	files := []github.ChangedFile{{Path: "helios/src/compress.rs"}, {Path: "a.go"}, {Path: "docs/guide.md"}}
	got := diffTerms(diff, files)
	want := []string{"compress_buffer_reuse_total", "WidgetFactory", "parse_header", "take_reused", "ResolveRef", "--max-batch", "compress.rs", "ByteSpan", "LinkKind", "Mirrors", "guide.md"}
	have := map[string]bool{}
	for _, g := range got {
		have[g] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("no term %q in %v", w, got)
		}
	}
	for _, not := range []string{"ignored_context", "short_name", "--no", "a.go", "x.go", "b/x.go"} {
		if have[not] {
			t.Errorf("term %q should not be there: %v", not, got)
		}
	}
	for i := 1; i < len(got); i++ {
		if len(got[i]) > len(got[i-1]) {
			t.Errorf("longest first: %v", got)
			break
		}
	}
	var many string
	for i := 0; i < 50; i++ {
		many += fmt.Sprintf("+fn name_number_%02d_here() {}\n", i)
	}
	if got := diffTerms(many, nil); len(got) != 30 {
		t.Errorf("%d terms, want the cap of 30", len(got))
	}
}

// The author: standing, counts of the last 180 days (not the item itself, nor older ones), their ten most
// recent items, and the areas their merged pull requests touched.
func TestPacketAuthor(t *testing.T) {
	f := newPacketFixture(t, packetGH{ci: "passed"})
	sec := part(f.run(), "The author")
	mustHave(t, "author", sec, "alice, GitHub's association with the repository: MEMBER",
		"in the last 180 days: 1 merged, 1 closed, 2 open",
		"#470 (open, issue) Compression is slow on big uploads, 2026-10-20", "#101 (merged, pr) Add compression to helios, 2026-10-01", "#90 (closed, pr) Try a thing",
		"Areas their merged pull requests touched: docs (1), helios/src (1)")
	mustNot(t, "author", sec, "#500", "#10 ", "Ancient")
	// Ten at most.
	for i := 0; i < 14; i++ {
		f.e.st.SaveItem(f.ws.ID, store.Item{Number: 800 + i, Kind: "issue", Title: fmt.Sprintf("Mine %d", i), State: "open", Author: "alice", Created: day("2026-11-02"), Updated: day("2026-11-02")}, nil)
	}
	sec = part(f.run(), "The author")
	if n := strings.Count(sec, "\n  - #"); n != 10 {
		t.Errorf("%d items listed:\n%s", n, sec)
	}
	mustHave(t, "author", sec, "…8 more", "1 merged, 1 closed, 16 open")
}

// The charter lines that mention an area's folder or a key word of the title, with their line
// numbers, at most 15.
func TestPacketCharter(t *testing.T) {
	f := newPacketFixture(t, packetGH{ci: "passed"})
	sec := part(f.run(), "The charter")
	mustHave(t, "charter", sec, "- (charter line 2) Helios is the compression engine and stays the priority.", "- (charter line 3) Docs must change with every feature.",
		"The full charter is available as {charter}.")
	mustNot(t, "charter", sec, "storage backends")
	var long strings.Builder
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&long, "Helios line %d.\n", i)
	}
	f.e.st.SaveCharter(f.ws.ID, 2, long.String(), f.e.user("erin", true))
	sec = part(f.run(), "The charter")
	if n := strings.Count(sec, "- (charter line"); n != 15 {
		t.Errorf("%d charter lines:\n%s", n, sec)
	}
	mustHave(t, "charter", sec, "…5 more matching lines")
}

// A section is cut at its limit and the whole packet at its own, each at a line end with a note.
func TestPacketSizeCaps(t *testing.T) {
	if got := capText("short", 100); got != "short" {
		t.Fatalf("%q", got)
	}
	text := strings.Repeat("a line of some length\n", 100)
	got := capText(text, 500)
	if len(got) > 500 || !strings.HasSuffix(got, "…more (cut at 500 characters)") || !strings.Contains(got, "a line of some length\n…more") {
		t.Fatalf("capText: %d %q", len(got), got[len(got)-80:])
	}
	if got := capText(strings.Repeat("é", 400), 301); len(got) > 301 || strings.Contains(got, "\ufffd") {
		t.Fatalf("a cut in the middle of a character: %q", got)
	}
	// The same through a packet: the charter section is cut, and the whole stays under its limit.
	f := newPacketFixture(t, packetGH{ci: "passed"})
	var charter strings.Builder
	for i := 0; i < 15; i++ {
		fmt.Fprintf(&charter, "Helios %d %s\n", i, strings.Repeat("compression ", 25))
	}
	f.e.st.SaveCharter(f.ws.ID, 2, charter.String(), f.e.user("erin", true))
	text = f.run()
	sec := part(text, "The charter")
	if len(sec) > sectionLimit+10 || !strings.Contains(sec, "…more (cut at 3000 characters)") {
		t.Errorf("the charter section is %d characters:\n%s", len(sec), sec[max(0, len(sec)-200):])
	}
	old := packetLimit
	packetLimit = 2500
	defer func() { packetLimit = old }()
	text = f.run()
	if len(text) > 2500 || !strings.HasSuffix(text, "…more (cut at 2500 characters)") || !strings.HasPrefix(text, packetHeading) {
		t.Errorf("the whole packet is %d characters, ends %q", len(text), text[max(0, len(text)-60):])
	}
}

// With no copy of the code, no index and no charter, the packet still builds: each section says what it
// lacks.
func TestPacketWithoutSources(t *testing.T) {
	e := newEnv(t)
	derek := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", derek, "flare-autobot", "tok-good")
	info := &github.PullRequest{Number: 7, State: "open", Author: "alice", Association: "NONE", HeadSHA: "0123456789abcdef0123456789abcdef01234567", Base: "main"}
	files := []github.ChangedFile{{Path: "a/b/c.go", Additions: 2}}
	values := map[string]string{"pr:title": "Retry uploads", "pr:body": "Retries.", "pr:author": "alice"}
	text := e.ui.orientation(context.Background(), ws, "tok-good", workflow.PR, 7, values, info, files)
	if !strings.HasPrefix(text, packetHeading) {
		t.Fatalf("no packet: %q", text)
	}
	mustHave(t, "the packet", text, "### The pull request\n- State: open, ready for review", "Opened and last updated: (not available: the local copy of pull requests and issues is not ready yet)",
		"Size: 1 file, +2/−0", "### Areas changed\n- a/b: 1 file, +2/−0 (c.go); in the description: no")
	for h, why := range map[string]string{"Recent activity in those areas": "the local copy of the code is not ready yet",
		"Possibly related items": "the local copy of pull requests and issues is not ready yet", "People in these areas": "the local copy of the code is not ready yet"} {
		mustHave(t, h, part(text, h), "(not available: "+why+")")
	}
	mustHave(t, "the author", part(text, "The author"), "alice, GitHub's association with the repository: NONE", "History: (not available: ")
	// A new workspace has the template charter; an emptied one is said to be empty.
	mustHave(t, "the template charter", part(text, "The charter"), "The full charter is available as {charter}.")
	e.st.SaveCharter(ws.ID, 1, "\n", derek)
	mustHave(t, "an empty charter", part(e.ui.orientation(context.Background(), ws, "tok-good", workflow.PR, 7, values, info, files), "The charter"), "(not available: the charter is empty)")
	// An item the copy does not have, in a workspace whose copy is ready.
	e.st.IndexSet(ws.ID, "backfill_phase", "done")
	text = e.ui.orientation(context.Background(), ws, "tok-good", workflow.PR, 7, values, info, files)
	mustHave(t, "an item not in the copy", text, "(not available: #7 is not in the local copy of pull requests and issues)", "No candidates found")
	// And no item at all.
	if text := e.ui.orientation(context.Background(), ws, "tok-good", workflow.PR, 0, values, nil, nil); !strings.Contains(text, "(not available: no pull request or issue was given)") {
		t.Errorf("no item: %q", text)
	}
}

// A packet that panics must not take the run with it.
func TestPacketNeverPanics(t *testing.T) {
	e := newEnv(t)
	ws, _ := e.st.CreateWorkspace("o/r", e.user("derek", true), "flare-autobot", "tok-good")
	e.ui.st = nil // every read now panics
	text := e.ui.orientation(context.Background(), ws, "t", workflow.PR, 7, map[string]string{}, nil, nil)
	mustHave(t, "the packet", text, packetHeading, "(not available: ")
}

// An issue's packet has its own facts, related items, author and charter, and the areas and people
// only when its text names paths that exist in the code.
func TestPacketForAnIssue(t *testing.T) {
	f := newPacketFixture(t, packetGH{ci: "passed"})
	run := func(body string) string {
		values := map[string]string{"issue:title": "Compression is slow", "issue:body": body, "issue:author": "alice"}
		return f.e.ui.orientation(context.Background(), f.ws, "tok-good", workflow.Issue, 470, values, nil, nil)
	}
	text := run("Uploads are slow. It happens in `take_reused` and the --max-batch flag.")
	mustHave(t, "issue", part(text, "The issue"), "State: open", "Opened 34 days ago (2026-10-20)", "Comments: 2", "Reported by: alice")
	mustHave(t, "issue related", part(text, "Possibly related items"), "#477 (merged, pr)", "names (", "#310 (open, issue)")
	mustHave(t, "issue author", part(text, "The author"), "alice, GitHub's association", "merged")
	mustHave(t, "issue charter", part(text, "The charter"), "Helios is the compression engine")
	mustNot(t, "no paths named", text, "### Recent activity", "### People in these areas", "### Areas changed", "### Repository paths")
	// Paths that exist bring the activity and people sections; one that does not is ignored.
	text = run("It is in helios/src/compress.rs and not in nowhere/real.go.")
	mustHave(t, "paths named", part(text, "Repository paths the issue names"), "- helios/src: 1 file (compress.rs)")
	mustNot(t, "paths named", part(text, "Repository paths the issue names"), "nowhere")
	mustHave(t, "activity", part(text, "Recent activity in those areas"), "Revert streaming compression (#478)")
	mustNot(t, "activity", part(text, "Recent activity in those areas"), "since this pull request branched")
	mustHave(t, "people", part(text, "People in these areas"), "bob (2)")
	// A folder named counts as an area of itself.
	mustHave(t, "folder", part(run("Look at docs/ please, no: docs/guide.md and helios/src/."), "Repository paths the issue names"), "- docs: 1 file (guide.md)", "- helios/src: 1 file")
}
