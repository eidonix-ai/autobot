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
	// reply, when set, makes the answer: it is given the messages and the names of the tools offered.
	reply func(msgs []agent.Message, tools []string) agent.Message
	// status, when set, may make a request fail: it is given the request's number (from 1) and answers
	// an HTTP status, or 0 to let it through.
	status func(n int) int
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
		if c.status != nil {
			if code := c.status(n); code != 0 {
				http.Error(w, "refused on purpose", code)
				return
			}
		}
		cached := 0
		if n > 1 {
			cached = 600
		}
		tc := agent.ToolCall{ID: fmt.Sprint("f", n), Type: "function"}
		tc.Function.Name, tc.Function.Arguments = "finish", fmt.Sprintf(`{"status":"met","report":"Report %d."}`, n)
		answer := agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{tc}}
		if c.reply != nil {
			var req struct {
				Messages []agent.Message
				Tools    []struct{ Function struct{ Name string } }
			}
			json.Unmarshal(b, &req)
			var names []string
			for _, tl := range req.Tools {
				names = append(names, tl.Function.Name)
			}
			answer = c.reply(req.Messages, names)
		}
		reply, _ := json.Marshal(answer)
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

// messagesOf is the messages of a request.
func messagesOf(t *testing.T, body string) []agent.Message {
	t.Helper()
	var req struct{ Messages []agent.Message }
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("a request: %v", err)
	}
	return req.Messages
}

// pad is a thread's scratchpad.
func (c *contEnv) pad(thread int64) []store.PadEntry {
	c.t.Helper()
	list, err := c.st.Pad(thread).List()
	if err != nil {
		c.t.Fatal(err)
	}
	return list
}

// section is the text of an opening message from a heading to the next blank line followed by
// another heading, or the end.
func section(text, from, to string) string {
	_, rest, ok := strings.Cut(text, from)
	if !ok {
		return ""
	}
	if to != "" {
		rest, _, _ = strings.Cut(rest, to)
	}
	return rest
}

// A block that continues another's thread starts a fresh session: its first request is the system
// prompt and one message, which has its own Instruction and Done when, then the other block's last
// report in full, its scratchpad's index, and what is new on the pull request since: and none of
// the earlier messages.
func TestContinuationOpensFreshFromTheLastReport(t *testing.T) {
	c := newContEnv(t, nil)
	a := c.agentNode("A", "Reviewer", nil)
	b := c.agentNode("B", "Rechecker", func(s *workflow.AgentSettings) {
		s.ContinueOf = "A"
		s.Instruction, s.Goal = "Recheck {pr_title}.", "goal of B"
	})
	c.saveGraph(append(append([]workflow.Node{}, events...), a, b), [2]string{"ev1", "A"}, [2]string{"ev2", "B"})
	first := c.run("ev1")
	c.editPR(1, func(l *github.Listed) { l.HeadSHA = "sha1-2" })
	c.gh.compare = func(base, head string) ([]github.Commit, []github.ChangedFile, error) {
		return []github.Commit{{SHA: "abcdef0123456789", Author: "alice", Message: "fix the typo"}}, []github.ChangedFile{{Path: "typo.go", Status: "modified", Additions: 1, Patch: "+the fixed line"}}, nil
	}
	c.gh.mu.Lock()
	c.gh.records = append(c.gh.records, github.CommentRecord{Kind: "comment", ID: 1, Number: 1, Author: "dave", Body: "please add a test", Created: time.Now().Add(time.Hour)})
	c.gh.mu.Unlock()
	second := c.run("ev2")

	reqs := c.llm.requests()
	if len(reqs) != 2 || second["B"].Status != "done" || second["B"].Output != "Report 2." {
		t.Fatalf("%d requests, B %+v", len(reqs), second["B"])
	}
	msgs := messagesOf(t, reqs[1])
	if len(msgs) != 2 || msgs[0].Role != "system" || msgs[1].Role != "user" {
		t.Fatalf("the continuation's first request has %d messages: %.600s", len(msgs), reqs[1])
	}
	opening := msgs[1].Content
	wantStart := "Recheck PR 1.\n\nDone when: goal of B\n\nYour last summary on this item:\nReport 1.\n\nThe full report is entry #1 of your notes: read it with scratchpad_read.\n\nYour notes so far, readable with scratchpad_read and scratchpad_search:\n#1 [high] Report of "
	if !strings.HasPrefix(opening, wantStart) {
		t.Fatalf("the opening message:\n%s", opening)
	}
	for _, want := range []string{"New since that report:\nCommits pushed since", "abcdef01 alice: fix the typo", "+the fixed line", "@dave commented at", "please add a test"} {
		if !strings.Contains(opening, want) {
			t.Errorf("the opening lacks %q:\n%s", want, opening)
		}
	}
	// None of the earlier session: not its task, not its tool calls, not the conversation's session key.
	if strings.Contains(reqs[1], "This is A.") || strings.Contains(reqs[1], `"role":"assistant"`) || requestField(t, reqs[1], "user") == requestField(t, reqs[0], "user") {
		t.Fatalf("the continuation carried over the earlier session:\n%.800s", reqs[1])
	}
	ra, rb := c.agentRun(first, "A"), c.agentRun(second, "B")
	if rb.Conversation == ra.Conversation || rb.MsgsFrom != 0 || rb.Thread != ra.Thread || rb.Thread == 0 || rb.NodeID != "B" {
		t.Fatalf("A's run %+v\nB's run %+v", ra, rb)
	}
	// The run's page shows the scratchpad of the thread and the session's own transcript.
	code, page := c.b.get(fmt.Sprintf("/w/o/r/agent-runs/%d", rb.ID))
	if code != 200 || !strings.Contains(page, "cache: 60% of input reused") || !strings.Contains(page, "Recheck PR 1.") || strings.Contains(page, "This is A.") ||
		!strings.Contains(page, "Scratchpad") || !strings.Contains(page, "Report of ") {
		t.Fatalf("run page %d:\n%s", code, page)
	}
}

