package web

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/eidonix-ai/autobot/internal/workflow"
)

var pageLinkRE = regexp.MustCompile(`\[([^\]]*)\]\(https://autobot\.example\.com/p/([0-9a-f-]{36})\.html\)`)

// A [[tag]] in a template becomes a Markdown link to a page that keeps the tag's value as it was; the
// same content of the same tag is one page, a changed value a new one; [[tag|Label]] sets the text;
// [[case]] is the live case file and publishes nothing; the page serves the content as a page and as
// raw Markdown; a GitHub Comment's trial writes the links into the case file.
func TestLinksPublishPagesAndReuseThem(t *testing.T) {
	c := newContEnv(t, nil)
	c.st.SetSetting(publicAddressKey, "https://autobot.example.com")
	cm := workflow.DefaultPost()
	cm.Message = "Comment: [[first|The first one]]"
	build := func(first string) {
		c.saveGraph(append(append([]workflow.Node{}, events...),
			caseEntryNode("a", "First", first),
			caseEntryNode("b", "Linker", "A=[[first]] B=[[first|The first]] C=[[case]] D=[[[[first]]"),
			node("cm", workflow.Post, "Comment", cm)),
			[2]string{"ev1", "a"}, [2]string{"a", "b"}, [2]string{"b", "cm"})
	}
	build("The v1 text, with <b>markup</b>.")
	steps := c.run("ev1")
	out := steps["b"].Output
	links := pageLinkRE.FindAllStringSubmatch(out, -1)
	if len(links) != 2 || links[0][1] != "First" || links[1][1] != "The first" || links[0][2] != links[1][2] {
		t.Fatalf("the links in %q: %v", out, links)
	}
	id := links[0][2]
	cs, _ := c.st.CaseByNumber(c.ws.ID, 1)
	if !strings.Contains(out, "C=[Case file](https://autobot.example.com/api/v1/cases/"+cs.UUID+".html)") || !strings.HasSuffix(out, "D=[[first]]") {
		t.Errorf("the case link, or the escape: %q", out)
	}
	pages, _ := c.st.PagesOf(cs.UUID)
	if len(pages) != 1 || pages[0].Tag != "first" || pages[0].Content != "The v1 text, with <b>markup</b>." || pages[0].UUID != id || pages[0].Item != 1 {
		t.Fatalf("pages %+v: [[case]] must publish nothing, and one content is one page", pages)
	}
	// The trial wrote its link into the case file.
	if !strings.Contains(cs.Contents, "Comment: [The first one](https://autobot.example.com/p/"+id+".html)") {
		t.Errorf("the case file:\n%s", cs.Contents)
	}
	// Served as a page and as raw text, to anyone for a public repository, and escaped.
	code, body := c.e().apiGet(t, nil, "/p/"+id+".html", "")
	if code != http.StatusOK || !strings.Contains(body, "The v1 text, with") || strings.Contains(body, "<b>markup</b>") || !strings.Contains(body, "<title>First · o/r #1") {
		t.Errorf("the page: %d\n%s", code, body)
	}
	code, body = c.e().apiGet(t, nil, "/p/"+id+".md", "")
	if code != http.StatusOK || body != "The v1 text, with <b>markup</b>." {
		t.Errorf("the raw text: %d %q", code, body)
	}
	// The same content again is the same page.
	steps = c.run("ev1")
	if again := pageLinkRE.FindStringSubmatch(steps["b"].Output); again == nil || again[2] != id {
		t.Errorf("a second run made another page: %q", steps["b"].Output)
	}
	// A changed value is a new page, and the old address still shows the old text.
	build("The v2 text.")
	steps = c.run("ev1")
	next := pageLinkRE.FindStringSubmatch(steps["b"].Output)
	if next == nil || next[2] == id {
		t.Fatalf("a changed value kept the page: %q", steps["b"].Output)
	}
	if pages, _ := c.st.PagesOf(cs.UUID); len(pages) != 2 {
		t.Errorf("%d pages", len(pages))
	}
	if _, body := c.e().apiGet(t, nil, "/p/"+id+".md", ""); body != "The v1 text, with <b>markup</b>." {
		t.Errorf("the old page changed: %q", body)
	}
	if _, body := c.e().apiGet(t, nil, "/p/"+next[2]+".md", ""); body != "The v2 text." {
		t.Errorf("the new page: %q", body)
	}
	// An address that is no page, and a page with another ending, are 404.
	for _, p := range []string{"/p/00000000-0000-4000-8000-000000000000.html", "/p/00000000-0000-4000-8000-000000000000.md", "/p/" + id, "/p/" + id + ".txt"} {
		if code, _ := c.e().apiGet(t, nil, p, ""); code != http.StatusNotFound {
			t.Errorf("%s: %d", p, code)
		}
	}
}

