package web

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/eidonix-ai/autobot/internal/agent"
	"github.com/eidonix-ai/autobot/internal/decisions"
	"github.com/eidonix-ai/autobot/internal/search"
	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// A block is tested from the canvas (shift-click): the form asks for a value for each tag its
// templates use, then the block runs once with them. A test writes nothing: no output tag, no case
// file entry, nothing posted.

// tagField is one value the test form asks for.
type tagField struct {
	Name, About, Value string
	Long               bool // a text box rather than a line
}

var longTags = map[string]bool{"input": true, "input.summary": true, "input.report": true, "input.scratchpad": true, "discord:message": true, "discord:context": true}

// stepTemplates is a node's settings and templates, or an error saying why it cannot be tested.
func (srv *Server) stepTemplates(n workflow.Node, kind string) (any, []string, error) {
	switch n.Type {
	case workflow.Agent:
		s, err := srv.agentSettings(n, kind)
		if err == nil {
			err = s.Check(kind)
		}
		return s, s.Templates(kind), err
	case workflow.Decision:
		s, err := workflow.DecisionSettingsOf(n, kind)
		if err == nil {
			err = s.Check(kind)
		}
		return s, s.Templates(), err
	case workflow.CaseEntry:
		s, err := workflow.CaseEntrySettingsOf(n)
		if err == nil {
			err = s.Check()
		}
		return s, []string{s.Entry}, err
	case workflow.Post:
		s, err := workflow.SettingsOf(n, workflow.DefaultPostFor(kind))
		if err == nil {
			err = s.Check()
		}
		return s, []string{s.Message}, err
	case workflow.Approval:
		s, err := workflow.SettingsOf(n, workflow.DefaultApproval(kind))
		if err == nil {
			err = s.CheckRun()
		}
		return s, []string{s.Question, s.Request}, err
	case workflow.Junction:
		return nil, nil, errors.New("a Node only connects links: there is nothing to test")
	case workflow.Condition, workflow.TestRun:
		return nil, nil, errors.New("this block needs a real pull request or issue: shift-click the event block to run the workflow, perhaps as a dry run")
	}
	return nil, nil, errors.New("an event block is tested once the workflow engine runs workflows")
}

func (s *Server) testForm(w http.ResponseWriter, r *http.Request) {
	ws, kind, _, g, i, ok := s.findNode(w, r)
	if !ok {
		return
	}
	s.renderTest(w, r, http.StatusOK, ws, kind, g.Named(g.Nodes[i]), nil, map[string]any{})
}

// renderTest shows the test form, filled with values (nil: the workspace's own where it has them).
func (s *Server) renderTest(w http.ResponseWriter, r *http.Request, status int, ws *store.Workspace, kind string, n workflow.Node, values map[string]string, data map[string]any) {
	t, _ := workflow.TypeOf(n.Type)
	data["Name"], data["Type"], data["Node"] = workflow.DisplayName(n), t, n
	data["Action"] = ws.Path() + "/workflows/" + kind + "/nodes/" + n.ID + "/test"
	if t.Event { // an event block runs the whole workflow from itself
		data["Action"], data["Kind"] = ws.Path()+"/workflows/"+kind+"/nodes/"+n.ID+"/run", kind
		s.renderFragment(w, r, status, "node_run.html", data)
		return
	}
	_, templates, err := s.stepTemplates(n, kind)
	if err != nil {
		data["Problem"] = err.Error()
		s.renderFragment(w, r, status, "node_test.html", data)
		return
	}
	if values == nil {
		values = map[string]string{}
	}
	about := map[string]string{}
	for _, tag := range workflow.AllTags(kind) {
		about[tag.Name] = tag.About
	}
	var fields []tagField
	// A field is filled from its parent, as a workflow run does: only the parent ({pr}, the link) is
	// asked, and {repo} is the workspace's.
	seen := map[string]bool{"repo": true, "case": true} // filled from the workspace and the item's case file
	for _, tpl := range templates {
		for _, tag := range workflow.TemplateTags(tpl) {
			tag = workflow.CanonicalTag(tag, kind)
			if p := workflow.TagField(tag); p != "" && p != "discord" {
				tag = p
			}
			if seen[tag] {
				continue
			}
			seen[tag] = true
			a, builtIn := about[tag]
			if !builtIn {
				a = "another step's output"
				if strings.Contains(tag, ".") {
					a = "a part of another step's output"
				}
			}
			fields = append(fields, tagField{tag, a, values[tag], longTags[tag] || !builtIn})
		}
	}
	data["Fields"], data["Agent"] = fields, n.Type == workflow.Agent
	if n.Type == workflow.Agent && r.Method == http.MethodPost {
		// An agent's test is posted from the canvas into a tab of its own, which opens on the run; one
		// that could not start shows its error there, as a whole page.
		data["Standalone"], data["Title"] = true, "Test · "+workflow.DisplayName(n)
		s.render(w, r, status, "node_test.html", data)
		return
	}
	s.renderFragment(w, r, status, "node_test.html", data)
}

