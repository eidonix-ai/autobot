package web

import (
	"context"
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

// gateLLM is a model that answers only when let: every request waits for a send on release (or for
// the channel to be closed, which lets all of them through), and says so on entered. It counts the
// requests being held at once, and the most it ever held.
type gateLLM struct {
	*httptest.Server
	mu          sync.Mutex
	in, max, n  int
	entered     chan struct{}
	release     chan struct{}
	releaseOnce sync.Once
}

func newGateLLM(t *testing.T) *gateLLM {
	g := &gateLLM{entered: make(chan struct{}, 100), release: make(chan struct{})}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		g.mu.Lock()
		g.in++
		g.n++
		g.max = max(g.max, g.in)
		n := g.n
		g.mu.Unlock()
		g.entered <- struct{}{}
		select {
		case <-g.release:
		case <-r.Context().Done():
		}
		g.mu.Lock()
		g.in--
		g.mu.Unlock()
		tc := agent.ToolCall{ID: fmt.Sprint("f", n), Type: "function"}
		tc.Function.Name, tc.Function.Arguments = "finish", `{"status":"met","report":"Report."}`
		reply, _ := json.Marshal(agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{tc}})
		fmt.Fprintf(w, `{"choices":[{"message":%s}],"usage":{"prompt_tokens":10,"completion_tokens":1}}`, reply)
	}))
	t.Cleanup(func() { g.openAll(); g.Close() })
	return g
}

// openAll lets every request through, now and later.
func (g *gateLLM) openAll() { g.releaseOnce.Do(func() { close(g.release) }) }

// letOne lets one waiting request through.
func (g *gateLLM) letOne(t *testing.T) {
	t.Helper()
	select {
	case g.release <- struct{}{}:
	case <-time.After(10 * time.Second):
		t.Fatal("no request was waiting to be let through")
	}
}

// waitEntered waits for n requests to have reached the model since the last call.
func (g *gateLLM) waitEntered(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-g.entered:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d requests reached the model", i, n)
		}
	}
}

// quiet checks that no more request reaches the model for a moment.
func (g *gateLLM) quiet(t *testing.T, why string) {
	t.Helper()
	select {
	case <-g.entered:
		t.Fatalf("a request reached the model: %s", why)
	case <-time.After(300 * time.Millisecond):
	}
}

func (g *gateLLM) stats() (in, most, total int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.in, g.max, g.n
}

// gateEnv is a workspace whose model waits at a gate, with pull request 1.
type gateEnv struct {
	*contEnv
	gate *gateLLM
	prov *store.Provider
	last int // the last pull request used: every run is on a pull request of its own, as the same block on one item would continue its thread
}

func newGateEnv(t *testing.T, limit int) *gateEnv {
	c := newContEnv(t, nil)
	g := newGateLLM(t)
	p, err := c.st.CreateProvider(store.ProviderEdit{Name: "Gate", Kind: store.KindOpenAI, Endpoint: g.URL + "/v1", Key: "k"}, c.admin)
	if err != nil {
		t.Fatal(err)
	}
	c.st.SetProviderModels(p.ID, []string{"m"})
	c.st.SetProviderRoutes(p.ID, true, false)
	c.st.LendProvider(p, []int64{c.ws.ID}, c.admin)
	if err := c.st.SetMaxRuns(c.ws, limit, c.admin); err != nil {
		t.Fatal(err)
	}
	e := &gateEnv{contEnv: c, gate: g, prov: p}
	// Both event blocks lead to an agent that waits at the gate.
	e.saveGraph([]workflow.Node{events[0], events[1], e.gateAgent("ag", "Gated review")}, [2]string{"ev1", "ag"}, [2]string{"ev2", "ag"})
	return e
}

func (e *gateEnv) gateAgent(id, name string) workflow.Node {
	return e.agentNode(id, name, func(a *workflow.AgentSettings) { a.Model = workflow.ModelRef{Provider: e.prov.ID, Model: "m"} })
}