// With no case file yet [[case]] says so; a value that is empty still gets a page, which says it is empty.
func TestLinkToNoCaseFileAndToAnEmptyValue(t *testing.T) {
	c := newContEnv(t, nil)
	c.st.SetSetting(publicAddressKey, "https://autobot.example.com")
	c.saveGraph(append(append([]workflow.Node{}, events...),
		caseEntryNode("a", "Early", "X [[case]] and [[case|Case File]]"),
		caseEntryNode("b", "Nothing", "{previous_output}"),
		caseEntryNode("c", "Look", "Y [[nothing|Empty one]]")),
		[2]string{"ev1", "a"}, [2]string{"a", "b"}, [2]string{"b", "c"})
	steps := c.run("ev1")
	if steps["a"].Output != "X Case file (not opened yet) and Case File (not opened yet)" {
		t.Errorf("before a case file: %q", steps["a"].Output)
	}
	// Written by the first step, so the next ones link to it.
	m := pageLinkRE.FindStringSubmatch(steps["c"].Output)
	if m == nil || m[1] != "Empty one" {
		t.Fatalf("%q", steps["c"].Output)
	}
	// Its value is the first step's text, not empty: make an empty one directly.
	cs, _ := c.st.CaseByNumber(c.ws.ID, 1)
	empty, err := c.st.Publish(cs, "nothing", "")
	if err != nil {
		t.Fatal(err)
	}
	if code, body := c.e().apiGet(t, nil, "/p/"+empty+".html", ""); code != http.StatusOK || !strings.Contains(body, "This is empty.") {
		t.Errorf("the empty page: %d\n%s", code, body)
	}
	if code, body := c.e().apiGet(t, nil, "/p/"+empty+".md", ""); code != http.StatusOK || body != "" {
		t.Errorf("the empty raw page: %d %q", code, body)
	}
}

// A test of one block from the canvas, and a dry run, publish nothing: the link says so.
func TestATestAndADryRunPublishNothing(t *testing.T) {
	c := newContEnv(t, nil)
	c.st.SetSetting(publicAddressKey, "https://autobot.example.com")
	c.saveGraph(append(append([]workflow.Node{}, events...),
		caseEntryNode("a", "First", "Some text."),
		caseEntryNode("b", "Linker", "See [[first]] and [[first|Label here]] and [[case]]")),
		[2]string{"ev1", "a"}, [2]string{"a", "b"})
	id, err := c.ui.startWorkflow(c.t.Context(), c.ws, workflow.PR, "ev1", 1, true, c.admin)
	if err != nil {
		t.Fatal(err)
	}
	steps := c.finish(id)
	if want := "See First (not published in a test) and Label here (not published in a test) and Case file (not opened yet)"; steps["b"].Output != want {
		t.Errorf("a dry run: %q, want %q", steps["b"].Output, want)
	}
	if _, err := c.st.CaseByNumber(c.ws.ID, 1); err == nil {
		t.Error("a dry run opened a case")
	}
	// The Test box of the block on the canvas.
	code, _, body := c.b.post("/w/o/r/workflows/pr/nodes/b/test", url.Values{"csrf": {c.csrf}, "tag_first": {"x"}})
	if code != 200 || !strings.Contains(body, "See First (not published in a test) and Label here (not published in a test) and Case file (not opened yet)") {
		t.Fatalf("the Test box %d:\n%s", code, body)
	}
	cs, _, _ := c.st.OpenCase(c.ws, 1, "pr", "PR 1", "Opened")
	if pages, _ := c.st.PagesOf(cs.UUID); len(pages) != 0 {
		t.Errorf("%d pages were published", len(pages))
	}
}

