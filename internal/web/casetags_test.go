package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// getAs makes a signed-in request as if it came through the proxy for host.
func (e *env) getAs(b *browser, path, host, proto string) int {
	e.t.Helper()
	req, _ := http.NewRequest("GET", e.srv.URL+path, nil)
	req.Host = host
	if proto != "" {
		req.Header.Set("X-Forwarded-Proto", proto)
	}
	u, _ := url.Parse(e.srv.URL)
	for _, c := range b.c.Jar.Cookies(u) { // the jar would not send them with another Host
		req.AddCookie(c)
	}
	resp, err := b.c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// The public address is filled in once, from the first signed-in request's scheme and Host, and a
// later request, whatever its Host, never changes it. A request that is not signed in does not fill
// it, and neither does a Host that is not a host name.
func TestSiteAddressIsFilledOnceFromTheFirstSignedInRequest(t *testing.T) {
	e := newEnv(t)
	e.user("derek", true)
	b := e.signIn("derek")
	anon := e.browser()
	if code := e.getAs(anon, "/login", "stranger.example", "https"); code != 200 {
		t.Fatalf("the sign-in page: %d", code)
	}
	if v, err := e.st.Setting(publicAddressKey); err == nil && v != "" {
		t.Fatalf("a request that is not signed in filled it: %q", v)
	}
	if e.getAs(b, "/account", "odd_host.example", "https"); e.ui.siteAddress() != "" {
		t.Fatalf("a Host that is no name filled it: %q", e.ui.siteAddress())
	}
	e.getAs(b, "/account", "autobot.test.example", "https")
	if got := e.ui.siteAddress(); got != "https://autobot.test.example" {
		t.Fatalf("after the first signed-in request: %q", got)
	}
	e.getAs(b, "/account", "evil.example", "")
	e.getAs(b, "/", "autobot.test.example:8080", "http")
	if got := e.ui.siteAddress(); got != "https://autobot.test.example" {
		t.Fatalf("a later request changed it: %q", got)
	}
}

// Only an admin changes the public address; it is checked as an address, and clearing it lets the next
// signed-in request fill it again.
func TestOnlyAdminsChangeTheSiteAddress(t *testing.T) {
	e := newEnv(t)
	e.user("derek", true)
	e.user("erin", false)
	admin, regular := e.signIn("derek"), e.signIn("erin")
	e.getAs(admin, "/account", "autobot.test.example", "https")
	post := func(b *browser, address string) (int, string) {
		code, _, body := b.post("/admin/site-address", url.Values{"csrf": {b.csrf()}, "address": {address}})
		return code, body
	}
	if code, _ := post(regular, "https://hijack.example"); code != http.StatusNotFound || e.ui.siteAddress() != "https://autobot.test.example" {
		t.Fatalf("a regular user: %d, address %q", code, e.ui.siteAddress())
	}
	if code, body := post(admin, "javascript:alert(1)"); code != http.StatusBadRequest || !strings.Contains(body, "give the address as https://host") || e.ui.siteAddress() != "https://autobot.test.example" {
		t.Fatalf("not an address: %d %q", code, e.ui.siteAddress())
	}
	if code, _ := post(admin, " HTTPS://Autobot.Example.com/ "); code != http.StatusSeeOther || e.ui.siteAddress() != "https://autobot.example.com" {
		t.Fatalf("the admin: %d, address %q", code, e.ui.siteAddress())
	}
	if _, page := admin.get("/admin/users"); !strings.Contains(page, `value="https://autobot.example.com"`) {
		t.Errorf("the admin page does not show it")
	}
	if code, _ := regular.get("/admin/users"); code != http.StatusNotFound {
		t.Errorf("a regular user sees the admin page: %d", code)
	}
	// Empty: filled again from the next signed-in request, once.
	post(admin, "")
	if e.ui.siteAddress() != "" {
		t.Fatalf("not cleared: %q", e.ui.siteAddress())
	}
	e.getAs(admin, "/account", "second.example", "https")
	e.getAs(admin, "/account", "third.example", "https")
	if got := e.ui.siteAddress(); got != "https://second.example" {
		t.Errorf("refilled: %q", got)
	}
}

// {case} is the item's case file, as {pr:case_file} or {issue:case_file} has it; {case:link} is the
// address of the case file's page, built from the public address setting and the case's uuid, never
// from a request's Host; both are empty before the item has a case file; the old {case_link} reads
// {case:link}.
func TestCaseAndCaseLinkTags(t *testing.T) {
	c := newContEnv(t, nil)
	c.st.SetSetting(publicAddressKey, "https://autobot.example.com")
	c.saveGraph(append(append([]workflow.Node{}, events...),
		// Runs before the item has a case file.
		caseEntryNode("early", "Early", "EARLY case=[{case}] link=[{case:link}] file=[{pr:case_file}]"),
		caseEntryNode("late", "Late", "LATE<<{case}>>FILE<<{pr:case_file}>>LINK<<{case:link}>>OLD<<{case_link}>>")),
		[2]string{"ev1", "early"}, [2]string{"early", "late"})
	steps := c.run("ev1")
	if steps["early"].Output != "EARLY case=[] link=[] file=[]" {
		t.Errorf("before a case file: %q", steps["early"].Output)
	}
	cs, err := c.st.CaseByNumber(c.ws.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	link := "https://autobot.example.com/api/v1/cases/" + cs.UUID + ".html"
	late := steps["late"].Output
	caseText, rest, _ := strings.Cut(strings.TrimPrefix(late, "LATE<<"), ">>FILE<<")
	fileText, rest, _ := strings.Cut(rest, ">>LINK<<")
	got, old, _ := strings.Cut(rest, ">>OLD<<")
	if got != link || strings.TrimSuffix(old, ">>") != link {
		t.Errorf("the link: %q and the old name %q, want %s", got, old, link)
	}
	// The same text as {pr:case_file}, which holds the entry the first step wrote.
	if caseText != fileText || !strings.Contains(caseText, "EARLY case=[]") {
		t.Errorf("{case} is %q but {pr:case_file} is %q", caseText, fileText)
	}
	// A request with another Host changes nothing: the link comes from the setting.
	c.e().getAs(c.b, "/account", "evil.example", "https")
	if got := c.ui.caseLink(cs); got != link {
		t.Errorf("after another Host: %q", got)
	}
	// An issue's case file is {issue:case_file}.
	if got := c.ui.caseTags(workflow.Issue, cs); got["issue:case_file"] != cs.Contents || got["case"] != cs.Contents || got["pr:case_file"] != "" {
		t.Errorf("an issue's tags: %v", got)
	}
}

func (c *contEnv) e() *env { return c.pollEnv.env }

// The old {case_link} is read as {case:link} and still saves in a template.
func TestOldCaseLinkIsTheNewTag(t *testing.T) {
	if workflow.CanonicalTag("case_link", workflow.PR) != "case:link" {
		t.Fatal("the old name is not the new tag")
	}
	if got := workflow.Render("[{case_link}] [{case:link}]", map[string]string{"case:link": "L"}); got != "[L] [L]" {
		t.Errorf("rendered %q", got)
	}
	for _, kind := range []string{workflow.PR, workflow.Issue} {
		if err := workflow.CheckTemplates(kind, nil, "{case} {case:link} {case_link}"); err != nil {
			t.Errorf("%s: %v", kind, err)
		}
	}
}

// The case file as a page: the same access rule as the Markdown (open for a public repository, the
// secret or a signed-in member for a private one), the current contents, and the raw address in
// its header.
func TestCaseAsAPage(t *testing.T) {
	e := newEnv(t)
	admin := e.user("derek", true)
	e.user("outsider", false)
	pub, _ := e.st.CreateWorkspace("o/r", admin, "", "")
	priv, _ := e.st.CreateWorkspace("o/secret", admin, "flare-autobot", "tok-good")
	pc, _, _ := e.st.OpenCase(pub, 1, "pr", "Public", "Opened for o/r PR #1")
	sc, _, _ := e.st.OpenCase(priv, 2, "issue", "Private", "Opened for o/secret issue #2")
	e.st.SetSetting(publicAddressKey, "https://autobot.example.com")

	code, body := e.apiGet(t, nil, "/api/v1/cases/"+pc.UUID+".html", "")
	if code != http.StatusOK || !strings.Contains(body, "<p>Opened for o/r PR #1</p>") || !strings.Contains(body, "Raw Markdown for agents: https://autobot.example.com/api/v1/cases/"+pc.UUID) ||
		strings.Contains(body, "Sign out") {
		t.Fatalf("public page: %d\n%s", code, body)
	}
	// It follows the case file as it grows.
	editor, _ := e.st.Users()
	if err := e.st.EditCase(pc.UUID, 1, "Opened for o/r PR #1\n\nA new finding.\n", &editor[0]); err != nil {
		t.Fatal(err)
	}
	if _, body := e.apiGet(t, nil, "/api/v1/cases/"+pc.UUID+".html", ""); !strings.Contains(body, "<p>A new finding.</p>") {
		t.Errorf("the page is not current:\n%s", body)
	}
	if code, body := e.apiGet(t, nil, "/api/v1/cases/00000000-0000-4000-8000-000000000000.html", ""); code != http.StatusNotFound || strings.Contains(body, "secret") {
		t.Fatalf("unknown uuid: %d %q", code, body)
	}
	// Markdown is escaped like the case page does it.
	e.st.EditCase(pc.UUID, 2, "<script>alert(1)</script>\n", &editor[0])
	if _, body := e.apiGet(t, nil, "/api/v1/cases/"+pc.UUID+".html", ""); strings.Contains(body, "<script>alert") {
		t.Errorf("raw HTML in the case file got through:\n%s", body)
	}

	// A private repository: the same rule as the Markdown.
	path := "/api/v1/cases/" + sc.UUID + ".html"
	secret := "autobot-0123456789abcdef0123456789abcdef"
	fakeSecret = secret
	defer func() { fakeSecret = "" }()
	for _, bad := range []string{"", "wrong-wrong-wrong-wrong-wrong-wrong-wrong"} {
		if code, body := e.apiGet(t, nil, path, bad); code != http.StatusUnauthorized || !strings.Contains(body, "did not send it, or sent a value that does not match") || strings.Contains(body, "Opened for") {
			t.Errorf("bearer %q: %d %q", bad, code, body)
		}
	}
	if code, body := e.apiGet(t, nil, path, secret); code != http.StatusOK || !strings.Contains(body, "<p>Opened for o/secret issue #2</p>") {
		t.Fatalf("right secret: %d %q", code, body)
	}
	if code, _ := e.apiGet(t, e.signIn("derek"), path, ""); code != http.StatusOK {
		t.Fatalf("a member: %d", code)
	}
	if code, _ := e.apiGet(t, e.signIn("outsider"), path, ""); code != http.StatusUnauthorized {
		t.Fatalf("an outsider: %d", code)
	}
}

var _ = store.ErrNotFound

// The Test box does not ask for {case}, {case:link} or the old {case_link}: it fills them from the
// item's case file, as a run does, when the pull request's link is given.
func TestTestBoxFillsTheCaseTags(t *testing.T) {
	e := newEnv(t)
	derek := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", derek, "", "")
	cs, _, err := e.st.OpenCase(ws, 7, "pr", "Retry uploads", "Opened for PR #7")
	if err != nil {
		t.Fatal(err)
	}
	e.st.SetSetting(publicAddressKey, "https://autobot.example.com")
	g := workflow.Graph{Nodes: []workflow.Node{node("ce", workflow.CaseEntry, "Entry",
		workflow.CaseEntrySettings{Entry: "C=[{case}] L=[{case:link}] O=[{case_link}] T={pr:title}", Finalize: true})}, Links: []workflow.Link{}}
	b, _ := jsonMarshal(g)
	if _, err := e.st.SaveWorkflow(ws.ID, workflow.PR, 0, string(b), derek); err != nil {
		t.Fatal(err)
	}
	br := e.signIn("derek")
	tok := br.csrf()
	_, form := br.get("/w/o/r/workflows/pr/nodes/ce/test")
	for _, not := range []string{`name="tag_case"`, `name="tag_case:link"`, `name="tag_case_link"`} {
		if strings.Contains(form, not) {
			t.Errorf("the Test box asks for %s", not)
		}
	}
	if !strings.Contains(form, `name="tag_pr"`) {
		t.Errorf("the Test box does not ask for the pull request:\n%s", form)
	}
	code, _, out := br.post("/w/o/r/workflows/pr/nodes/ce/test", url.Values{"csrf": {tok}, "tag_pr": {"https://github.com/o/r/pull/7"}})
	link := "https://autobot.example.com/api/v1/cases/" + cs.UUID + ".html"
	if code != 200 || !strings.Contains(out, "C=[Opened for PR #7") || !strings.Contains(out, "L=["+link+"]") || !strings.Contains(out, "O=["+link+"]") {
		t.Fatalf("the test of a case entry %d:\n%s", code, out)
	}
}