// item is a pull request no run has used.
func (e *gateEnv) item() int {
	e.last++
	if e.last > 1 { // the environment has number 1
		e.addPR(e.last, false)
	}
	return e.last
}

func (e *gateEnv) startItem(ev string, dry bool) int64 {
	e.t.Helper()
	id, err := e.ui.startWorkflow(e.t.Context(), e.ws, workflow.PR, ev, e.item(), dry, e.admin)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *gateEnv) status(id int64) string {
	r, err := e.st.WorkflowRun(id)
	if err != nil {
		e.t.Fatal(err)
	}
	return r.Status
}

func (e *gateEnv) waitStatus(id int64, want string) {
	e.t.Helper()
	waitFor(e.t, fmt.Sprintf("run %d to be %s", id, want), func() bool { return e.status(id) == want })
}

func (e *gateEnv) statuses(ids ...int64) string {
	var out []string
	for _, id := range ids {
		out = append(out, e.status(id))
	}
	return strings.Join(out, " ")
}

// A workspace never runs more workflow runs than its limit, however many are started (two of the
// four here are dry runs, which count too): the rest are queued, and once the model lets the runs
// finish, every one of them runs. [M]
func TestRunLimitNeverExceeded(t *testing.T) {
	e := newGateEnv(t, 2)
	ids := []int64{e.startItem("ev1", false), e.startItem("ev1", true), e.startItem("ev1", false), e.startItem("ev1", true)}
	e.gate.waitEntered(t, 2)
	e.gate.quiet(t, "a third run started with the limit at 2")
	if got := e.statuses(ids...); got != "running running queued queued" {
		t.Fatalf("with 4 started and a limit of 2: %s", got)
	}
	// The queued runs have not started: their steps are all pending, and they have no agent run.
	if steps, _ := e.st.WorkflowSteps(ids[2]); steps["ag"].Status != "pending" || steps["ev1"].Status != "pending" {
		t.Fatalf("a queued run's steps: %+v", steps)
	}
	e.gate.openAll()
	for _, id := range ids {
		e.waitStatus(id, "done")
	}
	if _, most, total := e.gate.stats(); most != 2 || total != 4 {
		t.Fatalf("the model held at most %d requests at once over %d requests, want 2 over 4", most, total)
	}
	waitFor(t, "the slots to be given back", func() bool {
		e.ui.slots.mu.Lock()
		defer e.ui.slots.mu.Unlock()
		return len(e.ui.slots.held) == 0
	})
}

// Queued runs start oldest first, one for each run that ends. [M]
func TestQueuedRunsStartInOrder(t *testing.T) {
	e := newGateEnv(t, 2)
	r1, r2, r3, r4, r5 := e.startItem("ev1", false), e.startItem("ev1", false), e.startItem("ev1", false), e.startItem("ev1", false), e.startItem("ev1", false)
	e.gate.waitEntered(t, 2)
	if got := e.statuses(r1, r2, r3, r4, r5); got != "running running queued queued queued" {
		t.Fatalf("at the start: %s", got)
	}
	e.gate.letOne(t) // one of the first two ends: the oldest queued run starts, and only that one
	e.gate.waitEntered(t, 1)
	waitFor(t, "the third run to start", func() bool { return e.status(r3) == "running" })
	if got := e.statuses(r3, r4, r5); got != "running queued queued" {
		t.Fatalf("after the first ended: r3 r4 r5 are %s", got)
	}
	e.gate.letOne(t)
	e.gate.waitEntered(t, 1)
	waitFor(t, "the fourth run to start", func() bool { return e.status(r4) == "running" })
	if got := e.statuses(r5); got != "queued" {
		t.Fatalf("after the second ended: r5 is %s", got)
	}
	// Its place in line is on the Runs page.
	if _, body := e.b.get("/w/o/r/workflow-runs"); !strings.Contains(body, "queued (1st in line)") {
		t.Fatalf("the Runs page:\n%s", body)
	}
	if in, most, _ := e.gate.stats(); in > 2 || most > 2 {
		t.Fatalf("the model held %d requests (at most %d)", in, most)
	}
	e.gate.openAll()
	for _, id := range []int64{r1, r2, r3, r4, r5} {
		e.waitStatus(id, "done")
	}
}