// Every final report is an entry of the thread's scratchpad, titled with its date and run, in
// high importance; the continuation appends to the same scratchpad, and the next one opens with
// the newest report, not an older one.
func TestEachReportIsAnEntryAndTheNewestOpensTheNext(t *testing.T) {
	c := newContEnv(t, nil)
	a := c.agentNode("A", "Reviewer", func(s *workflow.AgentSettings) { s.Rerun = "resume" })
	b := c.agentNode("B", "Rechecker", func(s *workflow.AgentSettings) { s.ContinueOf = "A" })
	c.saveGraph(append(append([]workflow.Node{}, events...), a, b), [2]string{"ev1", "A"}, [2]string{"ev2", "B"})
	r1 := c.agentRun(c.run("ev1"), "A")
	r2 := c.agentRun(c.run("ev1"), "A")
	r3 := c.agentRun(c.run("ev2"), "B")
	if r1.Thread == 0 || r2.Thread != r1.Thread || r3.Thread != r1.Thread {
		t.Fatalf("threads %d %d %d", r1.Thread, r2.Thread, r3.Thread)
	}
	pad := c.pad(r1.Thread)
	if len(pad) != 3 {
		t.Fatalf("%d entries: %+v", len(pad), pad)
	}
	for i, e := range pad {
		run := []*store.AgentRun{r1, r2, r3}[i]
		if e.N != i+1 || !e.Report || e.Importance != "high" || e.Author != "main" || e.Body != fmt.Sprintf("Report %d.", i+1) || e.Title != store.ReportTitle(run.ID) {
			t.Errorf("entry %d: %+v", i, e)
		}
	}
	// The second run's opening had the first report; the third's has the second, and not the first.
	reqs := c.llm.requests()
	if got := section(messagesOf(t, reqs[1])[1].Content, "Your last summary on this item:\n", "\n\nThe full report is entry"); got != "Report 1." {
		t.Fatalf("the second run opened with %q", got)
	}
	if got := section(messagesOf(t, reqs[2])[1].Content, "Your last summary on this item:\n", "\n\nThe full report is entry"); got != "Report 2." {
		t.Fatalf("the third run opened with %q", got)
	}
}

