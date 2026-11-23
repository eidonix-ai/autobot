package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eidonix-ai/autobot/internal/github"
	"github.com/eidonix-ai/autobot/internal/store"
)

// fakeGitHub knows a few public repositories (keyed by lower-case name, with their GitHub
// spelling) and two private ones, which only the token "tok-good" of flare-autobot can see.
type fakeGitHub struct{}

var fakeRepos = map[string]string{
	"nvidia/hyperflare-inference": "NVIDIA/hyperflare-inference",
	"eidonix-ai/autobot":          "eidonix-ai/autobot",
	"o/r":                         "o/r",
	"o/r2":                        "o/r2",
}

var fakePrivate = map[string]string{"o/secret": "o/secret", "o/secret2": "o/secret2"}

func (fakeGitHub) RepoName(_ context.Context, token, name string) (string, error) {
	if name == "down/repo" {
		return "", errors.New("could not reach GitHub: connection refused")
	}
	if c, ok := fakeRepos[strings.ToLower(name)]; ok {
		return c, nil
	}
	if c, ok := fakePrivate[strings.ToLower(name)]; ok && token == "tok-good" {
		return c, nil
	}
	return "", github.ErrNoRepo
}

func (fakeGitHub) Tarball(_ context.Context, token, name, commit string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("tarball of " + name + "@" + commit + " token=" + token)), nil
}

// fakeSecret is what o/secret's .github/autobot-secret.txt holds; "" for no file.
var fakeSecret = ""

func (fakeGitHub) File(_ context.Context, token, name, path string) (string, error) {
	if strings.ToLower(name) == "o/secret" && token == "tok-good" && path == ".github/autobot-secret.txt" && fakeSecret != "" {
		return fakeSecret + "\n", nil
	}
	return "", github.ErrNoRepo
}

func (fakeGitHub) PullRequest(_ context.Context, _, name string, n int) (*github.PullRequest, error) {
	if n != 7 {
		return nil, github.ErrNoRepo
	}
	return &github.PullRequest{Number: 7, URL: "https://github.com/" + name + "/pull/7", Title: "Retry uploads", Body: "Retries forever.",
		Author: "alice", State: "open", HeadSHA: "0123456789abcdef0123456789abcdef01234567", Base: "main"}, nil
}

func (fakeGitHub) Item(_ context.Context, _, name string, n int) (*github.Item, error) {
	if n == 7 {
		return &github.Item{Number: 7, Kind: "pr", Title: "Retry uploads", State: "open"}, nil
	}
	if n == 8 {
		return &github.Item{Number: 8, Kind: "issue", Title: "Uploads hang", State: "open", Body: "They never finish.", Author: "bob",
			URL: "https://github.com/" + name + "/issues/8"}, nil
	}
	return nil, github.ErrNoRepo
}

func (fakeGitHub) PullRequestFiles(context.Context, string, string, int) ([]github.ChangedFile, error) {
	return []github.ChangedFile{{Path: "upload.go", Status: "modified", Additions: 3, Deletions: 1, Patch: "@@ -1 +1,3 @@\n-retry(3)\n+for {\n+  retry()\n+}"}}, nil
}

func (fakeGitHub) Compare(context.Context, string, string, string, string) ([]github.Commit, []github.ChangedFile, error) {
	return nil, nil, errors.New("no comparison in this fake")
}

func (fakeGitHub) Comments(context.Context, string, string, int) ([]github.Comment, error) {
	return nil, nil
}

func (fakeGitHub) FileAt(_ context.Context, _, _, _, path string) (string, error) {
	if path == "upload.go" {
		return "package upload\n\nfunc send() {\n\tfor {\n\t\tretry()\n\t}\n}\n", nil
	}
	return "", github.ErrNoRepo
}

// fakeComments records what the fake GitHub was asked to post or edit.
var fakeComments []string

func (fakeGitHub) Comment(_ context.Context, token, name string, n int, body string) (int64, string, error) {
	fakeComments = append(fakeComments, fmt.Sprintf("post #%d by %s: %s", n, token, body))
	return int64(100 + len(fakeComments)), fmt.Sprintf("https://github.com/%s/pull/%d#issuecomment-%d", name, n, 100+len(fakeComments)), nil
}

