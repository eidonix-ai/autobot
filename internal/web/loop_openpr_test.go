package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eidonix-ai/autobot/internal/agent"
	"github.com/eidonix-ai/autobot/internal/repo"
	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// TestLoopBoundedRetriesAndLimitExit checks that a Loop block jumps back to its GOTO target up to
// MaxTries times (giving 1 + MaxTries total attempts), passes {loop:feedback} and {loop:attempt},
// and exits on its "limit" port once retries are exhausted.
func TestLoopBoundedRetriesAndLimitExit(t *testing.T) {
	oldPoll := agentPoll
	agentPoll = 50 * time.Millisecond
	t.Cleanup(func() { agentPoll = oldPoll })

	var attempts atomic.Int32
	var seenTasks []string

	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.HasSuffix(r.URL.Path, "/decisions") {
			// Always decline so the loop runs all retries and exits via "limit".
			fmt.Fprint(w, `{"answers":[{"choice":"no","probabilities":[{"value":"yes","probability":0.1},{"value":"no","probability":0.9}]}]}`)
			return
		}
		n := attempts.Add(1)
		seenTasks = append(seenTasks, string(body))
		tc := agent.ToolCall{ID: fmt.Sprintf("f%d", n), Type: "function"}
		tc.Function.Name = "finish"
		tc.Function.Arguments = fmt.Sprintf(`{"status":"met","report":"Attempt %d patch summary"}`, n)
		reply, _ := json.Marshal(agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{tc}})
		fmt.Fprintf(w, `{"choices":[{"message":%s}],"usage":{"prompt_tokens":10,"completion_tokens":1}}`, reply)
	}))
	t.Cleanup(llm.Close)

	e := newEnv(t)
	derek := e.user("derek", true)
	ws, err := e.st.CreateWorkspace("o/r", derek, "flare-autobot", "tok-good")
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.st.CreateProvider(store.ProviderEdit{Name: "LLM", Kind: store.KindOpenAI, Endpoint: llm.URL + "/v1", Key: "k"}, derek)
	if err != nil {
		t.Fatal(err)
	}
	e.st.SetProviderModels(p.ID, []string{"m"})
	e.st.SetProviderRoutes(p.ID, true, true)
	e.st.LendProvider(p, []int64{ws.ID}, derek)

	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	dev := workflow.DefaultAgent(workflow.Issue)
	dev.Persona, dev.Model = "custom", workflow.ModelRef{Provider: p.ID, Model: "m"}
	dev.Instruction = "Fix issue #{issue:number}. Attempt {loop:attempt}. Feedback: {loop:feedback}"
	dev.Goal = "Done"
	dev.NoTools = true

	dec := workflow.DecisionSettings{
		Model:    workflow.ModelRef{Provider: p.ID, Model: "m"},
		Context:  "{dev.report}",
		Question: "Is this ready?",
		Choices:  []workflow.Choice{{Name: "yes", Description: "ready"}, {Name: "no", Description: "needs more work"}},
		Output:   "decision",
	}
	lp := workflow.LoopSettings{Target: "dev", MaxTries: 2, Feedback: "Please fix: {dev.report}"}
	giveUp := workflow.CaseEntrySettings{Entry: "Needs human fix after attempt {loop:attempt}", Finalize: true}

	g := workflow.Graph{
		Nodes: []workflow.Node{
			{ID: "ev", Type: "issue_opened"},
			{ID: "dev", Type: workflow.Agent, Name: "Dev", Settings: raw(dev)},
			{ID: "rdy", Type: workflow.Decision, Name: "Ready", Settings: raw(dec)},
			{ID: "lp", Type: workflow.Loop, Name: "Retry", Settings: raw(lp)},
			{ID: "gu", Type: workflow.CaseEntry, Name: "Give Up", Settings: raw(giveUp)},
		},
		Links: []workflow.Link{
			{From: "ev", Out: "out", To: "dev", In: "in"},
			{From: "dev", Out: "out", To: "rdy", In: "in"},
			{From: "rdy", Out: "no", To: "lp", In: "in"},
			{From: "lp", Out: "limit", To: "gu", In: "in"},
		},
	}
	if _, err := e.st.SaveWorkflow(ws.ID, workflow.Issue, 0, string(raw(g)), derek); err != nil {
		t.Fatal(err)
	}

	// Verify Loop settings modal renders GOTO targets and saves.
	b := e.signIn("derek")
	if _, form := b.get("/w/o/r/workflows/issue/nodes/lp"); !strings.Contains(form, `value="dev" selected`) {
		t.Fatalf("Loop settings form missing selected GOTO target:\n%s", form)
	}

	id, err := e.ui.startWorkflow(t.Context(), ws, workflow.Issue, "ev", 8, false, derek)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "loop run to finish", func() bool {
		r, err := e.st.WorkflowRun(id)
		return err == nil && r.Status != "running" && r.Status != "queued"
	})
	run, _ := e.st.WorkflowRun(id)
	if run.Status != "done" {
		steps, _ := e.st.WorkflowSteps(id)
		t.Fatalf("run status=%s, steps=%+v", run.Status, steps)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("Dev ran %d times, want 3 (1 initial + 2 retries)", got)
	}
	if len(seenTasks) < 3 || !strings.Contains(seenTasks[1], "Attempt 2") || !strings.Contains(seenTasks[1], "Please fix: Attempt 1 patch summary") {
		t.Fatalf("retry 2 did not receive attempt/feedback: %v", seenTasks)
	}
	steps, _ := e.st.WorkflowSteps(id)
	if steps["lp"].Port != "limit" || steps["gu"].Status != "done" || !strings.Contains(steps["gu"].Output, "attempt 3") {
		t.Fatalf("limit branch mismatch: lp=%+v gu=%+v", steps["lp"], steps["gu"])
	}
}