// A reference block's model is its own, once set: it defaults to the other block's but may be
// changed, and a block left with none uses the other's.
func TestReferenceBlockCanUseItsOwnModel(t *testing.T) {
	c := newContEnv(t, nil)
	a := c.agentNode("A", "Reviewer", nil)
	own := c.agentNode("B", "Rechecker", func(s *workflow.AgentSettings) {
		s.ContinueOf, s.Model = "A", workflow.ModelRef{Provider: c.prov.ID, Model: "m2"}
	})
	c.saveGraph(append(append([]workflow.Node{}, events...), a, own), [2]string{"ev1", "A"}, [2]string{"ev2", "B"})
	c.run("ev1")
	steps := c.run("ev2")
	reqs := c.llm.requests()
	if len(reqs) != 2 || steps["B"].Status != "done" || requestField(t, reqs[0], "model") != "m" || requestField(t, reqs[1], "model") != "m2" {
		t.Fatalf("%d requests, models %q then %q, B %+v", len(reqs), requestField(t, reqs[0], "model"), requestField(t, reqs[len(reqs)-1], "model"), steps["B"])
	}
	// With none of its own, the other block's.
	none := c.agentNode("B", "Rechecker", func(s *workflow.AgentSettings) { s.ContinueOf, s.Model = "A", workflow.ModelRef{} })
	c.saveGraph(append(append([]workflow.Node{}, events...), a, none), [2]string{"ev1", "A"}, [2]string{"ev2", "B"})
	c.run("ev2")
	if reqs = c.llm.requests(); len(reqs) != 3 || requestField(t, reqs[2], "model") != "m" {
		t.Fatalf("a block with no model of its own: %d requests", len(reqs))
	}
}

// A continuation whose own model the workspace may no longer use fails plainly and asks the
// provider for nothing.
func TestContinuationFailsWhenItsModelIsGone(t *testing.T) {
	c := newContEnv(t, nil)
	b := c.agentNode("B", "Rechecker", func(s *workflow.AgentSettings) {
		s.ContinueOf, s.Model = "A", workflow.ModelRef{Provider: c.prov.ID, Model: "m2"}
	})
	c.saveGraph(append(append([]workflow.Node{}, events...), c.agentNode("A", "Reviewer", nil), b), [2]string{"ev1", "A"}, [2]string{"ev2", "B"})
	c.run("ev1")
	c.st.SetProviderModels(c.prov.ID, []string{"m"})
	steps := c.run("ev2")
	if steps["B"].Status != "failed" || len(c.llm.requests()) != 1 {
		t.Fatalf("%+v, %d requests", steps["B"], len(c.llm.requests()))
	}
}

