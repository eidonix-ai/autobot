package web

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eidonix-ai/autobot/internal/agent"
	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// slowLLM is a provider whose answers stop coming: from request number blockFrom on, a request waits
// until it is cancelled (and says so on entered); the ones before it finish the agent at once.
type slowLLM struct {
	*httptest.Server
	mu        sync.Mutex
	n         int
	blockFrom int
	entered   chan int
}

func newSlowLLM(t *testing.T, blockFrom int) *slowLLM {
	l := &slowLLM{blockFrom: blockFrom, entered: make(chan int, 20)}
	l.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		l.mu.Lock()
		l.n++
		n := l.n
		l.mu.Unlock()
		if n >= l.blockFrom {
			l.entered <- n
			<-r.Context().Done()
			return
		}
		tc := agent.ToolCall{ID: fmt.Sprint("f", n), Type: "function"}
		tc.Function.Name, tc.Function.Arguments = "finish", `{"status":"met","report":"Report."}`
		reply, _ := json.Marshal(agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{tc}})
		fmt.Fprintf(w, `{"choices":[{"message":%s}],"usage":{"prompt_tokens":10,"completion_tokens":1}}`, reply)
	}))
	t.Cleanup(l.Close)
	return l
}

func (l *slowLLM) requests() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.n
}

// wait waits for a request to be blocked.
func (l *slowLLM) wait(t *testing.T) {
	t.Helper()
	select {
	case <-l.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the model was never asked")
	}
}

// slowEnv is a workspace whose provider "slow" never answers its first request.
type slowEnv struct {
	*contEnv
	slow     *slowLLM
	slowProv *store.Provider
}

func newSlowEnv(t *testing.T, blockFrom int) *slowEnv {
	c := newContEnv(t, nil)
	slow := newSlowLLM(t, blockFrom)
	p, err := c.st.CreateProvider(store.ProviderEdit{Name: "Slow", Kind: store.KindOpenAI, Endpoint: slow.URL + "/v1", Key: "k"}, c.admin)
	if err != nil {
		t.Fatal(err)
	}
	c.st.SetProviderModels(p.ID, []string{"m"})
	c.st.SetProviderRoutes(p.ID, true, false)
	c.st.LendProvider(p, []int64{c.ws.ID}, c.admin)
	return &slowEnv{c, slow, p}
}

// slowAgent is an Agent block on the slow provider.
func (e *slowEnv) slowAgent(id, name string, change func(*workflow.AgentSettings)) workflow.Node {
	return e.agentNode(id, name, func(a *workflow.AgentSettings) {
		a.Model = workflow.ModelRef{Provider: e.slowProv.ID, Model: "m"}
		if change != nil {
			change(a)
		}
	})
}

func rawJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

// after are the blocks that must never run once a run is stopped: a case entry and a real GitHub comment.
func after() []workflow.Node {
	post := workflow.DefaultPost()
	post.Trial, post.Message = false, "Posted after the agent."
	return []workflow.Node{
		{ID: "ce", Type: workflow.CaseEntry, Name: "After", Settings: rawJSON(workflow.CaseEntrySettings{Entry: "After the agent.", Finalize: true})},
		{ID: "cm", Type: workflow.Post, Name: "Comment", Settings: rawJSON(post)},
	}
}

// startTest starts an agent block as a test from the canvas does, on pull request 1.
func (c *contEnv) startTest(n workflow.Node) int64 {
	c.t.Helper()
	a, err := c.ui.agentSettings(n, workflow.PR)
	if err != nil {
		c.t.Fatal(err)
	}
	id, err := c.ui.startAgent(context.Background(), c.ws, workflow.PR, n, a, map[string]string{"pr": "https://github.com/o/r/pull/1"}, c.admin, true, 1)
	if err != nil {
		c.t.Fatal(err)
	}
	return id
}

func (c *contEnv) stopRun(id int64) (int, time.Duration) {
	c.t.Helper()
	start := time.Now()
	code, _, _ := c.b.post("/w/o/r/workflow-runs/"+itoa(id)+"/stop", url.Values{"csrf": {c.csrf}})
	return code, time.Since(start)
}

func (c *contEnv) noRunsInMemory() {
	c.t.Helper()
	waitFor(c.t, "the runs to leave memory", func() bool {
		c.ui.running.mu.Lock()
		defer c.ui.running.mu.Unlock()
		return len(c.ui.running.m) == 0
	})
}

