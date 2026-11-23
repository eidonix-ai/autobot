package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eidonix-ai/autobot/internal/agent"
	"github.com/eidonix-ai/autobot/internal/github"
	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// chatLog is a provider for agent runs: it finishes at once with a report, keeps every request body,
// and says it reused 600 of the 1,000 input tokens of every request after the first. When hold is
// set, the first request waits for it to close.
type chatLog struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []string
}

func newChatLog(t *testing.T, hold chan struct{}) *chatLog {
	c := &chatLog{}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies = append(c.bodies, string(b))
		n := len(c.bodies)
		c.mu.Unlock()
		if n == 1 && hold != nil {
			<-hold
		}
		cached := 0
		if n > 1 {
			cached = 600
		}
		tc := agent.ToolCall{ID: fmt.Sprint("f", n), Type: "function"}
		tc.Function.Name, tc.Function.Arguments = "finish", fmt.Sprintf(`{"status":"met","report":"Report %d."}`, n)
		reply, _ := json.Marshal(agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{tc}})
		fmt.Fprintf(w, `{"choices":[{"message":%s}],"usage":{"prompt_tokens":1000,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":%d}}}`, reply, cached)
	}))
	t.Cleanup(c.Close)
	return c
}

func (c *chatLog) requests() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string{}, c.bodies...)
}

// continues reports whether a request is the previous one with messages added at the end: the same
// bytes (tools, system prompt, earlier messages), then more.
func continues(prev, next string) bool {
	return strings.HasPrefix(next, strings.TrimSuffix(prev, "]}"))
}

// lastMessage is the text of the last message of a request.
func lastMessage(t *testing.T, body string) string {
	t.Helper()
	var req struct{ Messages []agent.Message }
	if err := json.Unmarshal([]byte(body), &req); err != nil || len(req.Messages) == 0 {
		t.Fatalf("a request: %v", err)
	}
	return req.Messages[len(req.Messages)-1].Content
}

func requestField(t *testing.T, body, field string) string {
	t.Helper()
	var req map[string]any
	json.Unmarshal([]byte(body), &req)
	s, _ := req[field].(string)
	return s
}

// contEnv is a workspace with a provider that finishes at once, and pull request 1.
type contEnv struct {
	*pollEnv
	llm  *chatLog
	prov *store.Provider
}

func newContEnv(t *testing.T, hold chan struct{}) *contEnv {
	p := newPollEnv(t)
	llm := newChatLog(t, hold)
	prov, err := p.st.CreateProvider(store.ProviderEdit{Name: "LLM", Kind: store.KindOpenAI, Endpoint: llm.URL + "/v1", Key: "k"}, p.admin)
	if err != nil {
		t.Fatal(err)
	}
	p.st.SetProviderModels(prov.ID, []string{"m", "m2"})
	p.st.SetProviderRoutes(prov.ID, true, false)
	p.st.LendProvider(prov, []int64{p.ws.ID}, p.admin)
	p.addPR(1, false)
	oldWait, oldPoll := convWait, agentPoll
	convWait, agentPoll = 20*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { convWait, agentPoll = oldWait, oldPoll })
	return &contEnv{p, llm, prov}
}

// agentNode is an Agent block with its own Instruction and Done when, which finishes in one turn.
func (c *contEnv) agentNode(id, name string, change func(*workflow.AgentSettings)) workflow.Node {
	a := workflow.DefaultAgent(workflow.PR)
	a.Model, a.Persona, a.MaxTurns, a.MaxMinutes = workflow.ModelRef{Provider: c.prov.ID, Model: "m"}, "custom", 5, 1
	a.Instruction, a.Goal = "Review {pr_title}. This is "+id+".", "goal of "+id
	if change != nil {
		change(&a)
	}
	b, _ := json.Marshal(a)
	return workflow.Node{ID: id, Type: workflow.Agent, Name: name, Settings: b}
}

// saveGraph stores the pull request workflow. Each link goes from one node to the next of its pair.
func (c *contEnv) saveGraph(nodes []workflow.Node, links ...[2]string) {
	c.t.Helper()
	g := workflow.Graph{Nodes: nodes, Links: []workflow.Link{}}
	for _, l := range links {
		g.Links = append(g.Links, workflow.Link{From: l[0], Out: "out", To: l[1], In: "in"})
	}
	b, _ := json.Marshal(g)
	w, _ := c.st.Workflow(c.ws.ID, workflow.PR)
	if _, err := c.st.SaveWorkflow(c.ws.ID, workflow.PR, w.Version, string(b), c.admin); err != nil {
		c.t.Fatal(err)
	}
}

