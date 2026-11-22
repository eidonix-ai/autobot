package web

import (
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

	"github.com/eidonix-ai/autobot/internal/store"
)

type env struct {
	t   *testing.T
	st  *store.Store
	srv *httptest.Server
}

func newEnv(t *testing.T) *env {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	ui, err := New(st)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(ui.Handler())
	t.Cleanup(func() { srv.Close(); st.Close() })
	return &env{t, st, srv}
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
	code, loc, _ := b.post("/login", url.Values{"username": {username}, "password": {"password123"}})
	if code != http.StatusSeeOther || loc != "/" {
		e.t.Fatalf("sign in %s: %d %q", username, code, loc)
	}
	return b
}

func (e *env) user(name string, admin bool) *store.User {
	u, err := e.st.CreateUser(name, "", "password123", admin)
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
	if code, loc, _ := b.post("/login", url.Values{"username": {"u"}, "password": {"password123"}, "next": {"//evil.example"}}); code != http.StatusSeeOther || loc != "/" {
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

	code, loc, _ = admin.post("/admin/users", url.Values{"csrf": {tok}, "username": {"alice"}, "display_name": {"Alice"}, "password": {"password123"}})
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
	ws, _ := e.st.CreateWorkspace("o/r", admin)
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

	form := url.Values{"username": {"derek"}, "display_name": {"Derek"}, "password": {"password123"}, "confirm": {"password123"}}

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

	mismatch := url.Values{"username": {"derek"}, "password": {"password123"}, "confirm": {"password124"}}
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
	second := url.Values{"username": {"mallory"}, "password": {"password123"}, "confirm": {"password123"}}
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