// Stopping a run mid-agent cancels the agent at once (its model never answers: only the stop frees
// it), ends the run as stopped by the person, and starts nothing after it. [M]
func TestStopCancelsAgentAndStartsNothing(t *testing.T) {
	fakeComments = nil
	e := newSlowEnv(t, 1)
	e.saveGraph(append([]workflow.Node{events[0], e.slowAgent("ag", "Slow review", nil)}, after()...),
		[2]string{"ev1", "ag"}, [2]string{"ag", "ce"}, [2]string{"ag", "cm"})
	id := e.start("ev1")
	e.slow.wait(t)
	steps, _ := e.st.WorkflowSteps(id)
	if steps["ag"].Status != "running" || steps["ag"].AgentRun == 0 {
		t.Fatalf("the agent's step while it runs: %+v", steps["ag"])
	}
	agentRun := steps["ag"].AgentRun

	code, took := e.stopRun(id)
	if code != http.StatusSeeOther || took > 5*time.Second {
		t.Fatalf("stop: %d after %v", code, took)
	}
	run, _ := e.st.WorkflowRun(id)
	if run.Status != "stopped" || run.StoppedBy != "derek" || time.Since(run.StoppedAt) > time.Minute || run.FinishedAt.IsZero() {
		t.Fatalf("the run: %+v", run)
	}
	steps, _ = e.st.WorkflowSteps(id)
	if steps["ag"].Status != "stopped" || steps["ce"].Status != "pending" || steps["cm"].Status != "pending" {
		t.Fatalf("steps after the stop: %+v", steps)
	}
	ar, _ := e.st.AgentRun(agentRun)
	if ar.Status != "stopped" || ar.StoppedBy != "derek" {
		t.Fatalf("the agent run: %+v", ar)
	}
	e.noRunsInMemory()
	// Nothing was posted or written: no comment, no case file, no tag.
	if len(fakeComments) != 0 {
		t.Fatalf("posted after the stop: %q", fakeComments)
	}
	if c, err := e.st.CaseByNumber(e.ws.ID, 1); err == nil {
		t.Fatalf("the stopped run wrote a case file:\n%s", c.Contents)
	}
	time.Sleep(200 * time.Millisecond) // anything late would have come by now
	if run, _ = e.st.WorkflowRun(id); run.Status != "stopped" || len(fakeComments) != 0 {
		t.Fatalf("after a moment: %+v, posted %q", run, fakeComments)
	}
	// A second stop changes nothing.
	if code, _ := e.stopRun(id); code != http.StatusSeeOther {
		t.Fatalf("a second stop: %d", code)
	}
	if again, _ := e.st.WorkflowRun(id); !again.StoppedAt.Equal(run.StoppedAt) {
		t.Fatal("a second stop moved the time")
	}
}

// The page offers Stop only while the run runs, and afterwards says who stopped it and when.
func TestStopButtonOnRunPage(t *testing.T) {
	e := newSlowEnv(t, 1)
	e.saveGraph([]workflow.Node{events[0], e.slowAgent("ag", "Slow review", nil)}, [2]string{"ev1", "ag"})
	id := e.start("ev1")
	e.slow.wait(t)
	page := "/w/o/r/workflow-runs/" + itoa(id)
	_, body := e.b.get(page)
	if !strings.Contains(body, `action="/w/o/r/workflow-runs/`+itoa(id)+`/stop"`) || !strings.Contains(body, ">Stop</button>") {
		t.Fatalf("no Stop on a running run:\n%s", body)
	}
	if code, _, _ := e.b.post(page+"/stop", url.Values{"csrf": {"wrong"}}); code == http.StatusSeeOther {
		t.Fatal("a stop without the form token was taken")
	}
	// Someone who cannot see the run cannot stop it.
	e.user("mallory", false)
	m := e.signIn("mallory")
	if code, _, _ := m.post(page+"/stop", url.Values{"csrf": {m.csrf()}}); code != http.StatusNotFound {
		t.Fatalf("a non-member's stop: %d", code)
	}
	if r, _ := e.st.WorkflowRun(id); r.Status != "running" {
		t.Fatalf("a non-member stopped the run: %s", r.Status)
	}
	e.stopRun(id)
	_, body = e.b.get(page)
	if strings.Contains(body, "/stop\"") || strings.Contains(body, ">Stop</button>") {
		t.Fatalf("Stop on a stopped run:\n%s", body)
	}
	if !strings.Contains(body, "Stopped") || !strings.Contains(body, "stopped by derek") {
		t.Fatalf("the page does not say who stopped it:\n%s", body)
	}
	r, _ := e.st.WorkflowRun(id)
	if !strings.Contains(body, r.StoppedAt.In(eastern).Format("Jan 2, 2006 3:04 PM")+" ET") {
		t.Fatalf("the page does not say when:\n%s", body)
	}
}

