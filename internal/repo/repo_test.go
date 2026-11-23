package repo

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A source repository stands in for GitHub: a bare repository under <dir>/owner/repo.git, reached
// by a file:// address, and a work tree to commit in. No network is used.
type source struct {
	t       *testing.T
	base    string // file:// address of the folder holding owner/repo.git
	bare    string
	work    string
	realGit string
	prs     []string // the heads of pull requests 1, 2 and 3
}

func (s *source) git(dir string, args ...string) string {
	s.t.Helper()
	cmd := exec.Command(s.realGit, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		s.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newSource(t *testing.T) *source {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	s := &source{t: t, realGit: real, base: "file://" + root, bare: filepath.Join(root, "owner", "repo.git"), work: filepath.Join(root, "work")}
	os.MkdirAll(s.bare, 0o755)
	os.MkdirAll(s.work, 0o755)
	s.git(s.bare, "init", "--bare", "-b", "main")
	s.git(s.bare, "config", "uploadpack.allowFilter", "true")
	s.git(s.bare, "config", "uploadpack.allowAnySHA1InWant", "true")
	s.git(s.work, "init", "-b", "main")
	s.git(s.work, "remote", "add", "origin", s.bare)
	return s
}

// commit writes files (name -> content) and commits them, returning the commit id.
func (s *source) commit(msg string, files map[string]string) string {
	s.t.Helper()
	for name, content := range files {
		p := filepath.Join(s.work, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			s.t.Fatal(err)
		}
	}
	s.git(s.work, "add", "-A")
	s.git(s.work, "commit", "-q", "-m", msg)
	return s.git(s.work, "rev-parse", "HEAD")
}

func (s *source) push(refspec string) {
	s.t.Helper()
	s.git(s.work, "push", "-q", "-f", "origin", refspec)
}

// seed makes the usual contents: a few files, one over 1 MB, one binary, and a pull request.
func (s *source) seed() (first, second string) {
	big := strings.Repeat("0123456789abcdef\n", 80_000) // 1.3 MB
	first = s.commit("first commit", map[string]string{
		"README.md": "# Demo\n", "main.go": "package main\n\nfunc Hello() {}\n", "docs/guide.md": "the --version flag prints it\n",
		"docs/big.txt": big + "needle in the big file\n", "bin.dat": "a\x00b"})
	s.push("main")
	second = s.commit("add util: fix the Hello bug", map[string]string{"util/util.go": "package util\n"})
	s.push("main")
	// Pull requests 1 and 2 are open, 3 is closed (the tests say which are open); and a branch that is not the default.
	for n, name := range []string{"feature", "second-pr", "closed-pr"} {
		s.git(s.work, "checkout", "-q", "-b", name, first)
		s.prs = append(s.prs, s.commit(name+" change", map[string]string{name + ".txt": name + "\n"}))
		s.push("HEAD:refs/pull/" + strconv.Itoa(n+1) + "/head")
	}
	s.git(s.work, "checkout", "-q", "main")
	s.push("main:refs/heads/extra")
	return first, second
}

func newMirrors(t *testing.T, s *source) *Mirrors {
	m := New(t.TempDir())
	m.BaseURL = s.base
	return m
}

// syncOK syncs the default branch only, not knowing which pull requests are open.
func syncOK(t *testing.T, m *Mirrors, token string) {
	t.Helper()
	if err := m.Sync(context.Background(), 1, "owner/repo", token, Pulls{}); err != nil {
		t.Fatal(err)
	}
}

// syncPulls syncs knowing which pull requests are open.
func syncPulls(t *testing.T, m *Mirrors, open ...int) {
	t.Helper()
	if err := m.Sync(context.Background(), 1, "owner/repo", "", Pulls{Open: open, Known: true}); err != nil {
		t.Fatal(err)
	}
}

func TestCloneThenFetchOnlyAdds(t *testing.T) {
	s := newSource(t)
	first, second := s.seed()
	m := newMirrors(t, s)
	ctx := context.Background()
	if m.Ready(1) {
		t.Fatal("ready before the clone")
	}
	syncPulls(t, m, 1, 2)
	r := m.Repo(1, "")
	if !m.Ready(1) {
		t.Fatal("not ready after the clone")
	}
	for ref, want := range map[string]string{"main": second, "": second, "HEAD": second, first: first, strings.ToUpper(first): first} {
		if got, err := r.Resolve(ctx, ref); err != nil || got != want {
			t.Errorf("Resolve(%q) = %q, %v; want %s", ref, got, err, want)
		}
	}
	pr, err := r.Resolve(ctx, "pr/1")
	if err != nil || pr == first || pr == second {
		t.Fatalf("pr/1 = %q, %v", pr, err)
	}
	// The file over 1 MB was left behind by the clone, and is fetched when it is read.
	missing := s.git(m.path(1), "rev-list", "--objects", "--missing=print", "--all")
	if !strings.Contains(missing, "?") {
		t.Errorf("the large file was downloaded by the clone:\n%s", missing)
	}
	if text, err := r.ReadFile(ctx, second, "docs/big.txt"); err != nil || !strings.Contains(text, "needle in the big file") {
		t.Errorf("reading the large file: %v", err)
	}

	// New work on the source, and a pull request that is gone: the fetch brings only the new objects
	// and drops the old ref, and everything held before is still there.
	before := count(t, m)
	third := s.commit("third", map[string]string{"third.txt": "3\n", "deep/er/file.txt": "x\n"})
	s.push("main")
	newObjects := len(strings.Split(s.git(s.work, "rev-list", "--objects", second+".."+third), "\n"))
	syncPulls(t, m, 2) // pull request 1 has closed
	if len(strings.Split(s.git(s.work, "rev-list", "--objects", second+".."+third), "\n")) != newObjects {
		t.Fatal("test setup")
	}
	if got := count(t, m) - before; got != newObjects {
		t.Errorf("the fetch added %d objects; the source has %d new ones", got, newObjects)
	}
	if got, err := r.Resolve(ctx, "main"); err != nil || got != third {
		t.Errorf("main = %q, %v", got, err)
	}
	if got, err := r.Resolve(ctx, first); err != nil || got != first {
		t.Errorf("an old commit is gone: %v", err)
	}
	if _, err := r.Resolve(ctx, "pr/1"); err == nil {
		t.Error("the ref of a pull request that closed was kept")
	}
	if st := m.Status(1); st.Err != "" || st.Last.IsZero() || st.Size == 0 {
		t.Errorf("status %+v", st)
	}
}

// count is the number of objects the mirror holds.
func count(t *testing.T, m *Mirrors) int {
	t.Helper()
	out, err := exec.Command("git", "--git-dir="+m.path(1), "cat-file", "--batch-all-objects", "--batch-check").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(out), "\n")
}

func TestResolveRefusesAnythingElse(t *testing.T) {
	s := newSource(t)
	first, _ := s.seed()
	s.git(s.work, "push", "-q", "origin", "feature") // a branch that exists, but is not the default
	m := newMirrors(t, s)
	syncPulls(t, m, 1)
	r := m.Repo(1, "")
	ctx := context.Background()
	for _, ref := range []string{"-x", "--upload-pack=touch /tmp/pwned", "-", "feature", "refs/heads/main", "refs/pull/1/head", "main..feature", "main~1", "HEAD~1",
		"pr/0", "pr/01", "pr/1/../2", "pr/2", "pr/9", "pr/-1", "pr/", strings.Repeat("0", 40), first[:12], first + "0", "tags/v1", "origin/main", "main\n--stat"} {
		if got, err := r.Resolve(ctx, ref); err == nil {
			t.Errorf("Resolve(%q) = %s; it should be refused", ref, got)
		}
	}
}

func TestReadFileRefusesPathsOutsideTheRepository(t *testing.T) {
	s := newSource(t)
	_, second := s.seed()
	m := newMirrors(t, s)
	syncOK(t, m, "")
	r := m.Repo(1, "")
	ctx := context.Background()
	if text, err := r.ReadFile(ctx, second, "docs/guide.md"); err != nil || !strings.Contains(text, "--version") {
		t.Fatalf("a plain path: %q, %v", text, err)
	}
	for _, p := range []string{"../etc/passwd", "docs/../../x", "/etc/passwd", "", "./README.md", "docs//guide.md", "docs/./guide.md", "a\x00b", ".."} {
		_, err := r.ReadFile(ctx, second, p)
		if err == nil || !strings.Contains(err.Error(), "path not allowed") {
			t.Errorf("ReadFile(%q): %v; the path should be refused as not allowed", p, err)
		}
	}
	for _, p := range []string{"docs", "nothing.txt", "bin.dat"} { // a folder, a missing file, a binary file
		if text, err := r.ReadFile(ctx, second, p); err == nil {
			t.Errorf("ReadFile(%q) = %q", p, text)
		}
	}
	if _, err := r.ReadFile(ctx, "main", "README.md"); err == nil { // reads take a resolved commit id
		t.Error("a read at a name rather than a commit id")
	}
}

func TestGrepTakesAnyPatternAsAPattern(t *testing.T) {
	s := newSource(t)
	_, second := s.seed()
	m := newMirrors(t, s)
	syncOK(t, m, "")
	r := m.Repo(1, "")
	ctx := context.Background()
	for _, pat := range []string{"--version", "-e", "--open-files-in-pager=touch /tmp/pwned", "-h"} {
		out, err := r.Grep(ctx, second, pat, "", false)
		if err != nil {
			t.Fatalf("Grep(%q): %v", pat, err)
		}
		hit := strings.Contains(out, "docs/guide.md:1:the --version flag")
		if (pat == "--version") != hit || strings.Contains(out, "git version") {
			t.Errorf("Grep(%q) = %q", pat, out)
		}
	}
	out, _ := r.Grep(ctx, second, "hello", "", true)
	if !strings.Contains(out, "main.go:3:func Hello") || strings.Contains(out, second) {
		t.Errorf("ignore case, and no commit id in the lines: %q", out)
	}
	if out, _ := r.Grep(ctx, second, "Hello", "docs/*", false); !strings.Contains(out, "No matches") {
		t.Errorf("the path filter: %q", out)
	}
	if out, _ := r.Grep(ctx, second, "needle", "", false); strings.Contains(out, "needle") {
		t.Errorf("searched a large file git left behind: %q", out)
	}
	for _, spec := range []string{":(top)*", "-x", "../x", "docs/../../x", ":/"} {
		if _, err := r.Grep(ctx, second, "x", spec, false); err == nil {
			t.Errorf("path filter %q accepted", spec)
		}
	}
	if _, err := r.Grep(ctx, second, "bad(", "", false); err == nil {
		t.Error("an invalid expression gave no error")
	}
	// Many matches are cut at the cap.
	c := s.commit("many", map[string]string{"many.txt": strings.Repeat("match\n", 500)})
	s.push("main")
	syncOK(t, m, "")
	out, err := r.Grep(ctx, c, "match", "many.txt", false)
	if err != nil || strings.Count(out, "many.txt:") != MaxGrepLines || !strings.Contains(out, "narrow") {
		t.Errorf("cap: %d lines, %v", strings.Count(out, "many.txt:"), err)
	}
}

func TestListLogDiffOverview(t *testing.T) {
	s := newSource(t)
	first, second := s.seed()
	m := newMirrors(t, s)
	syncPulls(t, m, 1)
	r := m.Repo(1, "")
	ctx := context.Background()
	files, err := r.ListFiles(ctx, second)
	if err != nil || strings.Join(files, " ") != "README.md bin.dat docs/big.txt docs/guide.md main.go util/util.go" {
		t.Errorf("files %v, %v", files, err)
	}
	if out, err := r.Log(ctx, second, "hello", ""); err != nil || !strings.Contains(out, "add util: fix the Hello bug") || strings.Contains(out, "first commit") {
		t.Errorf("log by text: %q, %v", out, err)
	}
	if out, err := r.Log(ctx, second, "--all", ""); err != nil || !strings.Contains(out, "No commits") {
		t.Errorf("log by an option-like text: %q, %v", out, err)
	}
	if out, err := r.Log(ctx, second, "", "util"); err != nil || strings.Count(out, "\n") != 1 {
		t.Errorf("log by path: %q, %v", out, err)
	}
	pr, _ := r.Resolve(ctx, "pr/1")
	if out, err := r.Diff(ctx, second, pr); err != nil || !strings.Contains(out, "+feature") || strings.Contains(out, "util.go") {
		t.Errorf("diff: %q, %v", out, err)
	}
	if _, err := r.Diff(ctx, "main", pr); err == nil {
		t.Error("a diff against a name")
	}
	if got, err := r.Branch(ctx, "main"); err != nil || got != second {
		t.Errorf("branch: %q, %v", got, err)
	}
	if _, err := r.Branch(ctx, "--x"); err == nil {
		t.Error("branch accepted an option")
	}
	out, err := r.Overview(ctx, first)
	if err != nil || !strings.Contains(out, "docs/ (2)") || !strings.Contains(out, ".md Markdown (2)") {
		t.Errorf("overview: %q, %v", out, err)
	}
}

// wrapper makes a git that records every command line and environment, then runs the real one.
func wrapper(t *testing.T, real string) (path, args, env string) {
	dir := t.TempDir()
	path, args, env = filepath.Join(dir, "git"), filepath.Join(dir, "args.log"), filepath.Join(dir, "env.log")
	// A file named "fail" beside the logs makes git fail when its text is part of the command line.
	fail := filepath.Join(dir, "fail")
	script := "#!/bin/sh\necho \"$@\" >> " + args + "\nenv >> " + env + "\n" +
		"if [ -s " + fail + " ]; then case \"$*\" in *\"$(cat " + fail + ")\"*) echo 'boom' >&2; exit 1;; esac; fi\n" +
		"exec " + real + " \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return
}

// The workspace's token is for git's environment only: on no command line, and in no file the
// mirror holds, after a clone and after a fetch.
func TestTokenStaysOutOfConfigAndCommandLines(t *testing.T) {
	s := newSource(t)
	s.seed()
	m := newMirrors(t, s)
	gitPath, argsLog, envLog := wrapper(t, s.realGit)
	m.Git = gitPath
	const token = "ghp_SecretTokenValue123"
	header := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	syncOK(t, m, token)
	s.commit("more", map[string]string{"more.txt": "m\n"})
	s.push("main")
	syncOK(t, m, token)
	if _, err := m.Repo(1, token).ReadFile(context.Background(), s.git(s.work, "rev-parse", "HEAD"), "more.txt"); err != nil {
		t.Fatal(err)
	}

	args, _ := os.ReadFile(argsLog)
	env, _ := os.ReadFile(envLog)
	if !strings.Contains(string(args), "fetch") || !strings.Contains(string(args), "clone") {
		t.Fatalf("git was not run as expected:\n%s", args)
	}
	if strings.Contains(string(args), token) || strings.Contains(string(args), header) {
		t.Errorf("the token is on a command line:\n%s", args)
	}
	if !strings.Contains(string(env), "AUTHORIZATION: basic "+header) || !strings.Contains(string(env), "GIT_TERMINAL_PROMPT=0") ||
		!strings.Contains(string(env), "GIT_CONFIG_NOSYSTEM=1") {
		t.Errorf("git did not get its token and settings through the environment:\n%s", env)
	}
	filepath.WalkDir(m.dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if b, _ := os.ReadFile(p); strings.Contains(string(b), token) || strings.Contains(string(b), header) {
			t.Errorf("the token is in %s", p)
		}
		return nil
	})
	if b, _ := os.ReadFile(filepath.Join(m.path(1), "config")); !strings.Contains(string(b), "promisor") {
		t.Errorf("the mirror's config is not as expected:\n%s", b)
	}
	// Without a token there is no header at all.
	if strings.Contains(strings.Join(m.env(""), "\n"), "extraheader") {
		t.Error("an empty token still makes a header")
	}
	// A failure never shows the token.
	m.BaseURL = "file:///nonexistent"
	err := m.Sync(context.Background(), 2, "owner/repo", token, Pulls{})
	if err == nil || strings.Contains(err.Error(), token) {
		t.Errorf("error %v", err)
	}
}

