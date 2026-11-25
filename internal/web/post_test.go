package web

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"html"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// postEnv is a workspace with a Discord bot and forum, a Post block on a pull request workflow, and a way to run it.
type postEnv struct {
	*env
	ws    *store.Workspace
	admin *store.User
}

func newPostEnv(t *testing.T, name string) *postEnv {
	fakeDiscord(t)
	fakeComments = nil
	e := newEnv(t)
	admin := e.user("derek", true)
	ws, err := e.st.CreateWorkspace(name, admin, "flare-autobot", "tok-good")
	if err != nil {
		t.Fatal(err)
	}
	pub, _, _ := ed25519.GenerateKey(nil)
	e.st.SetDiscordBot(ws, "good-token", hex.EncodeToString(pub), admin)
	e.st.SetDiscordForum(ws, "f1", "flare-autobot", admin)
	e.st.SetSetting(publicAddressKey, "https://autobot.example.com")
	return &postEnv{e, ws, admin}
}

// run saves a workflow of an event and a Post and runs it on pull request 7; it returns the Post's step.
func (p *postEnv) run(t *testing.T, s workflow.PostSettings) store.WorkflowStep {
	t.Helper()
	b, _ := json.Marshal(s)
	g := workflow.Graph{Nodes: []workflow.Node{{ID: "ev", Type: "pr_ready"}, {ID: "po", Type: workflow.Post, Name: "Say it", Settings: b}},
		Links: []workflow.Link{{From: "ev", Out: "out", To: "po", In: "in"}}}
	gb, _ := json.Marshal(g)
	w, _ := p.st.Workflow(p.ws.ID, workflow.PR)
	if _, err := p.st.SaveWorkflow(p.ws.ID, workflow.PR, w.Version, string(gb), p.admin); err != nil {
		t.Fatal(err)
	}
	return runOnce(t, p.env, p.ws, p.admin, false)["po"]
}

var publishedLinkRE = regexp.MustCompile(`https://autobot\.example\.com/p/[0-9a-f-]{36}\.html`)

// With both ticked the same text, rendered once, goes to GitHub and then Discord: a link in it is the
// same page in both. GitHub alone and Discord alone post to one place only.
func TestPostSaysTheSameTextWhereverTicked(t *testing.T) {
	p := newPostEnv(t, "o/r")
	step := p.run(t, workflow.PostSettings{Message: "Look at [[pr:title|the title]]: {pr:title}", GitHub: true, Discord: true})
	if step.Status != "done" || !strings.Contains(step.Error, "GitHub: posted https://github.com/o/r/pull/7#issuecomment-") || !strings.Contains(step.Error, "Discord: posted on Discord") {
		t.Fatalf("the step: %+v", step)
	}
	text := step.Output
	link := publishedLinkRE.FindString(text)
	if link == "" || !strings.HasSuffix(text, ": Retry uploads") {
		t.Fatalf("the text: %q", text)
	}
	if len(fakeComments) != 1 || fakeComments[0] != "post #7 by tok-good: "+text {
		t.Fatalf("on GitHub: %q, want the text %q", fakeComments, text)
	}
	var sent []string
	for _, m := range discordSent {
		if strings.HasPrefix(m, "send: ") {
			sent = append(sent, m)
		}
	}
	if len(sent) != 1 || !strings.Contains(sent[0], link) || !strings.Contains(sent[0], text+`"`) {
		t.Fatalf("on Discord: %q, want the same link %s", discordSent, link)
	}

	fakeComments, discordSent = nil, nil
	p.run(t, workflow.PostSettings{Message: "Only here", GitHub: true})
	if len(fakeComments) != 1 || len(discordSent) != 0 {
		t.Errorf("GitHub only: %q and %q", fakeComments, discordSent)
	}
	fakeComments, discordSent = nil, nil
	step = p.run(t, workflow.PostSettings{Message: "Only there", Discord: true})
	if len(fakeComments) != 0 || step.Status != "done" || !strings.Contains(strings.Join(discordSent, "\n"), "Only there") {
		t.Errorf("Discord only: %q and %q (%+v)", fakeComments, discordSent, step)
	}
	// A trial writes into the case file, and posts nothing on GitHub; Discord still gets its message.
	fakeComments, discordSent = nil, nil
	p.run(t, workflow.PostSettings{Message: "Trial text", GitHub: true, Trial: true, Discord: true})
	cs, _ := p.st.CaseByNumber(p.ws.ID, 7)
	if len(fakeComments) != 0 || !strings.Contains(cs.Contents, "Would post on GitHub:") || !strings.Contains(strings.Join(discordSent, "\n"), "Trial text") {
		t.Errorf("trial: %q, %q, case:\n%s", fakeComments, discordSent, cs.Contents)
	}
	// A summary replaces the post's first message.
	discordSent = nil
	p.run(t, workflow.PostSettings{Message: "The summary", Discord: true, Summary: true})
	if !strings.Contains(strings.Join(discordSent, "\n"), "edit /channels/th1/messages/m1") {
		t.Errorf("summary: %q", discordSent)
	}
}