// start starts a run of the workflow from an event node on pull request 1, and returns its id.
func (c *contEnv) start(ev string) int64 {
	c.t.Helper()
	id, err := c.ui.startWorkflow(c.t.Context(), c.ws, workflow.PR, ev, 1, false, c.admin)
	if err != nil {
		c.t.Fatal(err)
	}
	return id
}

// finish waits for a run to end and returns its steps.
func (c *contEnv) finish(id int64) map[string]store.WorkflowStep {
	c.t.Helper()
	for i := 0; i < 400; i++ {
		if r, _ := c.st.WorkflowRun(id); r.Status != "running" {
			steps, _ := c.st.WorkflowSteps(id)
			return steps
		}
		time.Sleep(25 * time.Millisecond)
	}
	c.t.Fatal("the run did not end")
	return nil
}

func (c *contEnv) run(ev string) map[string]store.WorkflowStep { return c.finish(c.start(ev)) }

func (c *contEnv) agentRun(steps map[string]store.WorkflowStep, node string) *store.AgentRun {
	c.t.Helper()
	r, err := c.st.AgentRun(steps[node].AgentRun)
	if err != nil {
		c.t.Fatalf("no agent run for %s: %+v", node, steps[node])
	}
	return r
}

var events = []workflow.Node{{ID: "ev1", Type: "pr_ready"}, {ID: "ev2", Type: "pr_activity"}}

// A block that continues another's conversation sends what the conversation was started with, the
// same bytes, and then its own Instruction and Done when. It has the conversation's model even when
// the block it continues has since been given another; its report is its own.
func TestContinuationSendsTheStoredPrefixThenItsOwnInstruction(t *testing.T) {
	c := newContEnv(t, nil)
	a := c.agentNode("A", "Reviewer", nil)
	b := c.agentNode("B", "Rechecker", func(s *workflow.AgentSettings) {
		s.ContinueOf, s.Model = "A", workflow.ModelRef{}
		s.Instruction, s.Goal = "Recheck {pr_title}.\n{pr_new_commits}\n{pr_new_comments}", "goal of B"
	})
	c.saveGraph(append(append([]workflow.Node{}, events...), a, b), [2]string{"ev1", "A"}, [2]string{"ev2", "B"})
	first := c.run("ev1")
	// The block it continues now uses another model; the conversation keeps the one it began with.
	a2 := c.agentNode("A", "Reviewer", func(s *workflow.AgentSettings) { s.Model.Model = "m2" })
	c.saveGraph(append(append([]workflow.Node{}, events...), a2, b), [2]string{"ev1", "A"}, [2]string{"ev2", "B"})
	second := c.run("ev2")

	reqs := c.llm.requests()
	if len(reqs) != 2 || second["B"].Status != "done" || second["B"].Output != "Report 2." {
		t.Fatalf("%d requests, B %+v", len(reqs), second["B"])
	}
	if !continues(reqs[0], reqs[1]) {
		t.Fatalf("the continuation does not start with the conversation as it was:\n%.400s\n%.400s", reqs[0], reqs[1])
	}
	if requestField(t, reqs[1], "model") != "m" || requestField(t, reqs[1], "user") != requestField(t, reqs[0], "user") || requestField(t, reqs[0], "user") == "" {
		t.Fatalf("model %q, session %q then %q", requestField(t, reqs[1], "model"), requestField(t, reqs[0], "user"), requestField(t, reqs[1], "user"))
	}
	if got, want := lastMessage(t, reqs[1]), "Recheck PR 1.\nNo new commits.\nNo new comments.\n\nDone when: goal of B"; got != want {
		t.Fatalf("the new message:\n%q\nwant\n%q", got, want)
	}
	ra, rb := c.agentRun(first, "A"), c.agentRun(second, "B")
	if rb.Conversation != ra.Conversation || rb.MsgsFrom != ra.MsgsTo || rb.MsgsTo <= rb.MsgsFrom || rb.TurnsFrom != 1 || rb.NodeID != "B" {
		t.Fatalf("A's run %+v\nB's run %+v", ra, rb)
	}
	// The run's page shows the run's own part of the conversation, and how much input was reused.
	code, page := c.b.get(fmt.Sprintf("/w/o/r/agent-runs/%d", rb.ID))
	if code != 200 || !strings.Contains(page, "cache: 60% of input reused") || !strings.Contains(page, "Recheck PR 1.") || strings.Contains(page, "This is A.") ||
		!strings.Contains(page, "continues a conversation") {
		t.Fatalf("run page %d:\n%s", code, page)
	}
}