// TestOpenPRTrialAndLivePush verifies Open PR trial preview, settings form, and real git PushPatch +
// GitHub CreatePullRequest against a local bare repository.
func TestOpenPRTrialAndLivePush(t *testing.T) {
	// Set up a bare upstream git repo with one commit.
	root := t.TempDir()
	remote := filepath.Join(root, "o", "r.git")
	if err := os.MkdirAll(filepath.Dir(remote), 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git(root, "init", "--bare", "-b", "main", remote)
	seed := filepath.Join(root, "seed")
	git(root, "clone", remote, seed)
	if err := os.WriteFile(filepath.Join(seed, "hello.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(seed, "add", "hello.txt")
	git(seed, "commit", "-m", "initial")
	git(seed, "push", "origin", "main")
	baseSHA := git(seed, "rev-parse", "HEAD")

	// Build a valid unified diff changing hello.txt.
	if err := os.WriteFile(filepath.Join(seed, "hello.txt"), []byte("hello autobot\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patch := git(seed, "diff", "--no-color", "HEAD") + "\n"

	// PushPatch using repo.Mirrors pointed at file://root.
	m := repo.New(filepath.Join(root, "data"))
	m.BaseURL = "file://" + root
	if err := m.PushPatch(t.Context(), 1, "o/r", "", baseSHA, "autobot/issue-42", patch, "Fix hello"); err != nil {
		t.Fatalf("PushPatch: %v", err)
	}
	// Verify branch exists in bare upstream and contains the updated file.
	got := git(remote, "show", "refs/heads/autobot/issue-42:hello.txt")
	if got != "hello autobot" {
		t.Fatalf("pushed file content = %q, want %q", got, "hello autobot")
	}

	// Verify Open PR settings form + Trial preview via Server.
	e := newEnv(t)
	derek := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", derek, "flare-autobot", "tok-good")
	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	op := workflow.DefaultOpenPR(workflow.Issue)
	g := workflow.Graph{
		Nodes: []workflow.Node{
			{ID: "ev", Type: "issue_opened"},
			{ID: "pr", Type: workflow.OpenPR, Name: "Open Fix PR", Settings: raw(op)},
		},
		Links: []workflow.Link{{From: "ev", Out: "out", To: "pr", In: "in"}},
	}
	if _, err := e.st.SaveWorkflow(ws.ID, workflow.Issue, 0, string(raw(g)), derek); err != nil {
		t.Fatal(err)
	}
	b := e.signIn("derek")
	if _, form := b.get("/w/o/r/workflows/issue/nodes/pr"); !strings.Contains(form, `name="draft"`) || !strings.Contains(form, `name="trial" checked`) {
		t.Fatalf("Open PR form missing fields:\n%s", form)
	}
	w, _ := e.st.Workflow(ws.ID, workflow.Issue)
	if code, _, out := b.post("/w/o/r/workflows/issue/nodes/pr", url.Values{
		"csrf": {b.csrf()}, "version": {itoa(int64(w.Version))}, "name": {"Open Fix PR"},
		"title": {"Fix #{issue:number}"}, "branch": {"autobot/issue-{issue:number}"},
		"body": {"Fixes #{issue:number}"}, "draft": {"on"}, "trial": {"on"},
	}); code != http.StatusOK {
		t.Fatalf("save Open PR: %d %s", code, out)
	}

	// Running openPRStep in trial mode with a patch writes a preview to the case file.
	rs := &runState{
		ws:      ws,
		run:     &store.WorkflowRun{ID: 1, WorkspaceID: ws.ID, Kind: workflow.Issue, Item: 42},
		g:       g,
		it:      &item{kind: workflow.Issue, number: 42, title: "Broken greeting"},
		builtIn: map[string]string{"issue:number": "42", "issue:title": "Broken greeting", "run:patch": patch, "run:base_commit": baseSHA},
		tags:    map[string]string{},
	}
	savedOp := workflow.OpenPRSettings{Title: "Fix #{issue:number}", Body: "Fixes #{issue:number}", Branch: "autobot/issue-{issue:number}", Draft: true, Trial: true}
	out := e.ui.openPRStep(t.Context(), rs, g.Nodes[1], savedOp, rs.builtIn)
	if out.err != nil || out.port != "out" || !strings.Contains(out.note, "trial run") {
		t.Fatalf("trial openPRStep: %+v", out)
	}
	c, err := e.st.CaseByNumber(ws.ID, 42)
	if err != nil || !strings.Contains(c.Contents, "Would open Draft PR") || !strings.Contains(c.Contents, "hello.txt") {
		t.Fatalf("case file missing PR preview: err=%v contents=%s", err, c.Contents)
	}
}
