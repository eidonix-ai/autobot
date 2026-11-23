package web

import (
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eidonix-ai/autobot/internal/agent"
	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// subAgentModel plays a main agent that hands one task to a sub-agent and then finishes, and the
// sub-agent, which saves a note and finishes. A session is a sub-agent's when its preamble says so.
func subAgentModel(msgs []agent.Message, tools []string) agent.Message {
	hasAssistant := slices.ContainsFunc(msgs, func(m agent.Message) bool { return m.Role == "assistant" })
	if strings.HasPrefix(msgs[0].Content, "You are Autobot, a sub-agent") {
		if !hasAssistant {
			return toolCall("n", "scratchpad_write", `{"title":"SUB NOTE","importance":"high","description":"what the sub-agent found","body":"body of the sub-agent's note"}`)
		}
		return toolCall("f", "finish", `{"status":"met","report":"SUB REPORT: found in the loader."}`)
	}
	if !hasAssistant {
		return toolCall("s", "spawn_agent", `{"title":"callers","task":"find the callers"}`)
	}
	return toolCall("f", "finish", `{"status":"met","report":"MAIN REPORT"}`)
}

func (c *contEnv) subBlock(id string, atOnce int, change func(*workflow.AgentSettings)) workflow.Node {
	return c.agentNode(id, "Researcher", func(s *workflow.AgentSettings) {
		s.SubAgents, s.MaxTurns = atOnce, 6
		if change != nil {
			change(s)
		}
	})
}

// spawn_agent is the main agent's only with sub-agents allowed; a sub-agent has the read-only tools, its
// own scratchpad tools and no shell, no web unless the block allows it, and no spawn_agent.
func TestSpawnAgentIsOfferedOnlyWithSubAgents(t *testing.T) {
	c := newContEnv(t, nil)
	c.llm.reply = subAgentModel
	c.saveGraph(append(append([]workflow.Node{}, events...), c.subBlock("A", 0, nil)), [2]string{"ev1", "A"})
	c.run("ev1")
	if tools := requestTools(t, c.llm.requests()[0]); slices.Contains(tools, "spawn_agent") {
		t.Fatalf("with 0 sub-agents: %v", tools)
	}
	// With 2 at a time and 5 in a run.
	c.saveGraph(append(append([]workflow.Node{}, events...), c.subBlock("A", 2, func(s *workflow.AgentSettings) { s.SubAgentsPerRun = 5 })), [2]string{"ev1", "A"})
	run := c.agentRun(c.run("ev1"), "A")
	reqs := c.llm.requests()[1:]
	var main, sub []string
	var mainBody string
	for _, body := range reqs {
		if strings.HasPrefix(messagesOf(t, body)[0].Content, "You are Autobot, a sub-agent") {
			sub = requestTools(t, body)
		} else {
			main, mainBody = requestTools(t, body), body
		}
	}
	if !slices.Contains(main, "spawn_agent") || !strings.Contains(mainBody, "up to 2 sub-agents working at once and 5 in this run. Each has up to 6 turns of its own") {
		t.Fatalf("with 2 sub-agents: %v", main)
	}
	for _, name := range []string{"spawn_agent", "bash", "write_file", "edit_file", "fetch_url", "web_search"} {
		if slices.Contains(sub, name) {
			t.Errorf("a sub-agent has %s", name)
		}
	}
	// The web only if the block allows it.
	c.saveGraph(append(append([]workflow.Node{}, events...), c.subBlock("A", 2, func(s *workflow.AgentSettings) { s.Internet = true })), [2]string{"ev1", "A"})
	c.run("ev1")
	for _, body := range c.llm.requests()[len(c.llm.requests())-2:] {
		if tools := requestTools(t, body); !slices.Contains(tools, "fetch_url") || slices.Contains(tools, "bash") {
			t.Fatalf("with the internet allowed: %v", tools)
		}
	}
	for _, name := range []string{"scratchpad_write", "scratchpad_read", "scratchpad_index", "scratchpad_search", "prune_result", "compact", "update_plan", "finish", "read_file"} {
		if !slices.Contains(sub, name) {
			t.Errorf("a sub-agent lacks %s: %v", name, sub)
		}
	}
	// Its notes are its own: the thread's scratchpad has only the run's report.
	for _, e := range c.pad(run.Thread) { // two runs of the thread, each with its report; none of the sub-agent's notes
		if !e.Report || e.Body != "MAIN REPORT" {
			t.Fatalf("the thread's scratchpad has %+v", e)
		}
	}
	notes, err := c.st.SubNotes(run.ID, "s1")
	if err != nil || len(notes) != 1 || notes[0].Title != "SUB NOTE" {
		t.Fatalf("%+v %v", notes, err)
	}
}

// The run page shows each sub-agent with its notes and its transcript, and the run's totals include
// its sub-agents' turns and tokens, with the numbers of each session beside them.
func TestRunPageShowsSubAgentsAndTotals(t *testing.T) {
	c := newContEnv(t, nil)
	c.llm.reply = subAgentModel
	c.saveGraph(append(append([]workflow.Node{}, events...), c.subBlock("A", 2, nil)), [2]string{"ev1", "A"})
	run := c.agentRun(c.run("ev1"), "A")
	code, page := c.b.get(fmt.Sprintf("/w/o/r/agent-runs/%d", run.ID))
	// The model reports 1,000 tokens in and 10 out for every request; the main agent made two requests, the
	// sub-agent two.
	for _, want := range []string{`id="run-subs"`, "Sub-agent s1: callers", "Finished (met)", "2 turns · 2000 tokens in", "body of the sub-agent&#39;s note", "#1 · high · s1",
		"The main agent: 2 turns", "SUB REPORT: found in the loader.", "scratchpad_write", "MAIN REPORT"} {
		if !strings.Contains(page, want) {
			t.Errorf("the run page (%d) lacks %q", code, want)
		}
	}
	// Totals: 4 turns, 4 x 1000 tokens in, 40 out.
	if !strings.Contains(page, "4 turns · 4000 tokens in") || !strings.Contains(page, ", 40 out") {
		start := strings.Index(page, `<p class="status">`)
		t.Fatalf("the totals: %.300s", page[start:])
	}
	// A run without sub-agents has none of it.
	c2 := newContEnv(t, nil)
	c2.saveGraph(append(append([]workflow.Node{}, events...), c2.agentNode("A", "Plain", nil)), [2]string{"ev1", "A"})
	plain := c2.agentRun(c2.run("ev1"), "A")
	_, page = c2.b.get(fmt.Sprintf("/w/o/r/agent-runs/%d", plain.ID))
	if strings.Contains(page, "Sub-agent s1") || strings.Contains(page, "The main agent:") {
		t.Fatal("a run without sub-agents shows some")
	}
}

// The time warning reaches runs: with 45 of the 60 seconds gone, the default (10%, from 54 seconds) and
// 0 have not warned, and 50% (from 30 seconds) has, the main agent and its sub-agent on their first turn.
func TestTimeWarningIsWiredIntoRuns(t *testing.T) {
	warned := func(body string) bool { return strings.Contains(body, "left: finish with your report soon.") }
	for name, percent := range map[string]*int{"default": nil, "none": new(int), "all": ptr(50)} {
		c := newContEnv(t, nil)
		c.llm.reply = subAgentModel
		c.saveGraph(append(append([]workflow.Node{}, events...), c.subBlock("A", 1, func(s *workflow.AgentSettings) { s.TimeWarnPercent = percent })), [2]string{"ev1", "A"})
		// The clock says 45 seconds of the minute have gone when the first turn comes.
		var calls atomic.Int32
		runClock = func() time.Time {
			if calls.Add(1) == 1 {
				return time.Now()
			}
			return time.Now().Add(45 * time.Second)
		}
		t.Cleanup(func() { runClock = time.Now })
		c.run("ev1")
		var mainFirst, subFirst string
		for _, body := range c.llm.requests() {
			msgs := messagesOf(t, body)
			if len(msgs) == 2 || (len(msgs) == 3 && msgs[2].Role == "user") { // a first turn
				if strings.HasPrefix(msgs[0].Content, "You are Autobot, a sub-agent") {
					subFirst = body
				} else if mainFirst == "" {
					mainFirst = body
				}
			}
		}
		if mainFirst == "" || subFirst == "" {
			t.Fatalf("%s: no first requests", name)
		}
		want := name == "all"
		if warned(mainFirst) != want || warned(subFirst) != want {
			t.Fatalf("%s: the main agent warned %v, the sub-agent %v", name, warned(mainFirst), warned(subFirst))
		}
		if want && !strings.Contains(mainFirst, "About 1 minute left: finish with your report soon.") {
			t.Fatal("the message")
		}
	}
}

func ptr(n int) *int { return &n }

// The agent form keeps the limit on sub-agents per run (a checkbox and a number) and the time warning
// across save and reload; unchecked is no limit.
func TestSubAgentAndTimeWarningSettingsInTheForm(t *testing.T) {
	e := newEnv(t)
	derek := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", derek, "", "")
	p, _ := e.st.CreateProvider(store.ProviderEdit{Name: "LLM", Kind: store.KindOpenAI, Endpoint: "https://llm.example/v1", Key: "k"}, derek)
	e.st.SetProviderModels(p.ID, []string{"m"})
	e.st.LendProvider(p, []int64{ws.ID}, derek)
	g := workflow.Graph{Nodes: []workflow.Node{{ID: "ag", Type: workflow.Agent}}, Links: []workflow.Link{}}
	b, _ := json.Marshal(g)
	e.st.SaveWorkflow(ws.ID, workflow.PR, 0, string(b), derek)
	br := e.signIn("derek")
	save := func(extra url.Values) (int, workflow.AgentSettings) {
		t.Helper()
		w, _ := e.st.Workflow(ws.ID, workflow.PR)
		form := url.Values{"csrf": {br.csrf()}, "version": {itoa(int64(w.Version))}, "name": {"Facts"}, "model": {itoa(p.ID) + ":m"}, "persona": {"custom"},
			"instruction": {"my own"}, "goal": {"my goal"}, "max_turns": {"40"}, "max_minutes": {"20"}, "rerun": {"resume"}, "sub_agents": {"3"}}
		for k, v := range extra {
			form[k] = v
		}
		code, _, _ := br.post("/w/o/r/workflows/pr/nodes/ag", form)
		w, _ = e.st.Workflow(ws.ID, workflow.PR)
		var got workflow.Graph
		json.Unmarshal([]byte(w.Graph), &got)
		a, _ := workflow.AgentSettingsOf(got.Nodes[0], workflow.PR)
		return code, a
	}
	form := func() string { _, page := br.get("/w/o/r/workflows/pr/nodes/ag"); return page }

	// A new block: no limit, the number box hidden, the time warning 10.
	page := form()
	if !strings.Contains(page, `name="limit_sub_agents" data-limit-subs>`) || !strings.Contains(page, `data-when-limit hidden`) || !strings.Contains(page, `name="time_warn_percent" min="0" max="50" value="10"`) ||
		!strings.Contains(page, "Max sub-agents at a time") || !strings.Contains(page, "Warn before the time limit (%)") {
		t.Fatalf("a new block's form:\n%s", page)
	}
	// Checked: the number is kept. Unchecked: no limit, whatever the number says.
	code, a := save(url.Values{"limit_sub_agents": {"on"}, "sub_agents_per_run": {"8"}, "time_warn_percent": {"25"}})
	if code != 200 || a.SubAgentsPerRun != 8 || a.SubAgents != 3 || a.EffectiveTimeWarn() != 25 {
		t.Fatalf("checked: %d %+v", code, a)
	}
	if page := form(); !strings.Contains(page, `data-limit-subs checked>`) || !strings.Contains(page, `name="sub_agents_per_run" min="1" max="500" value="8"`) ||
		strings.Contains(page, `data-when-limit hidden`) || !strings.Contains(page, `name="time_warn_percent" min="0" max="50" value="25"`) {
		t.Fatalf("the form after saving:\n%s", page)
	}
	code, a = save(url.Values{"sub_agents_per_run": {"8"}, "time_warn_percent": {"0"}})
	if code != 200 || a.SubAgentsPerRun != 0 || a.TimeWarnPercent == nil || a.EffectiveTimeWarn() != 0 {
		t.Fatalf("unchecked: %d %+v", code, a)
	}
	if page := form(); strings.Contains(page, `data-limit-subs checked`) || !strings.Contains(page, `name="time_warn_percent" min="0" max="50" value="0"`) {
		t.Fatalf("the form after unchecking:\n%s", page)
	}
	if code, _ := save(url.Values{"limit_sub_agents": {"on"}, "sub_agents_per_run": {"0"}}); code != 400 {
		t.Fatalf("a limit of 0 was accepted: %d", code)
	}
	if code, _ := save(url.Values{"time_warn_percent": {"80"}}); code != 400 {
		t.Fatalf("a warning of 80%%: %d", code)
	}
}