// A canvas test's agent run has its own Stop; a workflow step's agent run stops the whole workflow run.
func TestStopAgentRunPage(t *testing.T) {
	e := newSlowEnv(t, 1)
	a := e.slowAgent("ag", "Slow review", nil)
	id := e.startTest(a)
	e.slow.wait(t)
	page := "/w/o/r/agent-runs/" + itoa(id)
	_, body := e.b.get(page)
	if !strings.Contains(body, page+`/stop"`) || strings.Contains(body, "whole workflow run") {
		t.Fatalf("a canvas test's page:\n%s", body)
	}
	start := time.Now()
	if code, _, _ := e.b.post(page+"/stop", url.Values{"csrf": {e.csrf}}); code != http.StatusSeeOther || time.Since(start) > 5*time.Second {
		t.Fatalf("stop: %d after %v", code, time.Since(start))
	}
	run, _ := e.st.AgentRun(id)
	if run.Status != "stopped" || run.StoppedBy != "derek" || run.FinishedAt.IsZero() {
		t.Fatalf("the agent run: %+v", run)
	}
	_, body = e.b.get(page)
	if strings.Contains(body, page+`/stop"`) || !strings.Contains(body, "Stopped by derek") {
		t.Fatalf("the page afterwards:\n%s", body)
	}
	e.noRunsInMemory()

	// A step of a workflow run: its Stop stops the workflow run.
	e.saveGraph([]workflow.Node{events[0], e.slowAgent("ag", "Slow review", nil)}, [2]string{"ev1", "ag"})
	wid := e.start("ev1")
	e.slow.wait(t)
	var step store.WorkflowStep
	waitFor(t, "the step's agent run", func() bool { s, _ := e.st.WorkflowSteps(wid); step = s["ag"]; return step.AgentRun != 0 })
	page = "/w/o/r/agent-runs/" + itoa(step.AgentRun)
	if _, body = e.b.get(page); !strings.Contains(body, "whole workflow run") {
		t.Fatalf("a step's page:\n%s", body)
	}
	if code, _, _ := e.b.post(page+"/stop", url.Values{"csrf": {e.csrf}}); code != http.StatusSeeOther {
		t.Fatalf("stop: %d", code)
	}
	if r, _ := e.st.WorkflowRun(wid); r.Status != "stopped" || r.StoppedBy != "derek" {
		t.Fatalf("the workflow run: %+v", r)
	}
	if ar, _ := e.st.AgentRun(step.AgentRun); ar.Status != "stopped" {
		t.Fatalf("the agent run: %+v", ar)
	}
}

// A stop during the closing step (the research is over and the model has stopped answering the save
// turn) ends the run: no report is written.
func TestStopDuringClosing(t *testing.T) {
	e := newSlowEnv(t, 2)
	a := e.slowAgent("ag", "Slow review", func(a *workflow.AgentSettings) { a.Closing = workflow.ClosingReport })
	id := e.startTest(a)
	e.slow.wait(t) // the second request: the save turn
	start := time.Now()
	if code, _, _ := e.b.post("/w/o/r/agent-runs/"+itoa(id)+"/stop", url.Values{"csrf": {e.csrf}}); code != http.StatusSeeOther || time.Since(start) > 5*time.Second {
		t.Fatalf("stop: %d after %v", code, time.Since(start))
	}
	if run, _ := e.st.AgentRun(id); run.Status != "stopped" {
		t.Fatalf("the agent run: %+v", run)
	}
	e.noRunsInMemory()
	if n := e.slow.requests(); n != 2 {
		t.Fatalf("the model was asked %d times: a report was started after the stop", n)
	}
}

// holdingGitHub is a GitHub whose comment call does not come back until released, whatever its context
// says: a step that finishes after a stop.
type holdingGitHub struct {
	GitHub
	entered, release chan struct{}
}

func (h *holdingGitHub) Comment(ctx context.Context, token, name string, n int, body string) (int64, string, error) {
	h.entered <- struct{}{}
	<-h.release
	return h.GitHub.Comment(ctx, token, name, n, body)
}