// approvalGraph is ev1 into a Human Approval and then a case entry, and ev2 into an agent that waits at the gate.
func (e *gateEnv) approvalGraph() {
	e.t.Helper()
	entry := workflow.CaseEntrySettings{Entry: "Approved.", Finalize: true}
	g := workflow.Graph{Nodes: []workflow.Node{events[0], events[1], e.gateAgent("ag", "Gated review"),
		{ID: "ap", Type: workflow.Approval, Name: "Hardware", Settings: rawJSON(workflow.ApprovalSettings{Request: "Go?", Hours: 1})},
		{ID: "ce", Type: workflow.CaseEntry, Name: "After", Settings: rawJSON(entry)}},
		Links: []workflow.Link{{From: "ev1", Out: "out", To: "ap", In: "in"}, {From: "ap", Out: "approved", To: "ce", In: "in"}, {From: "ev2", Out: "out", To: "ag", In: "in"}}}
	w, _ := e.st.Workflow(e.ws.ID, workflow.PR)
	if _, err := e.st.SaveWorkflow(e.ws.ID, workflow.PR, w.Version, string(rawJSON(g)), e.admin); err != nil {
		e.t.Fatal(err)
	}
}

// A run that is only waiting for an approval does not count: others run meanwhile, even with a limit
// of 1. Once it is answered, the run waits for a slot like any other before its next step.
func TestApprovalWaitFreesItsSlot(t *testing.T) {
	e := newGateEnv(t, 1)
	e.approvalGraph()
	a := e.startItem("ev1", false)
	waitFor(t, "the approval to wait", func() bool { s, _ := e.st.WorkflowSteps(a); return s["ap"].Status == "waiting" })
	waitFor(t, "the waiting run to give its slot up", func() bool { return !e.ui.holdsSlot(a) })

	b := e.startItem("ev2", false) // would be queued if the waiting run still counted
	e.gate.waitEntered(t, 1)
	if got := e.statuses(a, b); got != "running running" {
		t.Fatalf("a run waiting for approval, and another: %s", got)
	}
	e.gate.letOne(t)
	e.waitStatus(b, "done")

	// A third run takes the only slot; the answer to the approval must wait for it.
	c := e.startItem("ev2", false)
	e.gate.waitEntered(t, 1)
	ap, err := e.st.ApprovalFor(a, "ap")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := e.st.Decide(ap.ID, "approved", "derek", "the run page"); err != nil || !ok {
		t.Fatalf("answering: %v %v", ok, err)
	}
	waitFor(t, "the approval step to end", func() bool { s, _ := e.st.WorkflowSteps(a); return s["ap"].Status == "done" })
	time.Sleep(300 * time.Millisecond)
	if s, _ := e.st.WorkflowSteps(a); s["ce"].Status != "pending" || e.status(a) != "running" {
		t.Fatalf("the answered run went on past the limit: %+v", s["ce"])
	}
	e.gate.letOne(t) // the third run ends: the answered run gets the slot
	e.waitStatus(c, "done")
	e.waitStatus(a, "done")
	if s, _ := e.st.WorkflowSteps(a); s["ce"].Status != "done" {
		t.Fatalf("the case entry after the approval: %+v", s["ce"])
	}
}

