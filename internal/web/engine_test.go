package web

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/eidonix-ai/autobot/internal/github"
	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// A run routes by a decision's choice, skips the branch not taken, rejoins at a merge, keeps output
// tags for the case, and writes the case entries it reaches; a dry run writes none of it.
func TestWorkflowRun(t *testing.T) {
	e := newEnv(t)
	derek := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", derek, "", "")
	e.st.CreateWorkspace("o/r2", derek, "", "")
	llm, _ := llmServer(t) // its decisions answer "yes"
	p, _ := e.st.CreateProvider(store.ProviderEdit{Name: "LLM", Kind: store.KindOpenAI, Endpoint: llm.URL + "/v1", Key: "k"}, derek)
	e.st.SetProviderModels(p.ID, []string{"m"})
	e.st.SetProviderRoutes(p.ID, true, false)
	e.st.LendProvider(p, []int64{ws.ID}, derek)
	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	d := workflow.DefaultDecision(workflow.PR)
	d.Model, d.OutputTo = workflow.ModelRef{Provider: p.ID, Model: "m"}, "value_decision"
	entry := func(text string) json.RawMessage { return raw(workflow.CaseEntrySettings{Entry: text, Finalize: true}) }
	g := workflow.Graph{Nodes: []workflow.Node{
		{ID: "ev", Type: "pr_ready"},
		{ID: "de", Type: workflow.Decision, Name: "Deep review?", Settings: raw(d)},
		{ID: "ya", Type: workflow.CaseEntry, Name: "Yes branch", Settings: entry("Going deep on {pr_title}.")},
		{ID: "no", Type: workflow.CaseEntry, Name: "No branch", Settings: entry("Skipping {pr_title}.")},
		{ID: "jn", Type: workflow.CaseEntry, Name: "Join", Settings: entry("Decided {value_decision} after: {previous_output}")},
	}, Links: []workflow.Link{{From: "ev", Out: "out", To: "de", In: "in"}, {From: "de", Out: "yes", To: "ya", In: "in"}, {From: "de", Out: "no", To: "no", In: "in"}, {From: "ya", Out: "out", To: "jn", In: "in"}, {From: "no", Out: "out", To: "jn", In: "in"}}}
	if _, err := e.st.SaveWorkflow(ws.ID, workflow.PR, 0, string(raw(g)), derek); err != nil {
		t.Fatal(err)
	}
	b := e.signIn("derek")
	tok := b.csrf()
	run := func(dry bool) (string, map[string]store.WorkflowStep) {
		t.Helper()
		form := url.Values{"csrf": {tok}, "item": {"7"}}
		if dry {
			form.Set("dry_run", "on")
		}
		code, loc, body := b.post("/w/o/r/workflows/pr/nodes/ev/run", form)
		if code != http.StatusSeeOther {
			t.Fatalf("start: %d\n%s", code, body)
		}
		id := loc[strings.LastIndex(loc, "/")+1:]
		for i := 0; i < 100; i++ {
			r, _ := e.st.WorkflowRun(int64(atoi(id)))
			if r.Status != "running" {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		steps, _ := e.st.WorkflowSteps(int64(atoi(id)))
		return loc, steps
	}

	loc, steps := run(true)
	if steps["de"].Port != "yes" || steps["ya"].Status != "done" || steps["no"].Status != "skipped" || steps["jn"].Status != "done" {
		t.Fatalf("routing: %+v", steps)
	}
	// A branch not taken is how routing works: the join is given only what ran.
	if got := steps["jn"].Output; got != "Decided yes after: Going deep on Retry uploads." {
		t.Fatalf("join output %q", got)
	}
	if _, err := e.st.CaseByNumber(ws.ID, 7); err == nil {
		t.Fatal("a dry run opened a case file")
	}
	// A run belongs to its workspace.
	if code, _ := b.get(strings.Replace(loc, "/w/o/r/", "/w/o/r2/", 1)); code != http.StatusNotFound {
		t.Fatalf("another workspace's run: %d", code)
	}
	if code, body := b.get(loc); code != 200 || !strings.Contains(body, "Yes branch") || !strings.Contains(body, "→ yes") {
		t.Fatalf("run page %d", code)
	}

	run(false)
	c, err := e.st.CaseByNumber(ws.ID, 7)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.Contents, "Going deep on Retry uploads.") || strings.Contains(c.Contents, "Skipping") || !strings.Contains(c.Contents, "Decided yes") {
		t.Fatalf("case file:\n%s", c.Contents)
	}
	if v, _ := e.st.CaseValues(c.UUID); v["value_decision"] != "yes" {
		t.Fatalf("output tags %v", v)
	}
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}

// The event check says why the poller would not have started a run.
func TestVerdict(t *testing.T) {
	ev := workflow.DefaultEvent("pr_ready")
	ev.Mode = "summoned"
	b, _ := json.Marshal(ev)
	n := workflow.Node{ID: "e", Type: "pr_ready", Settings: b}
	it := &item{kind: workflow.PR, number: 7, state: "open", author: "dependabot[bot]", pr: &github.PullRequest{Base: "main"},
		comments: []github.Comment{{Author: "alice", Body: "@autobot review please"}, {Author: "bob", Body: "Autobot: ignore this one"}}}
	author := func(who string) bool { return who == "bob" || who == it.author }
	ok, lines := verdict(n, it, false, "Autobot: ignore", author)
	all := strings.Join(lines, "\n")
	if ok || !strings.Contains(all, "✗ the opt-out tag “Autobot: ignore” is in a comment by bob") || !strings.Contains(all, "is a bot") {
		t.Fatalf("%v\n%s", ok, all)
	}
	it.comments, it.author = it.comments[:1], "alice"
	if ok, lines := verdict(n, it, false, "Autobot: ignore", author); !ok {
		t.Fatalf("should start:\n%s", strings.Join(lines, "\n"))
	}
}