func (fakeGitHub) EditComment(_ context.Context, token, name string, id int64, body string) (string, error) {
	fakeComments = append(fakeComments, fmt.Sprintf("edit %d by %s: %s", id, token, body))
	return fmt.Sprintf("https://github.com/%s/pull/7#issuecomment-%d", name, id), nil
}

func (fakeGitHub) Tree(context.Context, string, string, string) ([]string, error) {
	return []string{"go.mod", "upload.go"}, nil
}

// The poller's reads: this fake knows no changes. The poller's tests bring their own.
func (fakeGitHub) ListPulls(context.Context, string, string, string, int) ([]github.Listed, error) {
	return nil, nil
}

func (fakeGitHub) ListIssues(context.Context, string, string, string, time.Time, int) ([]github.Listed, error) {
	return nil, nil
}

func (fakeGitHub) RepoComments(context.Context, string, string, time.Time, int) ([]github.RepoComment, error) {
	return nil, nil
}

func (fakeGitHub) LatestComment(context.Context, string, string) (*github.RepoComment, error) {
	return nil, nil
}

func (fakeGitHub) Reviews(context.Context, string, string, int) ([]github.Review, error) {
	return nil, nil
}

func (fakeGitHub) CIState(context.Context, string, string, string) (string, error) {
	return "none", nil
}

func (fakeGitHub) Role(context.Context, string, string, string) (string, error) { return "none", nil }

func (fakeGitHub) DefaultBranch(context.Context, string, string) (string, error) { return "main", nil }

func (fakeGitHub) Private(_ context.Context, token, name string) (bool, error) {
	_, private := fakePrivate[strings.ToLower(name)]
	return private, nil
}

func (fakeGitHub) CommitSHA(_ context.Context, _, _, ref string) (string, error) {
	if ref == "main" {
		return "0123456789abcdef0123456789abcdef01234567", nil
	}
	return "", errors.New("no such ref")
}

func (f fakeGitHub) CheckIdentity(ctx context.Context, login, token, name string) (*github.Identity, error) {
	if token != "tok-good" {
		return nil, github.ErrBadToken
	}
	if !strings.EqualFold(login, "flare-autobot") {
		return nil, errors.New("that token belongs to the GitHub user flare-autobot, not " + login)
	}
	repo, err := f.RepoName(ctx, token, name)
	if err != nil {
		return nil, errors.New("the GitHub user flare-autobot cannot see " + name)
	}
	_, private := fakePrivate[strings.ToLower(name)]
	return &github.Identity{Login: "flare-autobot", TokenKind: "fine-grained token", Repo: repo, Private: private, Access: "write"}, nil
}

type env struct {
	t   *testing.T
	st  *store.Store
	srv *httptest.Server
	dir string // holds the database file
	ui  *Server
}

func newEnv(t *testing.T) *env { return newEnvWith(t, fakeGitHub{}) }