// A queued run can be stopped: it becomes stopped, never starts, and the runs behind it move up.
func TestStopQueuedRun(t *testing.T) {
	e := newGateEnv(t, 1)
	a, b, c := e.startItem("ev1", false), e.startItem("ev1", false), e.startItem("ev1", false)
	e.gate.waitEntered(t, 1)
	if got := e.statuses(a, b, c); got != "running queued queued" {
		t.Fatalf("at the start: %s", got)
	}
	_, page := e.b.get("/w/o/r/workflow-runs/" + itoa(b))
	if !strings.Contains(page, "Queued (1st in line)") || !strings.Contains(page, `/stop"`) {
		t.Fatalf("a queued run's page:\n%s", page)
	}
	if code, _ := e.stopRun(b); code != http.StatusSeeOther {
		t.Fatalf("stop: %d", code)
	}
	run, _ := e.st.WorkflowRun(b)
	if run.Status != "stopped" || run.StoppedBy != "derek" || run.FinishedAt.IsZero() {
		t.Fatalf("the stopped queued run: %+v", run)
	}
	if _, body := e.b.get("/w/o/r/workflow-runs"); !strings.Contains(body, "queued (1st in line)") || strings.Contains(body, "queued (2nd in line)") {
		t.Fatalf("the run behind did not move up:\n%s", body)
	}
	e.gate.letOne(t) // the first run ends: the third starts, the stopped one never does
	e.gate.waitEntered(t, 1)
	e.waitStatus(a, "done")
	waitFor(t, "the third run to start", func() bool { return e.status(c) == "running" })
	e.gate.quiet(t, "the stopped run started")
	if e.status(b) != "stopped" {
		t.Fatalf("the stopped run: %s", e.status(b))
	}
	if s, _ := e.st.WorkflowSteps(b); s["ag"].Status != "pending" {
		t.Fatalf("a stopped queued run's steps: %+v", s)
	}
	e.gate.openAll()
	e.waitStatus(c, "done")
	if _, _, total := e.gate.stats(); total != 2 {
		t.Fatalf("the model was asked %d times, want 2", total)
	}
}