func (s *Server) runTest(w http.ResponseWriter, r *http.Request) {
	ws, kind, _, g, i, ok := s.findNode(w, r)
	if !ok {
		return
	}
	n := g.Named(g.Nodes[i])
	settings, _, err := s.stepTemplates(n, kind)
	if err != nil {
		s.renderTest(w, r, http.StatusBadRequest, ws, kind, n, nil, map[string]any{})
		return
	}
	values := map[string]string{}
	for k, v := range r.PostForm {
		if tag, ok := strings.CutPrefix(k, "tag_"); ok && len(v) > 0 {
			values[tag] = v[0]
		}
	}
	data := map[string]any{}
	if err := s.fillFromItem(r.Context(), ws, values); err != nil {
		data["Error"] = err.Error() + "."
		s.renderTest(w, r, http.StatusBadRequest, ws, kind, n, values, data)
		return
	}
	// A test publishes nothing: a [[tag]] shows its link's label and says so.
	s.fillLinks(s.testLinks(ws, itemNumber(values)), s.nodeTemplates(n, kind), values)
	switch st := settings.(type) {
	case workflow.CaseEntrySettings:
		data["Entry"], data["Finalize"] = workflow.Render(st.Entry, values), st.Finalize
	case workflow.PostSettings:
		data["Preview"], data["Where"] = workflow.Render(st.Message, values), "Would post: "+st.Where()
	case workflow.ApprovalSettings:
		data["Preview"], data["Where"] = strings.TrimSpace(workflow.Render(st.Request, values)+"\n\n"+workflow.Render(st.Question, values)), "Would ask"
	case workflow.DecisionSettings:
		answer, err := s.decide(r.Context(), ws, st, values)
		if err != nil {
			data["Error"] = err.Error()
		}
		data["Answer"] = answer
	case workflow.AgentSettings:
		// A test is a conversation of its own, never a continuation; a block that continues another's
		// has that block's model.
		st, err := withSourceModel(g, kind, st)
		var id int64
		if err == nil {
			id, err = s.startAgent(context.Background(), ws, kind, n, st, values, me(r), true, itemNumber(values))
		}
		if err != nil {
			data["Error"] = err.Error()
			s.renderTest(w, r, http.StatusBadRequest, ws, kind, n, values, data)
			return
		}
		http.Redirect(w, r, fmt.Sprintf("%s/agent-runs/%d", ws.Path(), id), http.StatusSeeOther)
		return
	}
	s.renderTest(w, r, http.StatusOK, ws, kind, n, values, data)
}

// decisionAnswer is a decision's result, for the test form.
type decisionAnswer struct {
	Choice  string
	Choices []struct {
		Name string
		P    float64
	}
}