// A page of a private repository follows the case file's rule: it needs the repository's secret, or a
// signed-in member of the workspace, and without it gets the same instructions; a public one needs nothing.
func TestPagesOfAPrivateRepository(t *testing.T) {
	e := newEnv(t)
	admin := e.user("derek", true)
	e.user("outsider", false)
	pub, _ := e.st.CreateWorkspace("o/r", admin, "", "")
	priv, _ := e.st.CreateWorkspace("o/secret", admin, "flare-autobot", "tok-good")
	pc, _, _ := e.st.OpenCase(pub, 1, "pr", "Public", "Opened")
	sc, _, _ := e.st.OpenCase(priv, 2, "issue", "Private", "Opened")
	pubPage, _ := e.st.Publish(pc, "report", "PUBLIC TEXT")
	privPage, _ := e.st.Publish(sc, "report", "PRIVATE TEXT")
	for _, ext := range []string{".html", ".md"} {
		if code, body := e.apiGet(t, nil, "/p/"+pubPage+ext, ""); code != http.StatusOK || !strings.Contains(body, "PUBLIC TEXT") {
			t.Errorf("public %s: %d %q", ext, code, body)
		}
	}
	secret := "autobot-0123456789abcdef0123456789abcdef"
	fakeSecret = secret
	defer func() { fakeSecret = "" }()
	for _, ext := range []string{".html", ".md"} {
		path := "/p/" + privPage + ext
		for _, bad := range []string{"", "wrong-wrong-wrong-wrong-wrong-wrong-wrong"} {
			if code, body := e.apiGet(t, nil, path, bad); code != http.StatusUnauthorized || !strings.Contains(body, "did not send it, or sent a value that does not match") || strings.Contains(body, "PRIVATE TEXT") {
				t.Errorf("%s with %q: %d %q", path, bad, code, body)
			}
		}
		if code, body := e.apiGet(t, nil, path, secret); code != http.StatusOK || !strings.Contains(body, "PRIVATE TEXT") {
			t.Errorf("%s with the secret: %d %q", path, code, body)
		}
		if code, _ := e.apiGet(t, e.signIn("derek"), path, ""); code != http.StatusOK {
			t.Errorf("%s as a member: %d", path, code)
		}
		if code, body := e.apiGet(t, e.signIn("outsider"), path, ""); code != http.StatusUnauthorized || strings.Contains(body, "PRIVATE TEXT") {
			t.Errorf("%s as an outsider: %d", path, code)
		}
	}
}

// The Tags list explains [[name]] and [[name|Label]], and a bad name is refused even with a label.
func TestTagsListExplainsLinks(t *testing.T) {
	c := newContEnv(t, nil)
	c.saveGraph(append(append([]workflow.Node{}, events...), caseEntryNode("a", "First", "x"), caseEntryNode("b", "Second", "y")), [2]string{"ev1", "a"}, [2]string{"a", "b"})
	_, body := c.b.get("/w/o/r/workflows/pr/nodes/b")
	if !strings.Contains(body, "<dt><code>[[name]]</code></dt><dd>a link to a page that keeps that tag's value") || !strings.Contains(body, "<code>[[name|Label]]</code> sets the link's text") {
		t.Errorf("the Tags list:\n%s", body)
	}
	save := func(entry string) (int, string) {
		code, _, out := c.b.post("/w/o/r/workflows/pr/nodes/b", url.Values{"csrf": {c.csrf}, "version": {itoa(int64(c.version()))}, "name": {"Second"}, "entry": {entry}, "finalize": {"on"}})
		return code, out
	}
	if code, out := save("[[first|Intake]] and [[first]]"); code != 200 {
		t.Fatalf("good links: %d %s", code, out)
	}
	if code, out := save("[[nosuch|Intake]]"); code != 400 || !strings.Contains(out, "[[nosuch]]: no block is called Nosuch") {
		t.Errorf("a bad name with a label: %d\n%s", code, out)
	}
}
