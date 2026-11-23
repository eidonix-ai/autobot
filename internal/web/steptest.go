package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"regexp"
	"slices"
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

var longTags = map[string]bool{"pr_body": true, "pr_diff": true, "pr_files": true, "issue_body": true,
	"project_charter": true, "case_file": true, "previous_output": true}

// stepTemplates is a node's settings and templates, or an error saying why it cannot be tested.
func stepTemplates(n workflow.Node, kind string) (any, []string, error) {
	switch n.Type {
	case workflow.Agent:
		s, err := workflow.AgentSettingsOf(n, kind)
		if err == nil {
			err = s.Check(kind)
		}
		return s, s.Templates(), err
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
	}
	return nil, nil, errors.New("an event block is tested once the workflow engine runs workflows")
}

func (s *Server) testForm(w http.ResponseWriter, r *http.Request) {
	ws, kind, _, g, i, ok := s.findNode(w, r)
	if !ok {
		return
	}
	s.renderTest(w, r, http.StatusOK, ws, kind, g.Nodes[i], nil, map[string]any{})
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
	_, templates, err := stepTemplates(n, kind)
	if err != nil {
		data["Problem"] = err.Error()
		s.renderFragment(w, r, status, "node_test.html", data)
		return
	}
	if values == nil {
		values = map[string]string{"repo": ws.Name}
		if c, err := s.st.Charter(ws.ID, 0); err == nil {
			values["project_charter"] = c.Body
		}
	}
	about := map[string]string{}
	for _, tag := range workflow.Tags(kind) {
		about[tag.Name] = tag.About
	}
	var fields []tagField
	seen := map[string]bool{}
	for _, tpl := range templates {
		for _, tag := range workflow.TemplateTags(tpl) {
			if seen[tag] {
				continue
			}
			seen[tag] = true
			a, builtIn := about[tag]
			if !builtIn {
				a = "another step's output"
			}
			fields = append(fields, tagField{tag, a, values[tag], longTags[tag] || !builtIn})
		}
	}
	data["Fields"], data["Agent"] = fields, n.Type == workflow.Agent
	s.renderFragment(w, r, status, "node_test.html", data)
}