// A private repository's cases stay out of Discord: the Post skips it and says so, and the step is done.
func TestPostSkipsDiscordForAPrivateRepository(t *testing.T) {
	p := newPostEnv(t, "o/secret")
	step := p.run(t, workflow.PostSettings{Message: "Both", GitHub: true, Discord: true})
	if step.Status != "done" || !strings.Contains(step.Error, "Discord: skipped, a private repository's cases stay out of Discord") ||
		!strings.Contains(step.Error, "GitHub: posted") {
		t.Fatalf("the step: %+v", step)
	}
	if len(fakeComments) != 1 || len(discordSent) != 0 {
		t.Errorf("posted %q and %q", fakeComments, discordSent)
	}
	// Discord alone, for a private repository: nothing is posted, and it is not a failure.
	fakeComments = nil
	step = p.run(t, workflow.PostSettings{Message: "Only Discord", Discord: true})
	if step.Status != "done" || len(fakeComments) != 0 || len(discordSent) != 0 || !strings.Contains(step.Error, "skipped") {
		t.Errorf("Discord only: %+v", step)
	}
}

// When one destination fails the other is still tried, and the step fails and reports both.
func TestPostTriesTheOtherWhenOneFails(t *testing.T) {
	p := newPostEnv(t, "o/r")
	step := p.run(t, workflow.PostSettings{Message: "FAILPOST this", GitHub: true, Discord: true})
	if step.Status != "failed" || !strings.Contains(step.Error, "GitHub: GitHub refused the comment") || !strings.Contains(step.Error, "Discord: posted on Discord") {
		t.Fatalf("GitHub fails: %+v", step)
	}
	if !strings.Contains(strings.Join(discordSent, "\n"), "FAILPOST this") {
		t.Errorf("Discord was not tried: %q", discordSent)
	}
	// And the other way round: Discord cannot be reached, GitHub's comment is posted all the same.
	p.st.SetDiscordBot(p.ws, "bad-token", hex.EncodeToString(make([]byte, 32)), p.admin)
	fakeComments = nil
	step = p.run(t, workflow.PostSettings{Message: "Hello", GitHub: true, Discord: true})
	if step.Status != "failed" || len(fakeComments) != 1 || !strings.Contains(step.Error, "GitHub: posted") || !strings.Contains(step.Error, "Discord: ") {
		t.Errorf("Discord fails: %+v, posted %q", step, fakeComments)
	}
}

// The form needs a message and a tick; the palette offers Post alone; old GitHub Comment and Post to
// Discord blocks open as Posts with the tick they had, and run.
func TestPostFormPaletteAndOldBlocks(t *testing.T) {
	p := newPostEnv(t, "o/r")
	// Saved graph from before: the two old types, with a setting that is gone.
	old := `{"nodes":[{"id":"ev","type":"pr_ready","x":0,"y":0},
		{"id":"gh","type":"github_comment","x":1,"y":1,"settings":{"message":"Old comment on {pr:title}","trial":false,"keep_one":true}},
		{"id":"dc","type":"discord_post","x":2,"y":2,"name":"Tell them","settings":{"message":"Old message","summary":false}}],
		"links":[{"from":"ev","out":"out","to":"gh","in":"in"},{"from":"ev","out":"out","to":"dc","in":"in"}]}`
	if _, err := p.st.SaveWorkflow(p.ws.ID, workflow.PR, 0, old, p.admin); err != nil {
		t.Fatal(err)
	}
	b := p.signIn("derek")
	_, page := b.get("/w/o/r/workflows/pr")
	page = html.UnescapeString(page)
	if !strings.Contains(page, `"label":"Post"`) || strings.Contains(page, `"label":"GitHub Comment"`) || strings.Contains(page, `"label":"Post to Discord"`) {
		t.Errorf("the palette:\n%s", page)
	}
	if !strings.Contains(page, `"type":"post"`) || strings.Contains(page, `"type":"github_comment"`) || strings.Contains(page, `"type":"discord_post"`) {
		t.Errorf("the old types are on the canvas:\n%s", page)
	}
	for id, want := range map[string][]string{
		"gh": {`name="github" checked`, `name="trial"`, "Old comment on {pr:title}"},
		"dc": {`name="discord" checked`, `value="Tell them"`, "Old message"},
	} {
		_, form := b.get("/w/o/r/workflows/pr/nodes/" + id)
		for _, w := range want {
			if !strings.Contains(form, w) {
				t.Errorf("the form of %s lacks %s:\n%s", id, w, form)
			}
		}
		if strings.Contains(form, "keep_one") || strings.Contains(strings.ToLower(form), "keep one") {
			t.Errorf("the form of %s has Keep one comment", id)
		}
	}
	if _, form := b.get("/w/o/r/workflows/pr/nodes/gh"); strings.Contains(form, `name="discord" checked`) || strings.Contains(form, `name="trial" checked`) {
		t.Errorf("the old comment block has ticks it did not have:\n%s", form)
	}
	steps := runOnce(t, p.env, p.ws, p.admin, false)
	if steps["gh"].Status != "done" || steps["dc"].Status != "done" || len(fakeComments) != 1 || !strings.Contains(fakeComments[0], "Old comment on Retry uploads") ||
		!strings.Contains(strings.Join(discordSent, "\n"), "Old message") {
		t.Fatalf("the old blocks: %+v %+v, posted %q, %q", steps["gh"], steps["dc"], fakeComments, discordSent)
	}
	// Saving one stores the new type.
	w, _ := p.st.Workflow(p.ws.ID, workflow.PR)
	if code, _, out := b.post("/w/o/r/workflows/pr/nodes/gh", url.Values{"csrf": {b.csrf()}, "version": {itoa(int64(w.Version))}, "name": {"Review"}, "message": {"New text"}, "github": {"on"}, "discord": {"on"}}); code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	w, _ = p.st.Workflow(p.ws.ID, workflow.PR)
	if strings.Contains(w.Graph, "github_comment") || strings.Contains(w.Graph, "discord_post") || !strings.Contains(w.Graph, `"github":true,"trial":false,"discord":true`) {
		t.Errorf("the stored graph:\n%s", w.Graph)
	}
	// A Post needs a message and a tick.
	for _, c := range []struct {
		form url.Values
		want string
	}{
		{url.Values{"name": {"Say"}, "message": {"Hello"}}, "tick where to post"},
		{url.Values{"name": {"Say"}, "message": {""}, "github": {"on"}}, "write the message"},
		{url.Values{"name": {"Say"}, "message": {"Hello"}, "trial": {"on"}, "summary": {"on"}}, "tick where to post"},
		{url.Values{"name": {"Say"}, "message": {"Hi {nosuch}"}, "github": {"on"}}, "{nosuch}: no block is called Nosuch"},
	} {
		form, want := c.form, c.want
		w, _ := p.st.Workflow(p.ws.ID, workflow.PR)
		form.Set("csrf", b.csrf())
		form.Set("version", itoa(int64(w.Version)))
		if code, _, out := b.post("/w/o/r/workflows/pr/nodes/gh", form); code != 400 || !strings.Contains(out, want) {
			t.Errorf("%v: %d, want %q:\n%s", form, code, want, out)
		}
	}
}