// A step that is already past the point of cancelling, and ends after the stop, is not recorded as
// done, and what hangs on it does not start. [M]
func TestStopStepEndsAfterStop(t *testing.T) {
	fakeComments = nil
	old := stopWait
	stopWait = 200 * time.Millisecond
	t.Cleanup(func() { stopWait = old })
	c := newContEnv(t, nil)
	hold := &holdingGitHub{GitHub: c.ui.gh, entered: make(chan struct{}, 1), release: make(chan struct{})}
	c.ui.gh = hold
	post := workflow.DefaultPost()
	post.Trial, post.Message = false, "In flight."
	c.saveGraph([]workflow.Node{events[0], {ID: "po", Type: workflow.Post, Name: "In flight", Settings: rawJSON(post)},
		{ID: "ce", Type: workflow.CaseEntry, Name: "After", Settings: rawJSON(workflow.CaseEntrySettings{Entry: "After it.", Finalize: true})}},
		[2]string{"ev1", "po"}, [2]string{"po", "ce"})
	id := c.start("ev1")
	<-hold.entered
	if code, _ := c.stopRun(id); code != http.StatusSeeOther {
		t.Fatalf("stop: %d", code)
	}
	if r, _ := c.st.WorkflowRun(id); r.Status != "stopped" {
		t.Fatalf("the run: %+v", r)
	}
	close(hold.release) // the comment call returns, after the stop
	c.noRunsInMemory()
	steps, _ := c.st.WorkflowSteps(id)
	if steps["po"].Status != "stopped" || steps["ce"].Status != "pending" {
		t.Fatalf("steps: %+v", steps)
	}
	if cf, err := c.st.CaseByNumber(c.ws.ID, 1); err == nil {
		t.Fatalf("a case file was written after the stop:\n%s", cf.Contents)
	}
	if r, _ := c.st.WorkflowRun(id); r.Status != "stopped" {
		t.Fatalf("the run ended as %s", r.Status)
	}
}

// A run stopped before it starts any step starts none. [M]
func TestStoppedRunStartsNothing(t *testing.T) {
	c := newContEnv(t, nil)
	c.saveGraph([]workflow.Node{events[0], {ID: "ce", Type: workflow.CaseEntry, Name: "After", Settings: rawJSON(workflow.CaseEntrySettings{Entry: "Should not run.", Finalize: true})}},
		[2]string{"ev1", "ce"})
	w, _ := c.st.Workflow(c.ws.ID, workflow.PR)
	id, err := c.st.StartWorkflowRun(store.WorkflowRun{WorkspaceID: c.ws.ID, Kind: workflow.PR, EventNode: "ev1", Item: 1, Graph: w.Graph}, []string{"ev1", "ce"}, c.admin)
	if err != nil {
		t.Fatal(err)
	}
	run, _ := c.st.WorkflowRun(id)
	it, err := c.ui.loadItem(t.Context(), c.ws, workflow.PR, 1)
	if err != nil {
		t.Fatal(err)
	}
	var g workflow.Graph
	json.Unmarshal([]byte(w.Graph), &g)
	rs := &runState{ws: c.ws, run: run, g: g, it: it, builtIn: it.values(c.ws), tags: map[string]string{}}
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(&agent.StopError{By: "derek"})
	rs.ctx = ctx
	c.ui.drive(rs)
	steps, _ := c.st.WorkflowSteps(id)
	if steps["ce"].Status != "pending" {
		t.Fatalf("a step started in a stopped run: %+v", steps)
	}
	if _, err := c.st.CaseByNumber(c.ws.ID, 1); err == nil {
		t.Fatal("a stopped run wrote a case file")
	}
	if r, _ := c.st.WorkflowRun(id); r.Status != "stopped" || r.StoppedBy != "derek" {
		t.Fatalf("the run: %+v", r)
	}
}