// decide asks a decision's model its question, with its context.
func (s *Server) decide(ctx context.Context, ws *store.Workspace, d workflow.DecisionSettings, values map[string]string) (*decisionAnswer, error) {
	p, key, err := s.stepModel(ws, d.Model, true)
	if err != nil {
		return nil, err
	}
	q := decisions.Question{Name: "decision", Type: "choice", Instructions: workflow.Render(d.Question, values)}
	for _, c := range d.Choices {
		q.Options = append(q.Options, decisions.Option{Name: c.Name, Description: c.Description})
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	as, err := decisions.Ask(ctx, p.DecisionAPI(), p.Endpoint, key, d.Model.Model, workflow.Render(d.Context, values), []decisions.Question{q})
	if err != nil {
		return nil, fmt.Errorf("the decision failed: %v", err)
	}
	out := &decisionAnswer{Choice: as[0].Choice}
	for _, c := range d.Choices {
		out.Choices = append(out.Choices, struct {
			Name string
			P    float64
		}{c.Name, as[0].Probabilities[c.Name]})
	}
	return out, nil
}

// stepModel finds the provider of a step's model, checking the workspace may still use it, and
// its key.
func (s *Server) stepModel(ws *store.Workspace, m workflow.ModelRef, decisions bool) (*store.Provider, string, error) {
	if err := s.checkModel(ws, m, decisions); err != nil {
		return nil, "", err
	}
	p, err := s.st.Provider(m.Provider)
	if err != nil {
		return nil, "", err
	}
	key, err := s.st.ProviderKey(p.ID)
	if err != nil {
		return nil, "", err
	}
	return p, key, nil
}

// searcher is the workspace's web search, or nil when it has none lent to it (fetch_url still works).
func (s *Server) searcher(ws *store.Workspace) (func(context.Context, string, int) ([]search.Result, error), error) {
	list, err := s.st.SearchProviders(ws.ID)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	p, err := s.st.Provider(list[0].ProviderID)
	if err != nil {
		return nil, err
	}
	key, err := s.st.ProviderKey(p.ID)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, q string, n int) ([]search.Result, error) {
		return search.Search(ctx, p.Kind, p.Endpoint, key, q, n)
	}, nil
}

// verifier asks a decision model whether a report shows the goal met.
func verifier(p *store.Provider, key, model string) func(context.Context, string, string) (bool, string, error) {
	return func(ctx context.Context, goal, report string) (bool, string, error) {
		q := decisions.Question{Name: "met", Type: "choice", Instructions: "Does the report show that the goal is met?",
			Options: []decisions.Option{{Name: "met", Description: "The report shows the goal is met."},
				{Name: "not_met", Description: "The goal is not yet met, or the report does not show that it is."}}}
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		as, err := decisions.Ask(ctx, p.DecisionAPI(), p.Endpoint, key, model, "Goal: "+goal+"\n\nReport:\n"+report, []decisions.Question{q})
		if err != nil {
			return true, "", err // a check that fails to answer does not hold the agent back
		}
		return as[0].Choice == "met", fmt.Sprintf(" (the check put it at %.0f%% met)", 100*as[0].Probabilities["met"]), nil
	}
}

// runEntry is one message of a run, as its page shows it.
type runEntry struct {
	Role, Label, Text string
	Collapsed         bool
	Calls             []struct{ Name, Args string }
	Usage             string // an agent message: what the model call that made it cost
}

// usageLine says what a model call cost: its input, how much of it the provider reused, its output.
func usageLine(t agent.Turn) string {
	line := fmt.Sprintf("%d in", t.In)
	if t.In > 0 {
		line += fmt.Sprintf(" (%d%% reused)", 100*t.Cached/t.In)
	}
	return line + fmt.Sprintf(", %d out", t.Out)
}

// transcript lists the messages of a conversation's file that a run added (from to to; all that
// there are while it runs), as the run's page shows them. The first is the task.
func transcript(evs []store.ConvEvent, from, to int, running bool) []runEntry {
	conv := agent.Show(evs)
	names := map[string]string{} // tool call id -> tool name, to label results
	entries := []runEntry{{Role: "system", Label: "System prompt", Text: conv.System, Collapsed: true}}
	for i, m := range conv.Messages {
		e := runEntry{Role: m.Role, Text: m.Content}
		switch m.Role {
		case "user":
			e.Label = "Autobot"
			if i == from {
				e.Label = "Task"
			}
		case "assistant":
			e.Label = "Agent"
			for _, c := range m.ToolCalls {
				names[c.ID] = c.Function.Name
				args := c.Function.Arguments
				if len(args) > 400 {
					args = args[:400] + "…"
				}
				e.Calls = append(e.Calls, struct{ Name, Args string }{c.Function.Name, args})
			}
			if t, ok := conv.Usage[i]; ok {
				e.Usage = usageLine(t)
			}
		case "tool":
			first, _, _ := strings.Cut(m.Content, "\n")
			if len(first) > 120 {
				first = first[:120] + "…"
			}
			e.Label, e.Collapsed = names[m.ToolCallID]+": "+first, true
		}
		if i >= from && (i < to || running) {
			entries = append(entries, e)
		}
	}
	return entries
}