// The conversation's model is enforced: when the workspace may no longer use it, the continuation
// fails plainly and asks the provider for nothing.
func TestContinuationFailsWhenItsModelIsGone(t *testing.T) {
	c := newContEnv(t, nil)
	b := c.agentNode("B", "Rechecker", func(s *workflow.AgentSettings) { s.ContinueOf, s.Model = "A", workflow.ModelRef{} })
	c.saveGraph(append(append([]workflow.Node{}, events...), c.agentNode("A", "Reviewer", nil), b), [2]string{"ev1", "A"}, [2]string{"ev2", "B"})
	c.run("ev1")
	c.st.SetProviderModels(c.prov.ID, []string{"m2"})
	steps := c.run("ev2")
	if steps["B"].Status != "failed" || !strings.Contains(steps["B"].Error, "started with m, which this workspace may no longer use") || len(c.llm.requests()) != 1 {
		t.Fatalf("%+v, %d requests", steps["B"], len(c.llm.requests()))
	}
}

// With no earlier conversation on the item, a continuation either skips (takes its output with a
// note, and the workflow goes on) or starts a conversation of its own.
func TestNoEarlierConversationSkipOrStartFresh(t *testing.T) {
	for _, fresh := range []bool{false, true} {
		c := newContEnv(t, nil)
		b := c.agentNode("B", "Rechecker", func(s *workflow.AgentSettings) {
			s.ContinueOf, s.Model = "A", workflow.ModelRef{}
			s.Instruction = "Recheck {pr_title}, this is B."
			if fresh {
				s.NoConversation = "fresh"
			}
		})
		after := workflow.Node{ID: "C", Type: workflow.CaseEntry, Settings: json.RawMessage(`{"entry":"after: [{previous_output}]","finalize":true}`)}
		c.saveGraph(append(append([]workflow.Node{}, events...), c.agentNode("A", "Reviewer", nil), b, after), [2]string{"ev2", "B"}, [2]string{"B", "C"})
		steps := c.run("ev2")
		reqs := c.llm.requests()
		if fresh {
			// A conversation of its own, with the model of the block it refers to and its own instruction.
			if len(reqs) != 1 || steps["B"].Output != "Report 1." || steps["B"].Error != "" || requestField(t, reqs[0], "model") != "m" ||
				!strings.Contains(lastMessage(t, reqs[0]), "Recheck PR 1, this is B.") {
				t.Fatalf("fresh: %d requests, %+v", len(reqs), steps["B"])
			}
			if conv, err := c.st.LatestConversation(c.ws.ID, workflow.PR, "B", 1); err != nil || conv.NodeID != "B" {
				t.Fatalf("its conversation: %v %v", conv, err)
			}
			continue
		}
		if len(reqs) != 0 || steps["B"].Status != "done" || steps["B"].Port != "out" || steps["B"].Output != "" || steps["B"].Error != "no earlier conversation" ||
			steps["C"].Status != "done" || steps["C"].Output != "after: []" {
			t.Fatalf("skip: %d requests, B %+v, C %+v", len(reqs), steps["B"], steps["C"])
		}
		if r, err := c.st.AgentRun(1); err == nil {
			t.Fatalf("a skipped step made a run: %+v", r)
		}
	}
}

// A continuation waits for the conversation's run in progress to end, then continues from where it
// left off.
func TestContinuationWaitsForARunInProgress(t *testing.T) {
	hold := make(chan struct{})
	c := newContEnv(t, hold)
	release := sync.OnceFunc(func() { close(hold) })
	t.Cleanup(release) // a failed check must not leave the provider holding the first request
	b := c.agentNode("B", "Rechecker", func(s *workflow.AgentSettings) { s.ContinueOf, s.Model = "A", workflow.ModelRef{} })
	c.saveGraph(append(append([]workflow.Node{}, events...), c.agentNode("A", "Reviewer", nil), b), [2]string{"ev1", "A"}, [2]string{"ev2", "B"})
	runA := c.start("ev1")
	for i := 0; len(c.llm.requests()) == 0; i++ { // A is with the provider
		if i > 400 {
			t.Fatal("A never asked")
		}
		time.Sleep(10 * time.Millisecond)
	}
	runB := c.start("ev2")
	time.Sleep(300 * time.Millisecond)
	if n := len(c.llm.requests()); n != 1 {
		t.Fatalf("B did not wait: %d requests", n)
	}
	if st, _ := c.st.WorkflowSteps(runB); st["B"].Status != "running" {
		t.Fatalf("B: %+v", st["B"])
	}
	release()
	c.finish(runA)
	steps := c.finish(runB)
	reqs := c.llm.requests()
	if len(reqs) != 2 || steps["B"].Status != "done" || !continues(reqs[0], reqs[1]) {
		t.Fatalf("%d requests, B %+v", len(reqs), steps["B"])
	}
	// What A said is in what B was sent.
	if !strings.Contains(reqs[1], "Report 1.") && !strings.Contains(reqs[1], "Finished.") {
		t.Fatalf("B did not see A's turn:\n%.600s", reqs[1])
	}
}