func TestEnsureFetchesOnlyWhenTheCommitIsMissing(t *testing.T) {
	s := newSource(t)
	s.seed()
	m := newMirrors(t, s)
	gitPath, argsLog, _ := wrapper(t, s.realGit)
	m.Git = gitPath
	syncOK(t, m, "")
	ctx := context.Background()
	head := s.git(s.work, "rev-parse", "HEAD")
	os.Remove(argsLog)
	if err := m.Ensure(ctx, 1, "owner/repo", "", head, 0); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(argsLog); strings.Contains(string(b), "fetch") {
		t.Errorf("fetched for a commit the mirror has:\n%s", b)
	}
	fresh := s.commit("fresh", map[string]string{"fresh.txt": "f\n"})
	s.push("main")
	if err := m.Ensure(ctx, 1, "owner/repo", "", fresh, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Repo(1, "").Resolve(ctx, fresh); err != nil {
		t.Error(err)
	}
	if err := m.Ensure(ctx, 1, "owner/repo", "", strings.Repeat("a", 40), 0); err == nil {
		t.Error("a commit that does not exist was ensured")
	}
}

func TestFailuresBackOffAndSuccessRecovers(t *testing.T) {
	s := newSource(t)
	s.seed()
	m := newMirrors(t, s)
	ctx := context.Background()
	m.BaseURL = "file:///nonexistent"
	var waits []time.Duration
	for i := 0; i < 8; i++ {
		before := time.Now()
		if err := m.Sync(ctx, 1, "owner/repo", "", Pulls{}); err == nil {
			t.Fatal("a clone of nothing worked")
		}
		st := m.state(1)
		waits = append(waits, st.next.Sub(before).Round(time.Minute))
	}
	want := []time.Duration{2, 4, 8, 16, 32, 60, 60, 60}
	for i, w := range waits {
		if w != want[i]*time.Minute {
			t.Fatalf("waits %v; want %v minutes", waits, want)
		}
	}
	if m.Due(1, time.Now()) || !m.Due(1, time.Now().Add(61*time.Minute)) {
		t.Error("Due ignores the wait")
	}
	if m.Ready(1) {
		t.Error("a failed clone left a mirror")
	}
	if st := m.Status(1); st.Err == "" || st.Ready {
		t.Errorf("status %+v", st)
	}
	m.BaseURL = s.base
	syncOK(t, m, "")
	if st := m.Status(1); st.Err != "" || !st.Ready || m.state(1).fails != 0 || !m.Due(1, time.Now().Add(Every+time.Second)) || m.Due(1, time.Now()) {
		t.Errorf("after recovering: %+v", st)
	}
}

func TestPruneRemovesOnlyMirrorsOfGoneWorkspaces(t *testing.T) {
	m := New(t.TempDir())
	os.MkdirAll(filepath.Join(m.dir, "1.git"), 0o700)
	os.MkdirAll(filepath.Join(m.dir, "2.git"), 0o700)
	os.MkdirAll(filepath.Join(m.dir, "abc.git"), 0o700)
	os.MkdirAll(filepath.Join(m.dir, "02.git"), 0o700)
	os.MkdirAll(filepath.Join(m.dir, "1.git.new"), 0o700)
	os.MkdirAll(filepath.Join(m.dir, "4.git.new"), 0o700)
	os.MkdirAll(filepath.Join(m.dir, "4.git.old"), 0o700)
	os.WriteFile(filepath.Join(m.dir, "3.git"), []byte("a file"), 0o600)
	os.WriteFile(filepath.Join(m.dir, "notes.txt"), []byte("mine"), 0o600)
	m.Prune([]int64{1})
	for name, want := range map[string]bool{"1.git": true, "1.git.new": true, "4.git.new": false, "4.git.old": false, "2.git": false, "abc.git": true, "02.git": true, "3.git": true, "notes.txt": true} {
		if _, err := os.Stat(filepath.Join(m.dir, name)); (err == nil) != want {
			t.Errorf("%s present = %v, want %v", name, err == nil, want)
		}
	}
}

// refsOf lists the refs of the mirror.
func refsOf(t *testing.T, m *Mirrors) string {
	t.Helper()
	out, err := exec.Command("git", "--git-dir="+m.path(1), "for-each-ref", "--format=%(refname)").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(strings.Fields(string(out)), " ")
}

// objects lists every object the mirror holds.
func objects(t *testing.T, m *Mirrors) string {
	t.Helper()
	out, err := exec.Command("git", "--git-dir="+m.path(1), "cat-file", "--batch-all-objects", "--batch-check").Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// The mirror is lean: the default branch and the open pull requests' heads, nothing else. The
// fetch asks for exactly those refs, a pull request that closes loses its ref, and when GitHub
// could not say which are open, the mirror keeps what it has and asks for no pull request.
func TestMirrorHoldsOnlyTheDefaultBranchAndOpenPulls(t *testing.T) {
	s := newSource(t)
	s.seed()
	m := newMirrors(t, s)
	gitPath, argsLog, _ := wrapper(t, s.realGit)
	m.Git = gitPath
	fetches := func() string { // the fetch command lines so far
		b, _ := os.ReadFile(argsLog)
		var out []string
		for _, l := range strings.Split(string(b), "\n") {
			if strings.Contains(l, " fetch ") {
				out = append(out, l)
			}
		}
		os.Remove(argsLog)
		return strings.Join(out, "\n")
	}

	syncPulls(t, m, 1, 2)
	if got := refsOf(t, m); got != "refs/heads/main refs/pull/1/head refs/pull/2/head" {
		t.Errorf("after the first sync: %s", got)
	}
	if strings.Contains(objects(t, m), s.prs[2]) {
		t.Error("the closed pull request's commit was downloaded")
	}
	if f := fetches(); !strings.Contains(f, "refs/pull/1/head") || !strings.Contains(f, "refs/pull/2/head") || strings.Contains(f, "refs/pull/3") ||
		strings.Contains(f, "refs/pull/*") || strings.Contains(f, "refs/heads/*") || strings.Contains(f, "refs/heads/extra") {
		t.Errorf("fetch asked for:\n%s", f)
	}

	syncPulls(t, m, 2) // pull request 1 closes
	if got := refsOf(t, m); got != "refs/heads/main refs/pull/2/head" {
		t.Errorf("after pull request 1 closed: %s", got)
	}
	if f := fetches(); strings.Contains(f, "refs/pull/1") || !strings.Contains(f, "refs/pull/2/head") {
		t.Errorf("fetch asked for:\n%s", f)
	}

	syncOK(t, m, "") // GitHub could not be asked: keep what there is, ask for no pull request
	if got := refsOf(t, m); got != "refs/heads/main refs/pull/2/head" {
		t.Errorf("when the open pull requests are not known: %s", got)
	}
	if f := fetches(); strings.Contains(f, "refs/pull/") || !strings.Contains(f, "refs/heads/main") {
		t.Errorf("fetch asked for:\n%s", f)
	}

	syncPulls(t, m) // none is open now
	if got := refsOf(t, m); got != "refs/heads/main" {
		t.Errorf("with none open: %s", got)
	}
}

// A run on a pull request fetches just that pull request's head, and the next sync drops it again
// if the pull request is not open.
func TestEnsureFetchesOnlyThatPullRequest(t *testing.T) {
	s := newSource(t)
	s.seed()
	m := newMirrors(t, s)
	gitPath, argsLog, _ := wrapper(t, s.realGit)
	m.Git = gitPath
	syncPulls(t, m, 1, 2)
	os.Remove(argsLog)
	ctx := context.Background()
	if err := m.Ensure(ctx, 1, "owner/repo", "", s.prs[2], 3); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(argsLog)
	if !strings.Contains(string(b), "refs/pull/3/head") || strings.Contains(string(b), "refs/pull/1") || strings.Contains(string(b), "refs/pull/2") ||
		strings.Contains(string(b), "refs/heads") || strings.Contains(string(b), "ls-remote") {
		t.Errorf("Ensure ran:\n%s", b)
	}
	if got, err := m.Repo(1, "").Resolve(ctx, "pr/3"); err != nil || got != s.prs[2] {
		t.Errorf("pr/3 = %q, %v", got, err)
	}
	if err := m.Ensure(ctx, 1, "owner/repo", "", s.prs[1][:39]+"0", 2); err == nil {
		t.Error("a commit that is not the pull request's was ensured")
	}
	syncPulls(t, m, 1, 2)
	if got := refsOf(t, m); got != "refs/heads/main refs/pull/1/head refs/pull/2/head" {
		t.Errorf("the closed pull request's ref stayed: %s", got)
	}
}

// When GitHub's default branch changes, the mirror follows it and drops the old branch.
func TestDefaultBranchChange(t *testing.T) {
	s := newSource(t)
	_, second := s.seed()
	m := newMirrors(t, s)
	syncOK(t, m, "")
	s.push("main:refs/heads/trunk")
	s.git(s.bare, "symbolic-ref", "HEAD", "refs/heads/trunk")
	syncOK(t, m, "")
	if got := refsOf(t, m); got != "refs/heads/trunk" {
		t.Errorf("refs %s", got)
	}
	r := m.Repo(1, "")
	if got, err := r.Resolve(context.Background(), ""); err != nil || got != second {
		t.Errorf("the default branch: %q, %v", got, err)
	}
	if got, err := r.Resolve(context.Background(), "trunk"); err != nil || got != second {
		t.Errorf("trunk: %q, %v", got, err)
	}
}