// padView is one scratchpad entry, as the run's page shows it.
type padView struct {
	N                                            int
	Importance, Title, Description, Author, Body string
	Report                                       bool
}

// subView is one sub-agent, as the run's page shows it.
type subView struct {
	Name, Title, Status, Line string
	Entries                   []runEntry
	Pad                       []padView
}

// agentRun is an agent run's page: live while it runs (the page refreshes itself), then its record.
func (s *Server) agentRun(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	run, err := s.st.AgentRun(id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && run.WorkspaceID != ws.ID) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	var tr agent.Transcript
	json.Unmarshal([]byte(run.Summary), &tr)
	running := run.Status == "running"
	// The run's own session: the messages it added, from its conversation's file.
	var entries, writing []runEntry
	evs, err := s.st.ReadConv(run.Conversation)
	gone := errors.Is(err, store.ErrNotFound)
	if err != nil && !gone {
		s.fail(w, err)
		return
	}
	if !gone {
		entries = transcript(evs, run.MsgsFrom, run.MsgsTo, running)
	}
	if run.WriterConversation != 0 { // the session that wrote the report, once it has started
		if wevs, err := s.st.ReadConv(run.WriterConversation); err == nil {
			if all := transcript(wevs, 0, 1<<30, true); len(all) > 1 {
				writing = all
			}
		}
	}
	var pad []padView
	if run.Thread != 0 {
		list, err := s.st.Pad(run.Thread).List()
		if err != nil {
			s.fail(w, err)
			return
		}
		for _, e := range list {
			pad = append(pad, padView{e.N, e.Importance, e.Title, e.Description, e.Author, e.Body, e.Report})
		}
	}
	// Its sub-agents, each with a transcript and notes of its own; their turns and tokens are the run's too.
	subRows, err := s.st.Subs(run.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	var subs []subView
	mainLine := ""
	if len(subRows) > 0 {
		mainLine = fmt.Sprintf("The main agent: %d turn%s · %d tokens in%s, %d out", tr.Turns, plural(tr.Turns), tr.In, cacheSuffix(tr.In, tr.Cached), tr.Out)
	}
	for _, row := range subRows {
		v := subView{Name: row.Name, Title: row.Title, Status: map[string]string{"running": "Running", agent.Met: "Finished (met)", agent.Blocked: "Finished (blocked)",
			agent.Budget: "Out of budget", agent.Unfinished: "Stopped without finishing", agent.Failed: "Failed", agent.Stopped: "Stopped"}[row.Status]}
		var st agent.Transcript
		json.Unmarshal([]byte(row.Summary), &st)
		v.Line = fmt.Sprintf("%d turn%s · %d tokens in%s, %d out", st.Turns, plural(st.Turns), st.In, cacheSuffix(st.In, st.Cached), st.Out)
		tr.Turns, tr.In, tr.Out, tr.Cached = tr.Turns+st.Turns, tr.In+st.In, tr.Out+st.Out, tr.Cached+st.Cached
		if evs, err := s.st.ReadConv(row.Conversation); err == nil {
			v.Entries = transcript(evs, 0, 1<<30, true)
		}
		if notes, err := s.st.SubNotes(run.ID, row.Name); err == nil {
			for _, e := range notes {
				v.Pad = append(v.Pad, padView{e.N, e.Importance, e.Title, e.Description, e.Author, e.Body, false})
			}
		}
		subs = append(subs, v)
	}
	var report, summary strings.Builder
	if tr.Report != "" {
		s.md.Convert([]byte(tr.Report), &report)
	}
	// A summary that is the report itself (a block with no closing step) has no panel of its own.
	if tr.Summary != "" && tr.Summary != tr.Report {
		s.md.Convert([]byte(tr.Summary), &summary)
	}
	outcome := map[string]string{"running": "Running", agent.Met: "Goal met", agent.Blocked: "Blocked",
		agent.Budget: "Out of budget", agent.Unfinished: "Stopped without finishing", agent.Failed: "Failed", agent.Stopped: "Stopped"}[run.Status]
	dot := map[string]string{"running": "busy", agent.Met: "on"}[run.Status]
	if dot == "" {
		dot = "off"
	}
	if run.Status == agent.Failed && tr.Error == "" {
		tr.Error = "Autobot restarted while it ran."
	}
	tr.Report, tr.Summary = "", ""
	cache := cacheLine(tr.In, tr.Cached)
	data := map[string]any{"Title": run.NodeName + " · " + ws.Name, "WS": ws, "Run": run, "T": tr, "Entries": entries,
		"Outcome": outcome, "Dot": dot, "Report": template.HTML(report.String()), "Summary": template.HTML(summary.String()), "Cache": cache, "Gone": gone,
		"Writing": writing, "HasPad": run.Thread != 0, "Pad": pad, "Subs": subs, "MainLine": mainLine,
		"Took": run.FinishedAt.Sub(run.StartedAt).Round(time.Second).String()}
	if w := tr.Writing; w != nil {
		data["WriterLine"] = fmt.Sprintf("Writing the report: %d turn%s · %d tokens in%s, %d out", w.Turns, plural(w.Turns), w.In, cacheSuffix(w.In, w.Cached), w.Out)
	}
	s.render(w, r, http.StatusOK, "agent_run.html", data)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// cacheLine says how much of a session's input the provider reused.
func cacheLine(in, cached int) string {
	if in <= 0 {
		return ""
	}
	return fmt.Sprintf("cache: %d%% of input reused", 100*cached/in)
}

func cacheSuffix(in, cached int) string {
	if line := cacheLine(in, cached); line != "" {
		return " (" + line + ")"
	}
	return ""
}

// itemNumber is the pull request or issue a set of tag values is about, or 0.
func itemNumber(values map[string]string) int {
	n, _ := strconv.Atoi(cmp.Or(values["pr:number"], values["issue:number"]))
	return n
}

// fillFromItem fills a test's tags the way a workflow run does: from the pull request or issue its
// link names (title, description, author, files, diff…), the workspace's repository and charter,
// and the item's case file. Values the person typed are kept.
func (s *Server) fillFromItem(ctx context.Context, ws *store.Workspace, values map[string]string) error {
	set := func(k, v string) {
		if values[k] == "" {
			values[k] = v
		}
	}
	set("repo", ws.Name)
	set("repo:opt_out", s.optOut(ws))
	if c, err := s.st.Charter(ws.ID, 0); err == nil {
		set("charter", c.Body)
	}
	set("machines", s.machinesSummary(ws))
	kind, link := workflow.PR, values["pr"]
	if link == "" {
		kind, link = workflow.Issue, values["issue"]
	}
	m := itemLinkRE.FindStringSubmatch(link)
	if link == "" {
		return nil
	}
	if m == nil || !strings.EqualFold(m[1], ws.Name) {
		return fmt.Errorf("the link must be to a pull request or issue of %s", ws.Name)
	}
	number, _ := strconv.Atoi(m[3])
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	// Autobot's own copy first; GitHub only for what it does not have.
	it := s.localItem(ctx, ws, kind, number)
	if it == nil {
		var err error
		if it, err = s.loadItem(ctx, ws, kind, number); err != nil {
			return fmt.Errorf("reading #%d from GitHub: %v", number, err)
		}
	}
	for k, v := range it.values(ws) {
		set(k, v)
	}
	if c, err := s.st.CaseByNumber(ws.ID, number); err == nil {
		for k, v := range s.caseTags(kind, c) {
			set(k, v)
		}
	}
	return nil
}

var itemLinkRE = regexp.MustCompile(`github\.com/([A-Za-z0-9._-]+/[A-Za-z0-9._-]+)/(pull|issues)/(\d+)`)