// Post once per item: a trial run does not consume the shot, the first real run posts, later real
// runs skip without posting while still continuing down "out", and dry runs still run.
func TestPostOncePerItem(t *testing.T) {
	p := newPostEnv(t, "o/r")

	// Trial-only run does not count as posted.
	step := p.run(t, workflow.PostSettings{Message: "Intake", GitHub: true, Trial: true, Once: true})
	if step.Status != "done" || len(fakeComments) != 0 {
		t.Fatalf("trial run: %+v, comments=%v", step, fakeComments)
	}

	// First real run posts to both GitHub and Discord.
	fakeComments, discordSent = nil, nil
	step = p.run(t, workflow.PostSettings{Message: "Intake", GitHub: true, Discord: true, Once: true})
	if step.Status != "done" || step.Port != "out" || len(fakeComments) != 1 || len(discordSent) == 0 {
		t.Fatalf("first real run: %+v, gh=%v, dc=%v", step, fakeComments, discordSent)
	}

	// Second real run with Once=true skips posting and still passes "out".
	fakeComments, discordSent = nil, nil
	step = p.run(t, workflow.PostSettings{Message: "Intake", GitHub: true, Discord: true, Once: true})
	if step.Status != "done" || step.Port != "out" || !strings.Contains(step.Error, "already posted on this item") ||
		len(fakeComments) != 0 || len(discordSent) != 0 {
		t.Fatalf("second real run should skip: %+v, gh=%v, dc=%v", step, fakeComments, discordSent)
	}

	// Dry run still runs even after a real post.
	fakeComments, discordSent = nil, nil
	dryStep := runOnce(t, p.env, p.ws, p.admin, true)["po"]
	if dryStep.Status != "done" || !strings.Contains(dryStep.Error, "dry run") || len(fakeComments) != 0 {
		t.Fatalf("dry run: %+v, gh=%v", dryStep, fakeComments)
	}

	// Form saves and re-renders the "once" checkbox, and card summary shows "once".
	b := p.signIn("derek")
	w, _ := p.st.Workflow(p.ws.ID, workflow.PR)
	if code, _, out := b.post("/w/o/r/workflows/pr/nodes/po", url.Values{
		"csrf": {b.csrf()}, "version": {itoa(int64(w.Version))}, "name": {"Intake"},
		"message": {"Opened"}, "github": {"on"}, "once": {"on"},
	}); code != 200 {
		t.Fatalf("save once: %d %s", code, out)
	}
	if _, form := b.get("/w/o/r/workflows/pr/nodes/po"); !strings.Contains(form, `name="once" checked`) {
		t.Fatalf("form missing once checked:\n%s", form)
	}
	if got := (workflow.PostSettings{GitHub: true, Once: true}).Where(); got != "GitHub · once" {
		t.Fatalf("Where() = %q, want %q", got, "GitHub · once")
	}
}