func (s *Server) runTest(w http.ResponseWriter, r *http.Request) {
	ws, kind, _, g, i, ok := s.findNode(w, r)
	if !ok {
		return
	}
	n := g.Nodes[i]
	settings, _, err := stepTemplates(n, kind)
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
	switch st := settings.(type) {
	case workflow.CaseEntrySettings:
		data["Entry"], data["Finalize"] = workflow.Render(st.Entry, values), st.Finalize
	case workflow.DecisionSettings:
		answer, err := s.decide(r.Context(), ws, st, values)
		if err != nil {
			data["Error"] = err.Error()
		}
		data["Answer"] = answer
	case workflow.AgentSettings:
		id, err := s.startAgent(ws, kind, n, st, values, me(r))
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

const preamble = `You are Autobot, an agent working on the GitHub repository %s.
You work in turns: call tools to find things out, several at once when they do not depend on each other, and keep going until the goal is met.
Make a plan with update_plan and keep it current. End only by calling finish: with status met once the goal is met,
or blocked, saying why, when it cannot be met. The report you give finish is your final answer and the only part people read,
so make it complete and specific.`

var prLinkRE = regexp.MustCompile(`github\.com/([A-Za-z0-9._-]+/[A-Za-z0-9._-]+)/pull/(\d+)`)

// startAgent starts an agent run in the background and returns its id.
func (s *Server) startAgent(ws *store.Workspace, kind string, n workflow.Node, a workflow.AgentSettings, values map[string]string, by *store.User) (int64, error) {
	p, key, err := s.stepModel(ws, a.Model, false)
	if err != nil {
		return 0, err
	}
	if p.Kind != store.KindOpenAI {
		return 0, errors.New("agents run on OpenAI-compatible providers for now; Anthropic's API comes next")
	}
	model := &agent.OpenAI{Endpoint: p.Endpoint, Key: key, Model: a.Model.Model}
	system := fmt.Sprintf(preamble, ws.Name)
	if per := workflow.PersonaOf(a.Persona); per != nil {
		system += "\n\n" + per.Prompt
	}
	if extra := strings.TrimSpace(workflow.Render(a.SystemPrompt, values)); extra != "" {
		system += "\n\n" + extra
	}
	goal := workflow.Render(a.Goal, values)
	cfg := agent.Config{Model: model, System: system, Goal: goal, MaxTurns: a.MaxTurns,
		Task: workflow.Render(a.Instruction, values) + "\n\nDone when: " + goal}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	item, commit, token := 0, "", ""
	if id, err := s.st.GitHub(ws.ID); err == nil {
		token = id.Token
	}
	prTools := false
	if m := prLinkRE.FindStringSubmatch(values["pr_link"]); m != nil && strings.EqualFold(m[1], ws.Name) {
		num, _ := strconv.Atoi(m[2])
		info, err := s.gh.PullRequest(ctx, token, ws.Name, num)
		if err != nil {
			return 0, fmt.Errorf("reading pull request #%d: %v", num, err)
		}
		files, err := s.gh.PullRequestFiles(ctx, token, ws.Name, num)
		if err != nil {
			return 0, fmt.Errorf("reading pull request #%d's files: %v", num, err)
		}
		item, commit, prTools = num, info.HeadSHA, true
		cfg.Tools = append(cfg.Tools, agent.PRTools(&agent.PR{GitHub: s.gh, Token: token, Repo: ws.Name, Info: info, Files: files})...)
	}
	// With machines to use, the agent gets a shell on one, and reads the code there (at the pull
	// request's head, or the default branch's) instead of through GitHub.
	sess, note, err := s.openSandbox(ctx, ws, a, token, commit, by)
	if err != nil {
		return 0, err
	}
	started := false
	if sess != nil {
		defer func() {
			if !started {
				sess.Close()
			}
		}()
		cfg.System += "\n\n" + note
		if prTools {
			cfg.Tools = slices.DeleteFunc(cfg.Tools, func(t agent.Tool) bool { return t.Spec.Name == "read_file" || t.Spec.Name == "list_files" })
		}
		cfg.Tools = append(cfg.Tools, agent.SandboxTools(sess)...)
	}
	if a.Internet {
		searcher, err := s.searcher(ws)
		if err != nil {
			return 0, err
		}
		cfg.Tools = append(cfg.Tools, agent.WebTools(searcher)...)
	}
	if a.Verify.Set() {
		vp, vkey, err := s.stepModel(ws, a.Verify, true)
		if err != nil {
			return 0, fmt.Errorf("the check's model: %v", err)
		}
		cfg.Verify = verifier(vp, vkey, a.Verify.Model)
	}
	id, err := s.st.StartAgentRun(store.AgentRun{WorkspaceID: ws.ID, Kind: kind, NodeID: n.ID, NodeName: workflow.DisplayName(n),
		Item: item, Test: true, Model: a.Model.Model}, by)
	if err != nil {
		return 0, err
	}
	saved := 0 // messages stored so far: each is written once
	save := func(tr *agent.Transcript, status string) {
		var fresh [][]byte
		for _, m := range tr.Messages[saved:] {
			b, _ := json.Marshal(m)
			fresh = append(fresh, b)
		}
		sum := *tr
		sum.Messages = nil
		b, _ := json.Marshal(sum)
		if err := s.st.SaveAgentRun(id, status, string(b), saved, fresh); err != nil {
			log.Printf("agent run %d: %v", id, err)
			return
		}
		saved = len(tr.Messages)
	}
	cfg.Save = func(tr *agent.Transcript) { save(tr, "running") }
	started = true
	go func() {
		if sess != nil {
			defer sess.Close()
			// The agent's time starts once its sandbox has the code and takes commands.
			rctx, cancel := context.WithTimeout(context.Background(), sandboxSetup)
			err := sess.Ready(rctx)
			cancel()
			if err != nil {
				save(&agent.Transcript{Outcome: agent.Failed, Error: "the sandbox did not start: " + err.Error()}, agent.Failed)
				return
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(a.MaxMinutes)*time.Minute)
		defer cancel()
		tr := agent.Run(ctx, cfg, nil)
		save(tr, tr.Outcome)
	}()
	return id, nil
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

// runEntry is one message of a transcript, as the run page shows it.
type runEntry struct {
	Role, Label, Text string
	Collapsed         bool
	Calls             []struct{ Name, Args string }
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
	msgs, err := s.st.AgentMessages(run.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, b := range msgs {
		var m agent.Message
		json.Unmarshal(b, &m)
		tr.Messages = append(tr.Messages, m)
	}
	names := map[string]string{} // tool call id -> tool name, to label results
	var entries []runEntry
	for i, m := range tr.Messages {
		e := runEntry{Role: m.Role, Text: m.Content}
		switch m.Role {
		case "system":
			e.Label, e.Collapsed = "System prompt", true
		case "user":
			e.Label = "Autobot"
			if i == 1 {
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
		case "tool":
			first, _, _ := strings.Cut(m.Content, "\n")
			if len(first) > 120 {
				first = first[:120] + "…"
			}
			e.Label, e.Collapsed = names[m.ToolCallID]+": "+first, true
		}
		entries = append(entries, e)
	}
	var report strings.Builder
	if tr.Report != "" {
		s.md.Convert([]byte(tr.Report), &report)
	}
	outcome := map[string]string{"running": "Running", agent.Met: "Goal met", agent.Blocked: "Blocked",
		agent.Budget: "Out of budget", agent.Unfinished: "Stopped without finishing", agent.Failed: "Failed"}[run.Status]
	dot := map[string]string{"running": "busy", agent.Met: "on"}[run.Status]
	if dot == "" {
		dot = "off"
	}
	if run.Status == agent.Failed && tr.Error == "" {
		tr.Error = "Autobot restarted while it ran."
	}
	tr.Report = ""
	data := map[string]any{"Title": run.NodeName + " · " + ws.Name, "WS": ws, "Run": run, "T": tr, "Entries": entries,
		"Outcome": outcome, "Dot": dot, "Report": template.HTML(report.String()),
		"Took": run.FinishedAt.Sub(run.StartedAt).Round(time.Second).String()}
	s.render(w, r, http.StatusOK, "agent_run.html", data)
}