// What a stopped run would write, post or keep, it does not. Each rule is tried on its own: a stop
// that comes while a step is working must still stop that step's last act. [M]
func TestStoppedRunWritesNothing(t *testing.T) {
	fakeDiscord(t)
	fakeComments = nil
	c := newContEnv(t, nil)
	pub, _, _ := ed25519.GenerateKey(nil)
	c.st.SetDiscordBot(c.ws, "good-token", hex.EncodeToString(pub), c.admin)
	c.st.SetDiscordForum(c.ws, "f1", "flare-autobot", c.admin)
	ag := c.agentNode("ag", "Review", nil)
	ce := workflow.Node{ID: "ce", Type: workflow.CaseEntry, Name: "Entry", Settings: rawJSON(workflow.CaseEntrySettings{Entry: "x", Finalize: true})}
	g := workflow.Graph{Nodes: []workflow.Node{events[0], ag, ce}, Links: []workflow.Link{{From: "ev1", Out: "out", To: "ag", In: "in"}}}
	it, err := c.ui.loadItem(t.Context(), c.ws, workflow.PR, 1)
	if err != nil {
		t.Fatal(err)
	}
	rs := &runState{ws: c.ws, run: &store.WorkflowRun{ID: 1, WorkspaceID: c.ws.ID, Kind: workflow.PR}, g: g, it: it, builtIn: it.values(c.ws), tags: map[string]string{}}
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(&agent.StopError{By: "derek"})
	rs.ctx = ctx

	post := workflow.DefaultPost()
	post.GitHub, post.Discord, post.Trial = true, true, false
	both := func() { // every way of posting
		c.ui.postStep(t.Context(), rs, ce, post, "text")
	}
	checks := map[string]func(){
		"a post to GitHub and Discord": both,
		"a GitHub comment":             func() { c.ui.commentStep(t.Context(), rs, ce, false, "text") },
		"a trial comment":              func() { c.ui.commentStep(t.Context(), rs, ce, true, "text") },
		"a Discord post":               func() { c.ui.discordStep(t.Context(), rs, false, "text") },
		"a case entry":                 func() { c.ui.writeEntry(rs, ce, "text", true) },
		"a kept tag":                   func() { c.ui.keepTag(rs, ce, "value", nil) },
		"an agent's entry": func() {
			c.ui.writeResult(rs, ag, stepOutcome{port: "out", agentRun: 1, tags: map[string]string{".summary": "S", ".report": "R"}})
		},
	}
	for what, do := range checks {
		discordSent, fakeComments = nil, nil
		do()
		if len(fakeComments) != 0 || len(discordSent) != 0 {
			t.Errorf("%s of a stopped run posted: %q %q", what, fakeComments, discordSent)
		}
		if cf, err := c.st.CaseByNumber(c.ws.ID, 1); err == nil {
			t.Fatalf("%s of a stopped run wrote a case file:\n%s", what, cf.Contents)
		}
	}
}

// A request waiting for a person is closed when the run is stopped: its Discord message says so and
// loses its buttons, and a click on them afterwards (on Discord or on the page) is refused. [M]
func TestStopClosesApproval(t *testing.T) {
	fakeDiscord(t)
	e := newEnv(t)
	derek := e.user("derek", true)
	alice := e.user("alice", false)
	ws, _ := e.st.CreateWorkspace("o/r", derek, "", "")
	e.st.SetMembers(ws.ID, []int64{alice.ID}, derek)
	pub, priv, _ := ed25519.GenerateKey(nil)
	e.st.SetDiscordBot(ws, "good-token", hex.EncodeToString(pub), derek)
	e.st.SetDiscordForum(ws, "f1", "flare-autobot", derek)
	approvalWorkflow(t, e, ws, derek)
	id, err := e.ui.startWorkflow(t.Context(), ws, workflow.PR, "ev", 7, false, derek)
	if err != nil {
		t.Fatal(err)
	}
	var ap *store.Approval
	waitFor(t, "the request on Discord", func() bool { ap, _ = e.st.ApprovalFor(id, "ap"); return ap != nil && ap.Message != "" })
	a := e.signIn("alice")
	if code, _, _ := a.post("/w/o/r/workflow-runs/"+itoa(id)+"/stop", url.Values{"csrf": {a.csrf()}}); code != http.StatusSeeOther {
		t.Fatalf("stop: %d", code)
	}
	ap, _ = e.st.Approval(ap.ID)
	if ap.Status != "stopped" || ap.DecidedBy != "alice" {
		t.Fatalf("the approval: %+v", ap)
	}
	steps, _ := e.st.WorkflowSteps(id)
	if steps["ap"].Status != "stopped" || steps["ya"].Status != "pending" || steps["no"].Status != "pending" {
		t.Fatalf("steps: %+v", steps)
	}
	edited := ""
	for _, s := range discordSent {
		if strings.HasPrefix(s, "edit ") {
			edited = s
		}
	}
	if !strings.Contains(edited, "The run was stopped") || !strings.Contains(edited, "alice") || strings.Contains(edited, "custom_id") {
		t.Fatalf("Discord was told %q", discordSent)
	}
	// Late answers, on Discord and on the page, change nothing.
	body, _ := json.Marshal(map[string]any{"type": 3, "data": map[string]any{"custom_id": "ap:" + itoa(ap.ID) + ":approved"},
		"member": map[string]any{"user": map[string]any{"id": "u1", "username": "festr", "global_name": "Festr"}}})
	req, _ := http.NewRequest("POST", e.srv.URL+"/discord/interactions/o/r", bytes.NewReader(body))
	req.Header.Set("X-Signature-Timestamp", "1")
	req.Header.Set("X-Signature-Ed25519", hex.EncodeToString(ed25519.Sign(priv, append([]byte("1"), body...))))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	reply, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(reply), "Already answered: the run was stopped by alice") {
		t.Fatalf("a click after the stop: %s", reply)
	}
	d := e.signIn("derek")
	d.post("/w/o/r/workflow-runs/"+itoa(id)+"/approvals/"+itoa(ap.ID), url.Values{"csrf": {d.csrf()}, "answer": {"approved"}})
	ap, _ = e.st.Approval(ap.ID)
	steps, _ = e.st.WorkflowSteps(id)
	if ap.Status != "stopped" || steps["ya"].Status != "pending" {
		t.Fatalf("a late answer was taken: %+v %+v", ap, steps)
	}
	// The case file was opened by the request's Discord post; nothing answers it there.
	if c, err := e.st.CaseByNumber(ws.ID, 7); err != nil || strings.Contains(c.Contents, "Approved") || strings.Contains(c.Contents, "Running it") {
		t.Fatalf("the case file after the stop: %v\n%v", err, c)
	}
}