// What a continued conversation is told is new: the commits pushed since its last run, with their
// diff, and the comments and reviews posted since, and nothing older.
func TestNewCommitsAndCommentsAreWhatCameAfter(t *testing.T) {
	c := newContEnv(t, nil)
	b := c.agentNode("B", "Rechecker", func(s *workflow.AgentSettings) {
		s.ContinueOf, s.Model = "A", workflow.ModelRef{}
		s.Instruction = "COMMITS:\n{pr_new_commits}\nCOMMENTS:\n{pr_new_comments}\nEND"
	})
	c.saveGraph(append(append([]workflow.Node{}, events...), c.agentNode("A", "Reviewer", nil), b), [2]string{"ev1", "A"}, [2]string{"ev2", "B"})
	record := func(kind, who, body string, at time.Time) {
		c.gh.mu.Lock()
		defer c.gh.mu.Unlock()
		c.gh.records = append(c.gh.records, github.CommentRecord{Kind: kind, ID: int64(len(c.gh.records) + 1), Number: 1, Author: who, Body: body, State: "CHANGES_REQUESTED", Path: "a.go", Line: 3, Created: at})
	}
	record("comment", "carol", "an old remark", time.Now().Add(-2*time.Hour))
	c.run("ev1")

	// Nothing has happened since.
	c.run("ev2")
	reqs := c.llm.requests()
	if got := lastMessage(t, reqs[1]); !strings.Contains(got, "COMMITS:\nNo new commits.\nCOMMENTS:\nNo new comments.\nEND") {
		t.Fatalf("with nothing new:\n%s", got)
	}

	// A commit is pushed, and a comment, a review and an inline comment are posted.
	c.editPR(1, func(l *github.Listed) { l.HeadSHA = "sha1-2" })
	c.gh.compare = func(base, head string) ([]github.Commit, []github.ChangedFile, error) {
		if base != "sha1-1" || head != "sha1-2" {
			t.Errorf("compared %s with %s", base, head)
		}
		return []github.Commit{{SHA: "abcdef0123456789", Author: "alice", Message: "fix the typo"}},
			[]github.ChangedFile{{Path: "typo.go", Status: "modified", Additions: 1, Patch: "+the fixed line"}}, nil
	}
	later := time.Now().Add(time.Hour)
	record("comment", "dave", "please add a test", later)
	record("review", "erin", "needs work", later.Add(time.Minute))
	record("review_comment", "erin", "off by one here", later.Add(2*time.Minute))
	c.run("ev2")
	got := lastMessage(t, c.llm.requests()[2])
	for _, want := range []string{"abcdef01 alice: fix the typo", "+the fixed line", "@dave commented at", "please add a test",
		"@erin reviewed (changes requested)", "needs work", "@erin commented on a.go:3", "off by one here"} {
		if !strings.Contains(got, want) {
			t.Errorf("the new message lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "an old remark") || strings.Contains(got, "No new") {
		t.Errorf("the new message has what was old, or says nothing is new:\n%s", got)
	}
	// The one after sees only what came after that: the comments are still in the future, so they come
	// again, but the commits, now seen, do not.
	c.run("ev2")
	if got := lastMessage(t, c.llm.requests()[3]); !strings.Contains(got, "No new commits.") {
		t.Fatalf("the commits came again:\n%s", got)
	}
}

// A block run again on the same item resumes its own conversation, with its follow-up as the next
// message; set to start fresh, or with a new model, it starts a conversation of its own.
func TestRerunResumesOrStartsFresh(t *testing.T) {
	c := newContEnv(t, nil)
	set := func(change func(*workflow.AgentSettings)) {
		a := c.agentNode("A", "Reviewer", func(s *workflow.AgentSettings) {
			s.Rerun, s.FollowUp = "resume", "Anything new? {pr_new_comments}"
			if change != nil {
				change(s)
			}
		})
		c.saveGraph(append(append([]workflow.Node{}, events...), a), [2]string{"ev1", "A"})
	}
	set(nil)
	c.run("ev1")
	c.run("ev1")
	reqs := c.llm.requests()
	if !continues(reqs[0], reqs[1]) || lastMessage(t, reqs[1]) != "Anything new? No new comments.\n\nDone when: goal of A" {
		t.Fatalf("resumed:\n%q", lastMessage(t, reqs[1]))
	}
	set(func(s *workflow.AgentSettings) { s.Rerun = "fresh" })
	c.run("ev1")
	reqs = c.llm.requests()
	if continues(reqs[1], reqs[2]) || strings.Count(reqs[2], `"role":"user"`) != 1 || requestField(t, reqs[2], "user") == requestField(t, reqs[1], "user") {
		t.Fatalf("a fresh start continued the conversation:\n%.300s", reqs[2])
	}
	// A new model cannot take over a conversation it did not have.
	set(func(s *workflow.AgentSettings) { s.Model.Model = "m2" })
	c.run("ev1")
	reqs = c.llm.requests()
	if requestField(t, reqs[3], "model") != "m2" || strings.Count(reqs[3], `"role":"user"`) != 1 {
		t.Fatalf("a new model resumed the old conversation:\n%.300s", reqs[3])
	}
}

// The poller starts no second run of an event block on an item while one is running, and the events
// stay until it has ended: all of them then start one run, which continues the conversation.
func TestPollerFoldsEventsWhileAConversationRuns(t *testing.T) {
	hold := make(chan struct{})
	c := newContEnv(t, hold)
	release := sync.OnceFunc(func() { close(hold) })
	t.Cleanup(release)
	c.saveGraph([]workflow.Node{c.eventNode("ev", "pr_activity", func(s *workflow.EventSettings) { s.Activity, s.QuietMinutes = []string{"comments"}, 0 }),
		c.agentNode("A", "Reviewer", nil)}, [2]string{"ev", "A"})
	c.goLive(workflow.PR)
	c.held[1] = true // the first run is kept running on purpose
	c.comment(1, "carol", "one")
	c.round()
	for i := 0; len(c.llm.requests()) == 0; i++ {
		if i > 400 {
			t.Fatal("the run never asked")
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.comment(1, "carol", "two")
	c.comment(1, "dave", "three")
	c.round()
	c.round()
	c.wantRuns(1)
	if pending, _ := c.st.PollPending(c.ws.ID, workflow.PR, "ev"); len(pending) != 1 || pending[0] != 1 {
		t.Fatalf("the events did not stay: pending %v", pending)
	}
	release()
	c.finish(1)
	delete(c.held, 1)
	c.round()
	c.wantRuns(1, 1)
	reqs := c.llm.requests()
	if len(reqs) != 2 || !continues(reqs[0], reqs[1]) {
		t.Fatalf("the second run did not continue the conversation: %d requests", len(reqs))
	}
	if pending, _ := c.st.PollPending(c.ws.ID, workflow.PR, "ev"); len(pending) != 0 {
		t.Fatalf("still pending: %v", pending)
	}
}

// eventNode is an event block of a type, with its settings changed.
func (c *contEnv) eventNode(id, typ string, change func(*workflow.EventSettings)) workflow.Node {
	s := workflow.DefaultEvent(typ)
	s.Authors, s.OverLimit, s.WaitCI, s.ReviewRequest = "all", "skip", "no", false
	if change != nil {
		change(&s)
	}
	b, _ := json.Marshal(s)
	return workflow.Node{ID: id, Type: typ, Settings: b}
}

// A conversation whose file is missing (Autobot stopped before its first turn was written) is set
// aside: the next run of the block starts a new one instead of failing on every run.
func TestMissingConversationFileIsSetAside(t *testing.T) {
	c := newContEnv(t, nil)
	a := c.agentNode("A", "Reviewer", func(s *workflow.AgentSettings) { s.Rerun = "resume" })
	c.saveGraph(append(append([]workflow.Node{}, events...), a), [2]string{"ev1", "A"})
	first := c.agentRun(c.run("ev1"), "A")
	os.Remove(filepath.Join(c.st.RunsDir(), fmt.Sprintf("conv-%d.jsonl.gz", first.Conversation)))
	steps := c.run("ev1")
	second := c.agentRun(steps, "A")
	if steps["A"].Status != "done" || second.Conversation == first.Conversation || second.Status != "met" {
		t.Fatalf("step %+v, run %+v", steps["A"], second)
	}
}