// queuedRun records a run as an earlier process left it: queued, with its workflow as it was.
func (e *gateEnv) queuedRun(status string) int64 {
	e.t.Helper()
	w, _ := e.st.Workflow(e.ws.ID, workflow.PR)
	var g workflow.Graph
	json.Unmarshal([]byte(w.Graph), &g)
	var nodes []string
	for _, n := range g.Nodes {
		nodes = append(nodes, n.ID)
	}
	id, err := e.st.StartWorkflowRun(store.WorkflowRun{WorkspaceID: e.ws.ID, Kind: workflow.PR, EventNode: "ev1", Item: e.item(), Graph: w.Graph, Status: status}, nodes, e.admin)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

// After a restart the queued runs are still queued (what was running is failed, as always), and they
// start once the queue is looked at; a run queued later is started by the periodic look.
func TestRestartStartsQueuedRuns(t *testing.T) {
	old := queueEvery
	queueEvery = 50 * time.Millisecond
	t.Cleanup(func() { queueEvery = old })
	e := newGateEnv(t, 2)
	e.gate.openAll()
	orphan := e.queuedRun("running")
	q1, q2, q3 := e.queuedRun("queued"), e.queuedRun("queued"), e.queuedRun("queued")
	if err := e.st.StopOrphanWorkflows(workflow.Restartable); err != nil {
		t.Fatal(err)
	}
	if got := e.statuses(orphan, q1, q2, q3); got != "failed queued queued queued" {
		t.Fatalf("after a restart: %s", got)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { e.ui.RunQueue(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done }) // before queueEvery is put back
	for _, id := range []int64{q1, q2, q3} {
		e.waitStatus(id, "done")
	}
	late := e.queuedRun("queued") // nothing told the queue about this one
	e.waitStatus(late, "done")
	if run, _ := e.st.WorkflowRun(q1); run.StartedBy != "derek" || run.FinishedAt.IsZero() {
		t.Fatalf("a started queued run: %+v", run)
	}
}

// The limit is a setting of the workspace's Settings page that its members may change, from 1 to 20;
// raising it starts what was queued.
func TestMaxRunsSetting(t *testing.T) {
	e := newGateEnv(t, 1)
	alice := e.user("alice", false)
	e.user("mallory", false)
	e.st.SetMembers(e.ws.ID, []int64{alice.ID}, e.admin)
	a := e.signIn("alice")
	post := func(b *browser, value string) (int, string) {
		code, _, body := b.post("/w/o/r/settings/max-runs", url.Values{"csrf": {b.csrf()}, "max_runs": {value}})
		return code, body
	}
	if _, body := e.b.get("/w/o/r/settings"); !strings.Contains(body, `name="max_runs" value="1"`) {
		t.Fatalf("the Settings page does not show the limit:\n%s", body)
	}
	for _, bad := range []string{"0", "21", "-1", "three", "", "2.5"} {
		code, body := post(a, bad)
		if code != http.StatusBadRequest || !strings.Contains(body, "whole number from 1 to 20") || e.st.MaxRuns(e.ws.ID) != 1 {
			t.Fatalf("%q: %d, limit %d\n%s", bad, code, e.st.MaxRuns(e.ws.ID), body)
		}
	}
	m := e.signIn("mallory")
	if code, _ := post(m, "5"); code != http.StatusNotFound || e.st.MaxRuns(e.ws.ID) != 1 {
		t.Fatalf("a non-member: %d, limit %d", code, e.st.MaxRuns(e.ws.ID))
	}
	// A member may change it, and the lowest and highest are allowed.
	for _, ok := range []string{"20", "1"} {
		if code, _ := post(a, ok); code != http.StatusSeeOther || fmt.Sprint(e.st.MaxRuns(e.ws.ID)) != ok {
			t.Fatalf("%q: %d, limit %d", ok, code, e.st.MaxRuns(e.ws.ID))
		}
	}
	other, _ := e.st.CreateWorkspace("o/r2", e.admin, "", "")
	if e.st.MaxRuns(other.ID) != 3 {
		t.Fatalf("the default is %d, want 3", e.st.MaxRuns(other.ID))
	}

	// Raising it starts the queued run.
	r1, r2 := e.startItem("ev1", false), e.startItem("ev1", false)
	e.gate.waitEntered(t, 1)
	if got := e.statuses(r1, r2); got != "running queued" {
		t.Fatalf("with a limit of 1: %s", got)
	}
	if code, _ := post(a, "2"); code != http.StatusSeeOther {
		t.Fatal(code)
	}
	e.gate.waitEntered(t, 1)
	e.waitStatus(r2, "running")
	_, body := e.b.get("/w/o/r/settings")
	if !strings.Contains(body, `name="max_runs" value="2"`) {
		t.Fatalf("the Settings page after saving:\n%s", body)
	}
	e.gate.openAll()
	e.waitStatus(r1, "done")
	e.waitStatus(r2, "done")
}

// Discord mention runs use their own concurrency pool, independent of the workspace's PR/Issue
// limit: filling the PR/Issue pool does not queue Discord mentions, and Discord's own queue caps
// concurrent mentions separately.
func TestDiscordUsesSeparateQueue(t *testing.T) {
	e := newGateEnv(t, 1)
	// Add a Discord workflow using the same gated LLM.
	ag := workflow.DefaultAgent(workflow.Discord)
	ag.Model = workflow.ModelRef{Provider: e.prov.ID, Model: "m"}
	agBytes, _ := json.Marshal(ag)
	dg := workflow.Graph{
		Nodes: []workflow.Node{
			{ID: "dm", Type: "discord_mention"},
			{ID: "ag", Type: workflow.Agent, Name: "Answer", Settings: agBytes},
		},
		Links: []workflow.Link{{From: "dm", Out: "out", To: "ag", In: "in"}},
	}
	gb, _ := json.Marshal(dg)
	w, _ := e.st.Workflow(e.ws.ID, workflow.Discord)
	if _, err := e.st.SaveWorkflow(e.ws.ID, workflow.Discord, w.Version, string(gb), e.admin); err != nil {
		t.Fatal(err)
	}

	// Fill the single PR slot and queue a second PR run.
	pr1 := e.startItem("ev1", false)
	e.gate.waitEntered(t, 1)
	pr2 := e.startItem("ev1", false)
	if got := e.statuses(pr1, pr2); got != "running queued" {
		t.Fatalf("PR pool: %s", got)
	}

	// Start 3 Discord mention runs while the PR pool is completely full.
	// The first 2 (maxDiscordRuns) must start immediately; the 3rd queues in Discord's own queue.
	startDiscord := func(msg string) int64 {
		t.Helper()
		id, err := e.ui.startDiscordWorkflow(t.Context(), e.ws, "dm", DiscordTrigger{
			Channel: "general", Author: "alice", Content: msg,
		}, false, e.admin)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	d1, d2, d3 := startDiscord("q1"), startDiscord("q2"), startDiscord("q3")
	e.gate.waitEntered(t, 2) // both d1 and d2 entered the LLM immediately
	if got := e.statuses(d1, d2, d3); got != "running running queued" {
		t.Fatalf("Discord pool while PR full: %s", got)
	}
	// Both pr2 and d3 are 1st in line in their respective queues.
	places := e.ui.queuePlaces(e.ws.ID)
	if places[pr2] != 1 || places[d3] != 1 {
		t.Fatalf("queue places: pr2=%d d3=%d (want both 1)", places[pr2], places[d3])
	}

	// Completing one Discord run dispatches d3, without unblocking pr2.
	e.gate.openAll()
	for _, id := range []int64{pr1, pr2, d1, d2, d3} {
		e.waitStatus(id, "done")
	}
}

// Bypass queue starts a queued run right away from either the Runs list or the run page (next to
// Stop), even when all workspace slots are busy.
func TestBypassQueue(t *testing.T) {
	e := newGateEnv(t, 1)
	e.user("mallory", false)
	m := e.signIn("mallory")

	r1 := e.startItem("ev1", false)
	e.gate.waitEntered(t, 1)
	r2 := e.startItem("ev1", false)
	r3 := e.startItem("ev1", false)
	if got := e.statuses(r1, r2, r3); got != "running queued queued" {
		t.Fatalf("initial: %s", got)
	}

	// The Runs page and the queued run's page both show Bypass queue.
	if _, body := e.b.get("/w/o/r/workflow-runs"); !strings.Contains(body, fmt.Sprintf("/workflow-runs/%d/bypass", r2)) {
		t.Fatalf("runs page missing bypass button for queued run %d", r2)
	}
	if _, body := e.b.get(fmt.Sprintf("/w/o/r/workflow-runs/%d", r2)); !strings.Contains(body, "Bypass queue") {
		t.Fatalf("run page missing Bypass queue button")
	}

	// Non-members cannot bypass.
	if code, _, _ := m.post(fmt.Sprintf("/w/o/r/workflow-runs/%d/bypass", r2), url.Values{"csrf": {m.csrf()}}); code != http.StatusNotFound {
		t.Fatalf("non-member bypass: %d, want 404", code)
	}

	// Bypassing r2 starts it immediately while r1 is still running; r3 stays queued.
	code, loc, _ := e.b.post(fmt.Sprintf("/w/o/r/workflow-runs/%d/bypass", r2), url.Values{"csrf": {e.b.csrf()}})
	if code != http.StatusSeeOther || !strings.HasSuffix(loc, fmt.Sprintf("/workflow-runs/%d", r2)) {
		t.Fatalf("bypass redirect: %d %q", code, loc)
	}
	e.gate.waitEntered(t, 1)
	e.waitStatus(r2, "running")
	if got := e.statuses(r1, r2, r3); got != "running running queued" {
		t.Fatalf("after bypass r2: %s", got)
	}

	// Bypassing r3 from the Runs list redirects back to /workflow-runs.
	code, loc, _ = e.b.post(fmt.Sprintf("/w/o/r/workflow-runs/%d/bypass", r3), url.Values{"csrf": {e.b.csrf()}, "from": {"list"}})
	if code != http.StatusSeeOther || !strings.HasSuffix(loc, "/workflow-runs") {
		t.Fatalf("bypass from list redirect: %d %q", code, loc)
	}
	e.gate.waitEntered(t, 1)
	e.waitStatus(r3, "running")

	e.gate.openAll()
	for _, id := range []int64{r1, r2, r3} {
		e.waitStatus(id, "done")
	}
}