// A run that is not in memory (Autobot restarted since) is marked stopped in the database, with its
// steps, its open approval and its agent run.
func TestStopRunNotInMemory(t *testing.T) {
	e := newEnv(t)
	derek := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", derek, "", "")
	approvalWorkflow(t, e, ws, derek)
	w, _ := e.st.Workflow(ws.ID, workflow.PR)
	id, _ := e.st.StartWorkflowRun(store.WorkflowRun{WorkspaceID: ws.ID, Kind: workflow.PR, EventNode: "ev", Item: 7, Graph: w.Graph}, []string{"ev", "ap", "ya", "no"}, derek)
	e.st.SetWorkflowStep(id, store.WorkflowStep{NodeID: "ev", Status: "done", Port: "out"})
	e.st.SetWorkflowStep(id, store.WorkflowStep{NodeID: "ap", Status: "waiting", StartedAt: time.Now()})
	ap, _ := e.st.OpenApproval(id, "ap", "Run it?", time.Now().Add(time.Hour))
	ar, _, _, _ := e.st.StartAgentRun(store.AgentRun{WorkspaceID: ws.ID, Kind: workflow.PR, NodeID: "x", NodeName: "x", Item: 7, Test: true, Model: "m"}, derek)

	b := e.signIn("derek")
	if code, _, _ := b.post("/w/o/r/workflow-runs/"+itoa(id)+"/stop", url.Values{"csrf": {b.csrf()}}); code != http.StatusSeeOther {
		t.Fatalf("stop: %d", code)
	}
	run, _ := e.st.WorkflowRun(id)
	steps, _ := e.st.WorkflowSteps(id)
	ap, _ = e.st.Approval(ap.ID)
	if run.Status != "stopped" || run.StoppedBy != "derek" || steps["ap"].Status != "stopped" || steps["ev"].Status != "done" || steps["ya"].Status != "pending" || ap.Status != "stopped" {
		t.Fatalf("%+v %+v %+v", run, steps, ap)
	}
	if code, _, _ := b.post("/w/o/r/agent-runs/"+itoa(ar)+"/stop", url.Values{"csrf": {b.csrf()}}); code != http.StatusSeeOther {
		t.Fatalf("stop agent: %d", code)
	}
	if a, _ := e.st.AgentRun(ar); a.Status != "stopped" || a.StoppedBy != "derek" || a.FinishedAt.IsZero() {
		t.Fatalf("%+v", a)
	}
	if code, _, _ := b.post("/w/o/r/agent-runs/9999/stop", url.Values{"csrf": {b.csrf()}}); code != http.StatusNotFound {
		t.Fatalf("a missing run: %d", code)
	}
}