// With no earlier report on the item, a continuation either skips (takes its output with a note, and
// the workflow goes on) or starts a thread of its own.
func TestNoEarlierReportSkipOrStartFresh(t *testing.T) {
	for _, fresh := range []bool{false, true} {
		c := newContEnv(t, nil)
		b := c.agentNode("B", "Rechecker", func(s *workflow.AgentSettings) {
			s.ContinueOf = "A"
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
			// A thread of its own, opening with its instruction and no report.
			if len(reqs) != 1 || steps["B"].Output != "Report 1." || steps["B"].Error != "" ||
				!strings.Contains(lastMessage(t, reqs[0]), "Recheck PR 1, this is B.") || strings.Contains(reqs[0], "Your last summary") {
				t.Fatalf("fresh: %d requests, %+v", len(reqs), steps["B"])
			}
			if th, err := c.st.LatestThread(c.ws.ID, workflow.PR, "B", 1); err != nil || th.NodeID != "B" {
				t.Fatalf("its thread: %v %v", th, err)
			}
			continue
		}
		if len(reqs) != 0 || steps["B"].Status != "done" || steps["B"].Port != "out" || steps["B"].Output != "" || steps["B"].Error != "no earlier report" ||
			steps["C"].Status != "done" || steps["C"].Output != "after: []" {
			t.Fatalf("skip: %d requests, B %+v, C %+v", len(reqs), steps["B"], steps["C"])
		}
		if r, err := c.st.AgentRun(1); err == nil {
			t.Fatalf("a skipped step made a run: %+v", r)
		}
	}
}

// A thread the block started that has notes but no report (its run failed) is no report to
// continue from: the block skips; a block run again on the item resumes it all the same.
func TestThreadWithoutAReport(t *testing.T) {
	c := newContEnv(t, nil)
	_, _, th, err := c.st.StartAgentRun(store.AgentRun{WorkspaceID: c.ws.ID, Kind: workflow.PR, NodeID: "A", NodeName: "A", Item: 1, Model: "m"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.st.StopOrphanRuns()
	c.st.Pad(th).Add(store.PadEntry{Title: "a finding", Importance: store.High, Description: "d", Body: "b"})
	b := c.agentNode("B", "Rechecker", func(s *workflow.AgentSettings) { s.ContinueOf = "A" })
	a := c.agentNode("A", "Reviewer", func(s *workflow.AgentSettings) { s.Rerun = "resume" })
	c.saveGraph(append(append([]workflow.Node{}, events...), a, b), [2]string{"ev1", "A"}, [2]string{"ev2", "B"})
	if steps := c.run("ev2"); steps["B"].Error != "no earlier report" || len(c.llm.requests()) != 0 {
		t.Fatalf("%+v", steps["B"])
	}
	steps := c.run("ev1")
	opening := lastMessage(t, c.llm.requests()[0])
	if c.agentRun(steps, "A").Thread != th || strings.Contains(opening, "Your last summary") || !strings.Contains(opening, "#1 [high] a finding — d") {
		t.Fatalf("thread %d, opening:\n%s", c.agentRun(steps, "A").Thread, opening)
	}
}

// A block run again on the same item resumes its thread (its follow-up replaces its instruction, and
// it opens with its last report and notes), or, set to start fresh, starts a new thread with an empty
// scratchpad and no report.
func TestRerunResumesOrStartsAnEmptyThread(t *testing.T) {
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
	r1 := c.agentRun(c.run("ev1"), "A")
	r2 := c.agentRun(c.run("ev1"), "A")
	reqs := c.llm.requests()
	msgs := messagesOf(t, reqs[1])
	if r2.Thread != r1.Thread || len(msgs) != 2 || !strings.HasPrefix(msgs[1].Content, "Anything new? No new comments.\n\nDone when: goal of A\n\nYour last summary on this item:\nReport 1.") {
		t.Fatalf("resumed (thread %d, then %d):\n%q", r1.Thread, r2.Thread, msgs[len(msgs)-1].Content)
	}
	set(func(s *workflow.AgentSettings) { s.Rerun = "fresh" })
	r3 := c.agentRun(c.run("ev1"), "A")
	reqs = c.llm.requests()
	if r3.Thread == r1.Thread || strings.Contains(reqs[2], "Your last summary") || strings.Contains(reqs[2], "Your notes so far") || requestField(t, reqs[2], "user") == requestField(t, reqs[1], "user") {
		t.Fatalf("a fresh start used the earlier thread:\n%.500s", reqs[2])
	}
	if pad := c.pad(r3.Thread); len(pad) != 1 || pad[0].Body != "Report 3." {
		t.Fatalf("the new thread's scratchpad: %+v", pad)
	}
	if pad := c.pad(r1.Thread); len(pad) != 2 {
		t.Fatalf("the old thread's scratchpad changed: %+v", pad)
	}
	// Fresh again: the next resume is of the newest thread.
	set(nil)
	r4 := c.agentRun(c.run("ev1"), "A")
	if r4.Thread != r3.Thread || !strings.Contains(lastMessage(t, c.llm.requests()[3]), "Your last summary on this item:\nReport 3.") {
		t.Fatalf("thread %d, want %d", r4.Thread, r3.Thread)
	}
}

// A continuation waits for the thread's run in progress to end, then opens with its report; so does
// a block that resumes its own thread.
func TestContinuationWaitsForARunInProgress(t *testing.T) {
	for _, resume := range []bool{false, true} {
		hold := make(chan struct{})
		c := newContEnv(t, hold)
		release := sync.OnceFunc(func() { close(hold) })
		t.Cleanup(release) // a failed check must not leave the provider holding the first request
		second, ev := "B", "ev2"
		nodes := []workflow.Node{c.agentNode("A", "Reviewer", func(s *workflow.AgentSettings) { s.Rerun = "resume" }),
			c.agentNode("B", "Rechecker", func(s *workflow.AgentSettings) { s.ContinueOf = "A" })}
		if resume {
			second, ev = "A", "ev1"
		}
		c.saveGraph(append(append([]workflow.Node{}, events...), nodes...), [2]string{"ev1", "A"}, [2]string{"ev2", "B"})
		runA := c.start("ev1")
		for i := 0; len(c.llm.requests()) == 0; i++ { // A is with the provider
			if i > 400 {
				t.Fatal("A never asked")
			}
			time.Sleep(10 * time.Millisecond)
		}
		runB := c.start(ev)
		time.Sleep(300 * time.Millisecond)
		if n := len(c.llm.requests()); n != 1 {
			t.Fatalf("resume %v: the second run did not wait: %d requests", resume, n)
		}
		if st, _ := c.st.WorkflowSteps(runB); st[second].Status != "running" {
			t.Fatalf("%s: %+v", second, st[second])
		}
		release()
		c.finish(runA)
		steps := c.finish(runB)
		reqs := c.llm.requests()
		if len(reqs) != 2 || steps[second].Status != "done" || !strings.Contains(lastMessage(t, reqs[1]), "Your last summary on this item:\nReport 1.") {
			t.Fatalf("resume %v: %d requests, %+v", resume, len(reqs), steps[second])
		}
	}
}

// What a continued thread is told is new: the commits pushed since its last run, with their diff,
// and the comments and reviews posted since, and nothing older.
func TestNewCommitsAndCommentsAreWhatCameAfter(t *testing.T) {
	c := newContEnv(t, nil)
	b := c.agentNode("B", "Rechecker", func(s *workflow.AgentSettings) {
		s.ContinueOf = "A"
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

// The poller starts no second run of an event block on an item while one is running, and the events
// stay until it has ended: all of them then start one run, which resumes the thread.
func TestPollerFoldsEventsWhileAThreadRuns(t *testing.T) {
	hold := make(chan struct{})
	c := newContEnv(t, hold)
	release := sync.OnceFunc(func() { close(hold) })
	t.Cleanup(release)
	c.saveGraph([]workflow.Node{c.eventNode("ev", "pr_activity", func(s *workflow.EventSettings) { s.Activity, s.QuietMinutes = []string{"comments"}, 0 }),
		c.agentNode("A", "Reviewer", func(s *workflow.AgentSettings) { s.Rerun = "resume" })}, [2]string{"ev", "A"})
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
	if len(reqs) != 2 || !strings.Contains(lastMessage(t, reqs[1]), "Your last summary on this item:\nReport 1.") {
		t.Fatalf("the second run did not resume the thread: %d requests", len(reqs))
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

// A thread does not depend on its sessions' files: with one gone (retention, or a disk problem), the
// next run still resumes the thread from its report and notes.
func TestThreadSurvivesAMissingConversationFile(t *testing.T) {
	c := newContEnv(t, nil)
	a := c.agentNode("A", "Reviewer", func(s *workflow.AgentSettings) { s.Rerun = "resume" })
	c.saveGraph(append(append([]workflow.Node{}, events...), a), [2]string{"ev1", "A"})
	first := c.agentRun(c.run("ev1"), "A")
	os.Remove(filepath.Join(c.st.RunsDir(), fmt.Sprintf("conv-%d.jsonl.gz", first.Conversation)))
	steps := c.run("ev1")
	second := c.agentRun(steps, "A")
	if steps["A"].Status != "done" || second.Thread != first.Thread || second.Status != "met" || !strings.Contains(lastMessage(t, c.llm.requests()[1]), "Your last summary on this item:\nReport 1.") {
		t.Fatalf("step %+v, run %+v", steps["A"], second)
	}
}