// newEnvWith is newEnv with another stand-in for GitHub.
func newEnvWith(t *testing.T, gh GitHub) *env {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UseKey(make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	ui, err := New(st, gh)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(ui.Handler())
	t.Cleanup(func() { srv.Close(); st.Close() })
	return &env{t, st, srv, dir, ui}
}

// browser is one person's cookie jar.
type browser struct {
	e *env
	c *http.Client
}

func (e *env) browser() *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{e, &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (b *browser) get(path string) (int, string) {
	b.e.t.Helper()
	resp, err := b.c.Get(b.e.srv.URL + path)
	if err != nil {
		b.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func (b *browser) post(path string, form url.Values) (int, string, string) {
	b.e.t.Helper()
	resp, err := b.c.PostForm(b.e.srv.URL+path, form)
	if err != nil {
		b.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Location"), string(body)
}

var csrfRE = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

// csrf returns the form token from a page the browser can see.
func (b *browser) csrf() string {
	b.e.t.Helper()
	_, body := b.get("/account")
	m := csrfRE.FindStringSubmatch(body)
	if m == nil {
		b.e.t.Fatal("no csrf token on /account")
	}
	return m[1]
}

func (e *env) signIn(username string) *browser {
	e.t.Helper()
	b := e.browser()
	code, loc, _ := b.post("/login", url.Values{"username": {username}, "password": {"password-1234"}})
	if code != http.StatusSeeOther || loc != "/" {
		e.t.Fatalf("sign in %s: %d %q", username, code, loc)
	}
	return b
}

func (e *env) user(name string, admin bool) *store.User {
	u, err := e.st.CreateUser(name, "", "password-1234", admin)
	if err != nil {
		e.t.Fatal(err)
	}
	return u
}

func TestSignInRequired(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	for _, p := range []string{"/", "/admin/users", "/w/a/b"} {
		resp, err := b.c.Get(e.srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login") {
			t.Errorf("%s: %d to %q, want redirect to /login", p, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	e.user("u", false)
	if code, _, body := b.post("/login", url.Values{"username": {"u"}, "password": {"nope-nope"}}); code != http.StatusUnauthorized || !strings.Contains(body, "Wrong username or password") {
		t.Errorf("bad password: %d", code)
	}
	// The post-login redirect stays on this site.
	if code, loc, _ := b.post("/login", url.Values{"username": {"u"}, "password": {"password-1234"}, "next": {"//evil.example"}}); code != http.StatusSeeOther || loc != "/" {
		t.Errorf("open redirect: %d %q", code, loc)
	}
}

func TestAdminSetsUpWorkspaceAndUser(t *testing.T) {
	e := newEnv(t)
	e.user("derek", true)
	admin := e.signIn("derek")
	tok := admin.csrf()

	code, loc, _ := admin.post("/workspaces", url.Values{"csrf": {tok}, "name": {"https://github.com/NVIDIA/hyperflare-inference"}})
	if code != http.StatusSeeOther || loc != "/w/NVIDIA/hyperflare-inference" {
		t.Fatalf("create workspace: %d %q", code, loc)
	}
	if code, _, body := admin.post("/workspaces", url.Values{"csrf": {tok}, "name": {"not a repo"}}); code != http.StatusBadRequest || !strings.Contains(body, "owner/repo") {
		t.Fatalf("bad workspace name: %d", code)
	}

	code, loc, _ = admin.post("/admin/users", url.Values{"csrf": {tok}, "username": {"alice"}, "display_name": {"Alice"}, "password": {"password-1234"}})
	if code != http.StatusSeeOther || !strings.HasPrefix(loc, "/admin/users/") {
		t.Fatalf("create user: %d %q", code, loc)
	}
	alicePage := strings.SplitN(loc, "?", 2)[0]
	ws, _ := e.st.Workspace("NVIDIA/hyperflare-inference")

	alice := e.signIn("alice")
	if _, body := alice.get("/"); strings.Contains(body, "hyperflare") {
		t.Fatal("unassigned user sees the workspace")
	}
	if code, _ := alice.get("/w/NVIDIA/hyperflare-inference"); code != http.StatusNotFound {
		t.Fatalf("unassigned user opens the workspace: %d", code)
	}

	code, _, _ = admin.post(alicePage, url.Values{"csrf": {tok}, "display_name": {"Alice"}, "workspace": {itoa(ws.ID)}})
	if code != http.StatusSeeOther {
		t.Fatalf("assign: %d", code)
	}
	if _, body := alice.get("/"); !strings.Contains(body, "NVIDIA/hyperflare-inference") {
		t.Fatal("assigned user does not see the workspace")
	}
	code, body := alice.get("/w/nvidia/hyperflare-inference")
	if code != http.StatusOK || !strings.Contains(body, "derek assigned Alice") {
		t.Fatalf("workspace page: %d, history missing the assignment", code)
	}

	// A regular user cannot reach admin pages or create workspaces.
	if code, _ := alice.get("/admin/users"); code != http.StatusNotFound {
		t.Errorf("regular user on /admin/users: %d", code)
	}
	if code, _, _ := alice.post("/workspaces", url.Values{"csrf": {alice.csrf()}, "name": {"a/b"}}); code != http.StatusNotFound {
		t.Errorf("regular user creates a workspace: %d", code)
	}

	// The only admin cannot demote themselves.
	derek, _ := e.st.UserByUsername("derek")
	if code, _, body := admin.post("/admin/users/"+itoa(derek.ID), url.Values{"csrf": {tok}}); code != http.StatusBadRequest || !strings.Contains(body, "only active admin") {
		t.Errorf("last admin demotion: %d", code)
	}
}

func TestCharterEditing(t *testing.T) {
	e := newEnv(t)
	admin := e.user("derek", true)
	alice := e.user("alice", false)
	ws, _ := e.st.CreateWorkspace("o/r", admin, "", "")
	e.st.UpdateUser(alice.ID, store.UserUpdate{Workspaces: []int64{ws.ID}}, admin)

	a := e.signIn("alice")
	d := e.signIn("derek")

	if code, body := a.get("/w/o/r/charter"); code != http.StatusOK || !strings.Contains(body, `id="vision"`) {
		t.Fatalf("charter page: %d", code)
	}
	// Both open the editor on revision 1. Derek saves first.
	if code, loc, _ := d.post("/w/o/r/charter", url.Values{"csrf": {d.csrf()}, "base_rev": {"1"}, "body": {"# Derek's charter\n"}}); code != http.StatusSeeOther || loc != "/w/o/r/charter" {
		t.Fatalf("derek save: %d %q", code, loc)
	}
	// Alice's save, still based on revision 1, is refused and her text kept.
	code, _, body := a.post("/w/o/r/charter", url.Values{"csrf": {a.csrf()}, "base_rev": {"1"}, "body": {"# Alice's charter\n"}})
	if code != http.StatusConflict || !strings.Contains(body, "Alice&#39;s charter") || !strings.Contains(body, `name="base_rev" value="2"`) {
		t.Fatalf("conflict: %d\n%s", code, body)
	}
	if c, _ := e.st.Charter(ws.ID, 0); c.Body != "# Derek's charter\n" {
		t.Fatalf("conflicting save overwrote the charter: %q", c.Body)
	}

	// Raw HTML in the charter is escaped, not executed.
	e.st.SaveCharter(ws.ID, 2, "# T\n\n<script>alert(1)</script>\n\n[x](javascript:alert(1))\n", admin)
	_, body = a.get("/w/o/r/charter")
	if strings.Contains(body, "<script>alert") || strings.Contains(body, `href="javascript:`) {
		t.Fatal("charter renders raw HTML or javascript: links")
	}

	// An older revision is viewable read-only.
	if code, body := a.get("/w/o/r/charter?rev=1"); code != http.StatusOK || !strings.Contains(body, "older version") || strings.Contains(body, "/charter/edit") {
		t.Fatalf("old revision view: %d", code)
	}
}

func TestPostsNeedFormToken(t *testing.T) {
	e := newEnv(t)
	e.user("derek", true)
	d := e.signIn("derek")
	if code, _, _ := d.post("/workspaces", url.Values{"name": {"a/b"}}); code != http.StatusForbidden {
		t.Fatalf("post without token: %d", code)
	}
	if code, _, _ := d.post("/workspaces", url.Values{"csrf": {"forged"}, "name": {"a/b"}}); code != http.StatusForbidden {
		t.Fatalf("post with forged token: %d", code)
	}
	if _, err := e.st.Workspace("a/b"); err == nil {
		t.Fatal("workspace created without a valid token")
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestFirstTimeSetup(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	if code, loc := redirect(t, b, "/login"); code != http.StatusSeeOther || loc != "/setup" {
		t.Fatalf("/login with no users: %d %q, want /setup", code, loc)
	}
	if code, body := b.get("/setup"); code != http.StatusOK || !strings.Contains(body, "Create admin account") {
		t.Fatalf("setup page: %d", code)
	}

	form := url.Values{"username": {"derek"}, "display_name": {"Derek"}, "password": {"password-1234"}, "confirm": {"password-1234"}}

	// A form posted from another site is refused, so a web page cannot plant its own admin.
	for _, h := range []map[string]string{{"Sec-Fetch-Site": "cross-site"}, {"Origin": "https://evil.example"}} {
		req, _ := http.NewRequest("POST", e.srv.URL+"/setup", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for k, v := range h {
			req.Header.Set(k, v)
		}
		resp, err := b.c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("cross-site setup with %v: %d, want 403", h, resp.StatusCode)
		}
	}
	if has, _ := e.st.HasUsers(); has {
		t.Fatal("cross-site post created a user")
	}

	mismatch := url.Values{"username": {"derek"}, "password": {"password-1234"}, "confirm": {"password124"}}
	if code, _, body := b.post("/setup", mismatch); code != http.StatusBadRequest || !strings.Contains(body, "did not match") {
		t.Fatalf("mismatched passwords: %d", code)
	}

	if code, loc, _ := b.post("/setup", form); code != http.StatusSeeOther || loc != "/" {
		t.Fatalf("setup: %d %q", code, loc)
	}
	// Signed in as the new admin.
	if code, body := b.get("/"); code != http.StatusOK || !strings.Contains(body, `href="/admin/users"`) {
		t.Fatalf("home after setup: %d, admin nav missing", code)
	}

	// Setup is closed for good.
	other := e.browser()
	if code, loc := redirect(t, other, "/setup"); code != http.StatusSeeOther || loc != "/login" {
		t.Fatalf("setup page after setup: %d %q", code, loc)
	}
	second := url.Values{"username": {"mallory"}, "password": {"password-1234"}, "confirm": {"password-1234"}}
	if code, loc, _ := other.post("/setup", second); code != http.StatusSeeOther || loc != "/login" {
		t.Fatalf("second setup: %d %q", code, loc)
	}
	if _, err := e.st.UserByUsername("mallory"); err == nil {
		t.Fatal("second setup created a user")
	}
	if code, _ := other.get("/login"); code != http.StatusOK {
		t.Fatalf("login page after setup: %d", code)
	}
}

func redirect(t *testing.T, b *browser, path string) (int, string) {
	t.Helper()
	resp, err := b.c.Get(b.e.srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Location")
}

func TestWorkspaceMustExistOnGitHub(t *testing.T) {
	e := newEnv(t)
	e.user("derek", true)
	d := e.signIn("derek")
	tok := d.csrf()
	for name, want := range map[string]string{
		"nobody/nothing": "GitHub has no public repository named nobody/nothing",
		"down/repo":      "Could not confirm down/repo on GitHub",
	} {
		code, _, body := d.post("/workspaces", url.Values{"csrf": {tok}, "name": {name}})
		if code != http.StatusBadRequest || !strings.Contains(body, want) {
			t.Errorf("%s: %d, want 400 with %q", name, code, want)
		}
		if _, err := e.st.Workspace(name); err == nil {
			t.Errorf("%s was saved", name)
		}
	}
	// Saved as GitHub spells it.
	if code, loc, _ := d.post("/workspaces", url.Values{"csrf": {tok}, "name": {"nvidia/hyperflare-inference"}}); code != http.StatusSeeOther || loc != "/w/NVIDIA/hyperflare-inference" {
		t.Fatalf("create: %d %q", code, loc)
	}
}

func TestWorkspaceSettings(t *testing.T) {
	e := newEnv(t)
	admin := e.user("derek", true)
	alice := e.user("alice", false)
	ws, _ := e.st.CreateWorkspace("o/r", admin, "", "") // as if made before the GitHub check existed
	e.st.CreateWorkspace("eidonix-ai/autobot", admin, "", "")
	e.st.UpdateUser(alice.ID, store.UserUpdate{Workspaces: []int64{ws.ID}}, admin)
	d := e.signIn("derek")
	a := e.signIn("alice")
	tok := d.csrf()

	// Members configure their workspace, but renaming it, choosing its members and deleting it
	// stay with admins.
	if code, body := a.get("/w/o/r/settings"); code != http.StatusOK || !strings.Contains(body, "GitHub identity") ||
		strings.Contains(body, "Delete this workspace") || strings.Contains(body, "settings/name") {
		t.Errorf("member settings page: %d", code)
	}
	atok := a.csrf()
	for op, form := range map[string]url.Values{
		"delete":  {"csrf": {atok}, "confirm": {"o/r"}},
		"name":    {"csrf": {atok}, "name": {"o/r2"}},
		"members": {"csrf": {atok}},
	} {
		if code, _, _ := a.post("/w/o/r/settings/"+op, form); code != http.StatusNotFound {
			t.Errorf("member POSTs settings/%s: %d", op, code)
		}
	}
	if code, _, _ := a.post("/w/o/r/settings/github", url.Values{"csrf": {atok}, "gh_login": {"flare-autobot"}, "gh_token": {"tok-good"}}); code != http.StatusSeeOther {
		t.Errorf("member sets the GitHub identity: %d", code)
	}
	if _, err := e.st.Workspace("o/r"); err != nil {
		t.Fatal("a member's post removed or renamed the workspace")
	}
	if code, _, _ := a.post("/w/o/r/settings/github/remove", url.Values{"csrf": {atok}}); code != http.StatusSeeOther {
		t.Errorf("member removes the GitHub identity: %d", code)
	}
	if code, body := d.get("/w/o/r/settings"); code != http.StatusOK || !strings.Contains(body, "Delete this workspace") {
		t.Fatalf("admin settings page: %d", code)
	}

	// Rename: must exist on GitHub and not clash with another workspace.
	for name, want := range map[string]string{"nobody/nothing": "no public repository", "eidonix-ai/autobot": "already named"} {
		if code, _, body := d.post("/w/o/r/settings/name", url.Values{"csrf": {tok}, "name": {name}}); code != http.StatusBadRequest || !strings.Contains(body, want) {
			t.Errorf("rename to %s: %d, want 400 %q", name, code, want)
		}
	}
	if code, loc, _ := d.post("/w/o/r/settings/name", url.Values{"csrf": {tok}, "name": {"O/R2"}}); code != http.StatusSeeOther || loc != "/w/o/r2/settings?saved=1" {
		t.Fatalf("rename: %d %q", code, loc)
	}
	if code, _ := d.get("/w/o/r"); code != http.StatusNotFound {
		t.Errorf("old address still works: %d", code)
	}
	if code, body := a.get("/w/o/r2"); code != http.StatusOK || !strings.Contains(body, "renamed the workspace from o/r to o/r2") {
		t.Fatalf("renamed workspace for its member: %d", code)
	}

	// Members from the workspace side.
	if code, _, _ := d.post("/w/o/r2/settings/members", url.Values{"csrf": {tok}}); code != http.StatusSeeOther {
		t.Fatalf("clear members: %d", code)
	}
	if code, _ := a.get("/w/o/r2"); code != http.StatusNotFound {
		t.Fatalf("unassigned member still opens it: %d", code)
	}
	if code, _, _ := d.post("/w/o/r2/settings/members", url.Values{"csrf": {tok}, "member": {itoa(alice.ID)}}); code != http.StatusSeeOther {
		t.Fatalf("set members: %d", code)
	}
	if code, _ := a.get("/w/o/r2"); code != http.StatusOK {
		t.Fatalf("reassigned member cannot open it: %d", code)
	}

	// Delete needs the name typed.
	if code, _, _ := d.post("/w/o/r2/settings/delete", url.Values{"csrf": {tok}, "confirm": {"o/r"}}); code != http.StatusBadRequest {
		t.Fatalf("delete with wrong name: %d", code)
	}
	if _, err := e.st.Workspace("o/r2"); err != nil {
		t.Fatal("deleted without confirmation")
	}
	if code, loc, _ := d.post("/w/o/r2/settings/delete", url.Values{"csrf": {tok}, "confirm": {"o/r2"}}); code != http.StatusSeeOther || loc != "/?deleted=1" {
		t.Fatalf("delete: %d %q", code, loc)
	}
	if _, err := e.st.Workspace("o/r2"); err == nil {
		t.Fatal("workspace still exists")
	}
	if m, _ := e.st.Memberships(alice.ID); len(m) != 0 {
		t.Fatalf("assignments survived the delete: %v", m)
	}
}

func TestPrivateWorkspaceNeedsIdentity(t *testing.T) {
	e := newEnv(t)
	e.user("derek", true)
	d := e.signIn("derek")
	tok := d.csrf()
	create := func(login, token string) (int, string, string) {
		return d.post("/workspaces", url.Values{"csrf": {tok}, "name": {"o/secret"}, "gh_login": {login}, "gh_token": {token}})
	}
	for _, c := range []struct{ login, token, want string }{
		{"", "", "If it is private, enter the GitHub user and token"},
		{"flare-autobot", "", "Enter both the GitHub user and that user"},
		{"flare-autobot", "tok-bad", "did not accept the token"},
		{"someone", "tok-good", "belongs to the GitHub user flare-autobot, not someone"},
	} {
		code, _, body := create(c.login, c.token)
		if code != http.StatusBadRequest || !strings.Contains(body, c.want) {
			t.Errorf("login %q token %q: %d, want 400 with %q", c.login, c.token, code, c.want)
		}
		if strings.Contains(body, "tok-") {
			t.Errorf("login %q: the token was sent back in the page", c.login)
		}
	}
	if code, loc, _ := create("Flare-Autobot", "tok-good"); code != http.StatusSeeOther || loc != "/w/o/secret" {
		t.Fatalf("create with identity: %d %q", code, loc)
	}

	code, body := d.get("/w/o/secret/settings")
	if code != http.StatusOK || !strings.Contains(body, "Autobot acts on GitHub as <strong>flare-autobot</strong>") ||
		!strings.Contains(body, "private repository flare-autobot has <strong>write</strong> access") {
		t.Fatalf("settings: %d\n%s", code, body)
	}
	for _, page := range []string{"/", "/w/o/secret", "/w/o/secret/settings"} {
		if _, body := d.get(page); strings.Contains(body, "tok-good") {
			t.Errorf("%s shows the token", page)
		}
	}
	if _, body := d.get("/w/o/secret"); !strings.Contains(body, "set the GitHub identity to flare-autobot") {
		t.Error("history lacks the identity")
	}

	// A private repository cannot lose its identity.
	if code, _, body := d.post("/w/o/secret/settings/github/remove", url.Values{"csrf": {tok}}); code != http.StatusBadRequest || !strings.Contains(body, "is private") {
		t.Fatalf("remove from private: %d", code)
	}
	// Renames are checked with the workspace's token, so they work for private repositories.
	if code, loc, _ := d.post("/w/o/secret/settings/name", url.Values{"csrf": {tok}, "name": {"o/secret2"}}); code != http.StatusSeeOther || loc != "/w/o/secret2/settings?saved=1" {
		t.Fatalf("private rename: %d %q", code, loc)
	}
}

func TestPublicWorkspaceIdentity(t *testing.T) {
	e := newEnv(t)
	admin := e.user("derek", true)
	e.st.CreateWorkspace("NVIDIA/hyperflare-inference", admin, "", "")
	d := e.signIn("derek")
	tok := d.csrf()
	p := "/w/NVIDIA/hyperflare-inference/settings"

	if _, body := d.get(p); !strings.Contains(body, "Autobot uses GitHub anonymously here") {
		t.Fatal("no-identity state not shown")
	}
	code, _, body := d.post(p+"/github", url.Values{"csrf": {tok}, "gh_login": {"flare-autobot"}, "gh_token": {"tok-bad"}})
	if code != http.StatusBadRequest || !strings.Contains(body, `value="flare-autobot"`) || strings.Contains(body, "tok-bad") {
		t.Fatalf("bad token: %d; want 400 keeping the typed user and dropping the token", code)
	}
	if code, _, _ := d.post(p+"/github", url.Values{"csrf": {tok}, "gh_login": {"flare-autobot"}, "gh_token": {"tok-good"}}); code != http.StatusSeeOther {
		t.Fatalf("save identity: %d", code)
	}
	if _, body := d.get(p); !strings.Contains(body, "public repository flare-autobot has <strong>write</strong>") {
		t.Fatal("identity not shown")
	}
	if code, _, _ := d.post(p+"/github/remove", url.Values{"csrf": {tok}}); code != http.StatusSeeOther {
		t.Fatalf("remove from public: %d", code)
	}
	if _, body := d.get("/w/NVIDIA/hyperflare-inference"); !strings.Contains(body, "removed the GitHub identity") {
		t.Fatal("history lacks the removal")
	}
}
