package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/eidonix-ai/autobot/internal/agent"
	"github.com/eidonix-ai/autobot/internal/github"
	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// The engine runs a workflow on one pull request or issue, from one of its event nodes. A step
// runs once everything linked into it has finished, and only if at least one of those links fired:
// a link fires when its node finished on that output (a decision fires only its choice's links).
// A step none of whose links fired is skipped, and so on downstream, which is how a decision routes
// and how a fork rejoins. Independent steps run at the same time. Each step's output is the
// {previous_output} of the steps it links to (several, when links merge, each under its node's
// name), and a step's "Output to" tag is kept for the case. A dry run writes nothing: no case file,
// no output tags, nothing posted.

const maxDiff = 200_000 // characters of a pull request's diff given to {pr_diff}

// item is what a run knows of its pull request or issue.
type item struct {
	kind     string
	number   int
	title    string
	state    string
	body     string
	author   string
	url      string
	pr       *github.PullRequest // pull requests only
	files    []github.ChangedFile
	assoc    string // the author's standing in the project, as GitHub says it
	labels   []string
	comments []github.Comment
}

// loadItem reads a pull request or issue from GitHub.
func (s *Server) loadItem(ctx context.Context, ws *store.Workspace, kind string, number int) (*item, error) {
	token, err := s.token(ws)
	if err != nil {
		return nil, err
	}
	it := &item{kind: kind, number: number}
	if kind == workflow.PR {
		pr, err := s.gh.PullRequest(ctx, token, ws.Name, number)
		if err != nil {
			return nil, err
		}
		if it.files, err = s.gh.PullRequestFiles(ctx, token, ws.Name, number); err != nil {
			return nil, err
		}
		it.pr, it.title, it.state, it.body, it.author, it.url = pr, pr.Title, pr.State, pr.Body, pr.Author, pr.URL
		it.assoc, it.labels = pr.Association, pr.Labels
	} else {
		gi, err := s.gh.Item(ctx, token, ws.Name, number)
		if err != nil {
			return nil, err
		}
		if gi.Kind != "issue" {
			return nil, fmt.Errorf("#%d is a pull request; run the pull request workflow on it", number)
		}
		it.title, it.state, it.body, it.author, it.url = gi.Title, gi.State, gi.Body, gi.Author, gi.URL
		it.assoc, it.labels = gi.Association, gi.Labels
	}
	if it.comments, err = s.gh.Comments(ctx, token, ws.Name, number); err != nil {
		return nil, err
	}
	return it, nil
}

// values are an item's built-in tags.
func (it *item) values(ws *store.Workspace) map[string]string {
	v := map[string]string{"repo": ws.Name}
	n := strconv.Itoa(it.number)
	if it.kind == workflow.PR {
		diff := github.Diff(it.files)
		if len(diff) > maxDiff {
			diff = strings.ToValidUTF8(diff[:maxDiff], "") + "\n[the diff goes on; read the rest with the pr_diff tool]"
		}
		var files []string
		for _, f := range it.files {
			files = append(files, fmt.Sprintf("%s (%s, +%d -%d)", f.Path, f.Status, f.Additions, f.Deletions))
		}
		v["pr_link"], v["pr_number"], v["pr_title"], v["pr_body"], v["pr_author"] = it.url, n, it.title, it.body, it.author
		v["pr_diff"], v["pr_files"] = diff, strings.Join(files, "\n")
	} else {
		v["issue_link"], v["issue_number"], v["issue_title"], v["issue_body"], v["issue_author"] = it.url, n, it.title, it.body, it.author
	}
	return v
}

// verdict says whether the event's conditions would have started the workflow on the item, and
// how each stood. A manual run goes ahead either way; this says what the poller would have done.
func verdict(n workflow.Node, it *item, caseExists bool) (bool, []string) {
	ev, _ := workflow.EventSettingsOf(n)
	ok := true
	var lines []string
	check := func(pass bool, yes, no string) {
		if pass {
			lines = append(lines, "✓ "+yes)
		} else {
			ok = false
			lines = append(lines, "✗ "+no)
		}
	}
	note := func(s string) { lines = append(lines, "· "+s) }
	check(it.state == "open", "it is open", "it is closed")
	switch n.Type {
	case "pr_ready":
		check(!it.pr.Draft, "it is ready for review", "it is a draft")
	case "pr_draft":
		check(it.pr.Draft, "it is a draft", "it is ready for review, not a draft")
	case "pr_activity":
		note("which activity started it is not checked in a manual run")
		if ev.CaseExists {
			check(caseExists, "it has a case file", "it has no case file yet")
		}
	}
	stopped, by := workflow.Stops(it.body, ev.StopTag), "its description"
	for _, c := range it.comments {
		if !stopped && workflow.Stops(c.Body, ev.StopTag) {
			stopped, by = true, "a comment by "+c.Author
		}
	}
	check(!stopped, "no stop tag “"+ev.StopTag+"”", "the stop tag “"+ev.StopTag+"” is in "+by)
	summoned := false
	for _, c := range it.comments {
		summoned = summoned || workflow.Summons(c.Body, ev.SummonPhrase, ev.StopTag)
	}
	if ev.Mode == "summoned" {
		check(summoned, "summoned with “"+ev.SummonPhrase+"”", "not summoned: no comment has “"+ev.SummonPhrase+"”")
	}
	if it.kind != workflow.PR {
		return ok, lines
	}
	lines0 := 0
	for _, f := range it.files {
		lines0 += f.Additions + f.Deletions
	}
	for _, c := range workflow.PRFilters(ev, workflow.PRFacts{Author: it.author, Base: it.pr.Base, Lines: lines0, Summoned: summoned}) {
		check(c.Pass, c.Text, c.Text)
	}
	if ev.WaitCI != "no" {
		note("waiting for CI is not checked in a manual run")
	}
	if ev.TrustedOnly && ev.Mode == "summoned" {
		note("whether the summons came from the author or a collaborator is not checked yet")
	}
	return ok, lines
}

// runState is one run as its driver holds it.
type runState struct {
	ws      *store.Workspace
	run     *store.WorkflowRun
	g       workflow.Graph
	it      *item
	builtIn map[string]string
	tags    map[string]string // output tags: the case's, then this run's
	by      *store.User
}

// startWorkflow starts a run of a kind's workflow on an item from an event node, and returns its id.
func (s *Server) startWorkflow(ctx context.Context, ws *store.Workspace, kind, eventNode string, number int, dry bool, by *store.User) (int64, error) {
	stored, err := s.st.Workflow(ws.ID, kind)
	if err != nil {
		return 0, err
	}
	g, err := loadGraph(stored)
	if err != nil {
		return 0, err
	}
	var ev *workflow.Node
	for i, n := range g.Nodes {
		if n.ID == eventNode {
			ev = &g.Nodes[i]
		}
	}
	if ev == nil {
		return 0, errors.New("that block is not on the canvas any more")
	}
	if t, _ := workflow.TypeOf(ev.Type); !t.Event {
		return 0, errors.New("a workflow runs from an event block")
	}
	it, err := s.loadItem(ctx, ws, kind, number)
	if err != nil {
		return 0, err
	}
	rs := &runState{ws: ws, g: g, it: it, builtIn: it.values(ws), tags: map[string]string{}, by: by}
	var caseFile *store.Case
	if c, err := s.st.CaseByNumber(ws.ID, number); err == nil {
		caseFile = c
		if rs.tags, err = s.st.CaseValues(c.UUID); err != nil {
			return 0, err
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return 0, err
	}
	if c, err := s.st.Charter(ws.ID, 0); err == nil {
		rs.builtIn["project_charter"] = c.Body
	}
	pass, lines := verdict(*ev, it, caseFile != nil)
	head := "The event would have started it."
	if !pass {
		head = "The event would not have started it; run by hand anyway."
	}
	graph, _ := json.Marshal(g)
	var ids []string
	for _, n := range g.Nodes {
		ids = append(ids, n.ID)
	}
	r := store.WorkflowRun{WorkspaceID: ws.ID, Kind: kind, EventNode: eventNode, Item: number, DryRun: dry, Graph: string(graph),
		Note: head + "\n" + strings.Join(lines, "\n")}
	id, err := s.st.StartWorkflowRun(r, ids, by)
	if err != nil {
		return 0, err
	}
	run, err := s.st.WorkflowRun(id)
	if err != nil {
		return 0, err
	}
	rs.run = run
	go func() {
		defer survive(fmt.Sprintf("workflow run %d", rs.run.ID), func(msg string) { s.st.FinishWorkflowRun(rs.run.ID, "failed") })
		s.drive(rs)
	}()
	return id, nil
}

// drive runs a run's steps until none can run, then ends it.
func (s *Server) drive(rs *runState) {
	steps, err := s.st.WorkflowSteps(rs.run.ID)
	if err != nil {
		log.Printf("workflow run %d: %v", rs.run.ID, err)
		return
	}
	save := func(st store.WorkflowStep) {
		steps[st.NodeID] = st
		if err := s.st.SetWorkflowStep(rs.run.ID, st); err != nil {
			log.Printf("workflow run %d: %v", rs.run.ID, err)
		}
	}
	now := time.Now()
	nodes := map[string]workflow.Node{}
	incoming := map[string][]workflow.Link{}
	for _, n := range rs.g.Nodes {
		nodes[n.ID] = n
		if t, _ := workflow.TypeOf(n.Type); t.Event && steps[n.ID].Status == "pending" {
			st := steps[n.ID]
			st.Status, st.StartedAt, st.FinishedAt = "skipped", now, now
			if n.ID == rs.run.EventNode {
				st.Status, st.Port = "done", "out"
				st.Output = fmt.Sprintf("%s #%d: %s", workflow.DisplayName(n), rs.it.number, rs.it.title)
			}
			save(st)
		}
	}
	for _, l := range rs.g.Links {
		incoming[l.To] = append(incoming[l.To], l)
	}
	type finished struct {
		id string
		o  stepOutcome
	}
	results := make(chan finished)
	running := 0
	// A resumed run picks up its steps that were waiting for a person.
	for _, n := range rs.g.Nodes {
		if steps[n.ID].Status == "waiting" && n.Type == workflow.Approval {
			running++
			go func(n workflow.Node) {
				o := stepOutcome{err: errors.New("the step crashed")}
				defer func() { results <- finished{n.ID, o} }()
				defer survive(fmt.Sprintf("workflow run %d step %s", rs.run.ID, n.ID), nil)
				ctx, cancel := context.WithTimeout(context.Background(), 15*24*time.Hour)
				defer cancel()
				o = s.awaitApproval(ctx, rs, n)
			}(n)
		}
	}
	for {
		for progressed := true; progressed; {
			progressed = false
			for _, n := range rs.g.Nodes {
				st := steps[n.ID]
				if st.Status != "pending" {
					continue
				}
				ready, fired := true, []workflow.Link(nil)
				for _, l := range incoming[n.ID] {
					switch src := steps[l.From]; src.Status {
					case "done":
						if src.Port == l.Out {
							fired = append(fired, l)
						}
					case "skipped", "failed":
					default:
						ready = false
					}
				}
				if !ready {
					continue
				}
				progressed = true
				st.StartedAt = time.Now()
				if len(fired) == 0 {
					st.Status, st.FinishedAt = "skipped", st.StartedAt
					save(st)
					continue
				}
				st.Status = "running"
				save(st)
				running++
				prev := previousOutput(fired, steps, nodes)
				go func(n workflow.Node, prev string) {
					o := stepOutcome{err: errors.New("the step crashed")}
					defer func() { results <- finished{n.ID, o} }()
					defer survive(fmt.Sprintf("workflow run %d step %s", rs.run.ID, n.ID), nil)
					o = s.runStep(rs, n, prev)
				}(n, prev)
			}
		}
		if running == 0 {
			break
		}
		f := <-results
		running--
		st := steps[f.id]
		st.FinishedAt, st.Port, st.Output, st.AgentRun = time.Now(), f.o.port, f.o.output, f.o.agentRun
		st.Status = "done"
		if f.o.err != nil {
			st.Status, st.Error, st.Port = "failed", f.o.err.Error(), ""
		} else if f.o.note != "" {
			st.Error = f.o.note
		}
		save(st)
		if st.Status == "done" {
			s.keepTag(rs, nodes[f.id], st.Output)
		}
	}
	status := "done"
	for _, st := range steps {
		if st.Status == "failed" {
			status = "failed"
		}
	}
	if err := s.st.FinishWorkflowRun(rs.run.ID, status); err != nil {
		log.Printf("workflow run %d: %v", rs.run.ID, err)
	}
}

// previousOutput is what the links that fired into a step bring it: one output as it is, several
// each under the name of the node it came from.
func previousOutput(fired []workflow.Link, steps map[string]store.WorkflowStep, nodes map[string]workflow.Node) string {
	if len(fired) == 1 {
		return steps[fired[0].From].Output
	}
	var parts []string
	for _, l := range fired {
		parts = append(parts, "## "+workflow.DisplayName(nodes[l.From])+"\n\n"+steps[l.From].Output)
	}
	return strings.Join(parts, "\n\n")
}

// outputTo is a node's output tag, or "".
func outputTo(n workflow.Node, kind string) string {
	switch n.Type {
	case workflow.Agent:
		a, _ := workflow.AgentSettingsOf(n, kind)
		return a.OutputTo
	case workflow.Decision:
		d, _ := workflow.DecisionSettingsOf(n, kind)
		return d.OutputTo
	}
	return ""
}

// keepTag stores a finished step's output under its output tag: for the rest of the run, and for
// the case unless the run is dry.
func (s *Server) keepTag(rs *runState, n workflow.Node, output string) {
	name := outputTo(n, rs.run.Kind)
	if name == "" {
		return
	}
	rs.tags[name] = output
	if rs.run.DryRun {
		return
	}
	c, err := s.st.CaseByNumber(rs.ws.ID, rs.it.number)
	if errors.Is(err, store.ErrNotFound) {
		if c, err = s.caseForRun(rs); err != nil {
			log.Printf("workflow run %d: %v", rs.run.ID, err)
			return
		}
	}
	if err == nil {
		err = s.st.SetCaseValue(c.UUID, name, output, n.ID)
	}
	if err != nil {
		log.Printf("workflow run %d: keeping {%s}: %v", rs.run.ID, name, err)
	}
}

func (s *Server) caseForRun(rs *runState) (*store.Case, error) {
	et, err := time.LoadLocation("America/New_York")
	if err != nil {
		et = time.UTC
	}
	c, _, err := s.st.OpenCase(rs.ws, rs.it.number, rs.it.kind, rs.it.title,
		fmt.Sprintf("Opened by a workflow run, %s.", time.Now().In(et).Format("Jan 2, 2006 3:04 PM MST")))
	return c, err
}

// stepOutcome is how a step ended: the output it took and what it produced. note is something to
// show beside a step that still counts as done, such as an agent that finished blocked.
type stepOutcome struct {
	port, output, note string
	agentRun           int64
	err                error
}

// runStep runs one step with the run's tags and the output of the steps before it.
func (s *Server) runStep(rs *runState, n workflow.Node, prev string) stepOutcome {
	values := map[string]string{}
	for k, v := range rs.tags {
		values[k] = v
	}
	for k, v := range rs.builtIn {
		values[k] = v
	}
	values["previous_output"] = prev
	if c, err := s.st.CaseByNumber(rs.ws.ID, rs.it.number); err == nil {
		values["case_file"] = c.Contents
	}
	kind := rs.run.Kind
	limit := 4 * time.Hour
	if n.Type == workflow.Approval {
		limit = 15 * 24 * time.Hour // a person may take up to two weeks
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	switch n.Type {
	case workflow.Decision:
		d, err := workflow.DecisionSettingsOf(n, kind)
		if err == nil {
			err = d.Check(kind)
		}
		if err != nil {
			return stepOutcome{err: err}
		}
		ans, err := s.decide(ctx, rs.ws, d, values)
		if err != nil {
			return stepOutcome{err: err}
		}
		out := ans.Choice
		if d.Output == "probabilities" {
			for _, c := range ans.Choices {
				out += fmt.Sprintf("\n%s: %.1f%%", c.Name, 100*c.P)
			}
		}
		return stepOutcome{port: ans.Choice, output: out}
	case workflow.CaseEntry:
		c, err := workflow.CaseEntrySettingsOf(n)
		if err == nil {
			err = c.Check()
		}
		if err != nil {
			return stepOutcome{err: err}
		}
		text := strings.TrimSpace(workflow.Render(c.Entry, values))
		if rs.run.DryRun {
			return stepOutcome{port: "out", output: text, note: "dry run: not written"}
		}
		if err := s.writeEntry(rs, n, text, c.Finalize); err != nil {
			return stepOutcome{err: err}
		}
		return stepOutcome{port: "out", output: text}
	case workflow.Agent:
		a, err := workflow.AgentSettingsOf(n, kind)
		if err == nil {
			err = a.Check(kind)
		}
		if err != nil {
			return stepOutcome{err: err}
		}
		return s.agentStep(ctx, rs, n, a, values)
	case workflow.GitHubComment:
		c, err := workflow.SettingsOf(n, workflow.DefaultGitHubComment())
		if err == nil {
			err = c.Check()
		}
		if err != nil {
			return stepOutcome{err: err}
		}
		return s.commentStep(ctx, rs, n, c, strings.TrimSpace(workflow.Render(c.Message, values)))
	case workflow.Condition:
		c, err := workflow.SettingsOf(n, workflow.DefaultCondition())
		if err == nil {
			err = c.Check(kind)
		}
		if err != nil {
			return stepOutcome{err: err}
		}
		f := workflow.ItemFacts{Author: rs.it.author, Association: rs.it.assoc, Labels: rs.it.labels, Title: rs.it.title, Body: rs.it.body, Tags: rs.tags}
		for _, cf := range rs.it.files {
			f.Files, f.Lines = append(f.Files, cf.Path), f.Lines+cf.Additions+cf.Deletions
		}
		ok, notes := c.Holds(f)
		port := "no"
		if ok {
			port = "yes"
		}
		return stepOutcome{port: port, output: strings.Join(notes, "\n")}
	case workflow.TestRun:
		t, err := workflow.SettingsOf(n, workflow.DefaultTestRun())
		if err == nil {
			err = t.Check()
		}
		if err != nil {
			return stepOutcome{err: err}
		}
		return s.testStep(ctx, rs, t, workflow.Render(t.Script, values))
	case workflow.DiscordPost:
		d, err := workflow.SettingsOf(n, workflow.DefaultDiscordPost())
		if err == nil {
			err = d.Check()
		}
		if err != nil {
			return stepOutcome{err: err}
		}
		return s.discordStep(ctx, rs, d, strings.TrimSpace(workflow.Render(d.Message, values)))
	case workflow.Approval:
		a, err := workflow.SettingsOf(n, workflow.DefaultApproval(kind))
		if err == nil {
			err = a.Check()
		}
		if err != nil {
			return stepOutcome{err: err}
		}
		return s.approvalStep(ctx, rs, n, a, strings.TrimSpace(workflow.Render(a.Request, values)))
	}
	return stepOutcome{err: fmt.Errorf("a %s block cannot run yet", n.Type)}
}

// commentStep posts a comment on the item, or, in a trial, writes what it would post into the case
// file. Keeping one comment, a later run edits the node's earlier comment.
func (s *Server) commentStep(ctx context.Context, rs *runState, n workflow.Node, c workflow.GitHubCommentSettings, text string) stepOutcome {
	if text == "" {
		return stepOutcome{err: errors.New("the comment came out empty")}
	}
	if rs.run.DryRun {
		return stepOutcome{port: "out", output: text, note: "dry run: not posted"}
	}
	if c.Trial {
		if err := s.writeEntry(rs, n, "**Would post on GitHub:**\n\n"+text, true); err != nil {
			return stepOutcome{err: err}
		}
		return stepOutcome{port: "out", output: text, note: "trial run: written to the case file, not posted"}
	}
	token, err := s.token(rs.ws)
	if err != nil {
		return stepOutcome{err: err}
	}
	if token == "" {
		return stepOutcome{err: errors.New("posting needs the workspace's GitHub identity: set it in the workspace's Settings")}
	}
	cs, err := s.st.CaseByNumber(rs.ws.ID, rs.it.number)
	if errors.Is(err, store.ErrNotFound) {
		cs, err = s.caseForRun(rs)
	}
	if err != nil {
		return stepOutcome{err: err}
	}
	if c.KeepOne {
		if id, _ := s.st.GitHubComment(cs.UUID, n.ID); id != 0 {
			url, err := s.gh.EditComment(ctx, token, rs.ws.Name, id, text)
			if err == nil {
				s.st.SetGitHubComment(cs.UUID, n.ID, id, url)
				return stepOutcome{port: "out", output: text, note: "edited " + url}
			}
			if !errors.Is(err, github.ErrNoRepo) { // deleted on GitHub: post a new one
				return stepOutcome{err: fmt.Errorf("editing the earlier comment: %v", err)}
			}
		}
	}
	id, url, err := s.gh.Comment(ctx, token, rs.ws.Name, rs.it.number, text)
	if err != nil {
		return stepOutcome{err: err}
	}
	if err := s.st.SetGitHubComment(cs.UUID, n.ID, id, url); err != nil {
		log.Printf("workflow run %d: %v", rs.run.ID, err)
	}
	return stepOutcome{port: "out", output: text, note: "posted " + url}
}

// testStep runs a script on one of the workspace's machines, with the item's code, and waits.
func (s *Server) testStep(ctx context.Context, rs *runState, t workflow.TestRunSettings, script string) stepOutcome {
	m, err := s.st.Machine(t.Machine)
	if err != nil || m.WorkspaceID != rs.ws.ID || m.Revoked {
		return stepOutcome{err: errors.New("the machine is no longer one of this workspace's; choose another")}
	}
	if !m.Enabled {
		return stepOutcome{err: fmt.Errorf("%s is offline", m.Name)}
	}
	token, err := s.token(rs.ws)
	if err != nil {
		return stepOutcome{err: err}
	}
	commit, ref := "", ""
	if rs.it.pr != nil {
		commit, ref = rs.it.pr.HeadSHA, rs.it.pr.HeadSHA
	} else if commit, err = s.gh.CommitSHA(ctx, token, rs.ws.Name, "HEAD"); err != nil {
		return stepOutcome{err: fmt.Errorf("finding the repository's latest commit: %v", err)}
	}
	private := false
	if token != "" {
		if private, err = s.gh.Private(ctx, token, rs.ws.Name); err != nil {
			return stepOutcome{err: err}
		}
	}
	run, err := s.st.QueueRun(m, script, ref, commit, private, t.GPUs, time.Duration(t.Minutes)*time.Minute, rs.by)
	if err != nil {
		return stepOutcome{err: err}
	}
	deadline := time.After(time.Duration(t.Minutes)*time.Minute + 30*time.Minute) // waiting for the machine counts too
	for {
		select {
		case <-ctx.Done():
			return stepOutcome{err: errors.New("stopped waiting for the test run")}
		case <-deadline:
			s.st.RequestCancel(run.ID)
			return stepOutcome{err: fmt.Errorf("test run %d did not finish in time", run.ID)}
		case <-time.After(3 * time.Second):
		}
		state, _, summary, err := s.st.RunState(run.ID)
		if err != nil {
			return stepOutcome{err: err}
		}
		switch state {
		case "passed", "failed":
			out := summary
			if r, err := s.st.Run(run.ID); err == nil {
				tail := strings.TrimSpace(r.Output)
				if len(tail) > 4000 {
					tail = "…" + strings.ToValidUTF8(tail[len(tail)-4000:], "")
				}
				out += "\n\n" + tail
			}
			return stepOutcome{port: state, output: out, note: fmt.Sprintf("test run %d on %s", run.ID, m.Name)}
		case "error":
			return stepOutcome{err: fmt.Errorf("test run %d could not run: %s", run.ID, summary)}
		}
	}
}

// writeEntry writes a case entry under the node's name: it revises the node's open draft, or
// starts one, and finalizes it when asked.
func (s *Server) writeEntry(rs *runState, n workflow.Node, text string, finalize bool) error {
	c, err := s.st.CaseByNumber(rs.ws.ID, rs.it.number)
	if errors.Is(err, store.ErrNotFound) {
		c, err = s.caseForRun(rs)
	}
	if err != nil {
		return err
	}
	author := workflow.DisplayName(n)
	contribs, err := s.st.Contributions(c.UUID)
	if err != nil {
		return err
	}
	var id int64
	for _, k := range contribs {
		if k.Author == author && k.FinalizedAt.IsZero() {
			id = k.ID
		}
	}
	if id == 0 {
		if id, err = s.st.StartContribution(c.UUID, author); err != nil {
			return err
		}
	}
	if err := s.st.SetDraft(id, text); err != nil {
		return err
	}
	if finalize {
		_, err = s.st.FinalizeContribution(id)
	}
	return err
}

// agentStep runs an agent and waits for it to end.
func (s *Server) agentStep(ctx context.Context, rs *runState, n workflow.Node, a workflow.AgentSettings, values map[string]string) stepOutcome {
	id, err := s.startAgent(rs.ws, rs.run.Kind, n, a, values, rs.by, false)
	if err != nil {
		return stepOutcome{err: err}
	}
	for {
		select {
		case <-ctx.Done():
			return stepOutcome{agentRun: id, err: errors.New("the agent did not end in time")}
		case <-time.After(2 * time.Second):
		}
		run, err := s.st.AgentRun(id)
		if err != nil {
			return stepOutcome{agentRun: id, err: err}
		}
		if run.Status == "running" {
			continue
		}
		var tr agent.Transcript
		json.Unmarshal([]byte(run.Summary), &tr)
		switch run.Status {
		case agent.Met:
			return stepOutcome{port: "out", output: tr.Report, agentRun: id}
		case agent.Failed:
			return stepOutcome{agentRun: id, err: fmt.Errorf("the agent failed: %s", tr.Error)}
		default:
			out := tr.Report
			if out == "" {
				out = "(The agent ended without a report: " + tr.Error + ".)"
			}
			return stepOutcome{port: "out", output: out, agentRun: id, note: "the agent ended " + run.Status + ": " + tr.Error}
		}
	}
}

// runWorkflow starts a run from the event node in the URL, on the item in the form.
func (s *Server) runWorkflow(w http.ResponseWriter, r *http.Request) {
	ws, kind, _, g, i, ok := s.findNode(w, r)
	if !ok {
		return
	}
	n := g.Nodes[i]
	num, _ := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(r.PostForm.Get("item")), "#"))
	fail := func(msg string) {
		s.renderTest(w, r, http.StatusBadRequest, ws, kind, n, nil, map[string]any{"Error": msg, "Form": r.PostForm})
	}
	if num <= 0 {
		fail("Give the number of the pull request or issue.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	id, err := s.startWorkflow(ctx, ws, kind, n.ID, num, r.PostForm.Get("dry_run") == "on", me(r))
	if err != nil {
		fail("Not started: " + err.Error() + ".")
		return
	}
	http.Redirect(w, r, fmt.Sprintf("%s/workflow-runs/%d", ws.Path(), id), http.StatusSeeOther)
}

// stepView is one step on a run's page.
type stepView struct {
	Name, Label, Status, Port, Output, Error string
	AgentRun                                 int64
	Took                                     string
	Approval                                 *store.Approval // a Human Approval's request
}

// workflowRun is a run's page: live while it runs (the page refreshes itself), then its record.
func (s *Server) workflowRun(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	run, err := s.st.WorkflowRun(id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && run.WorkspaceID != ws.ID) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	var g workflow.Graph
	json.Unmarshal([]byte(run.Graph), &g)
	steps, err := s.st.WorkflowSteps(run.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	// In the order they ran, then the ones that did not.
	order := append([]workflow.Node{}, g.Nodes...)
	slices.SortStableFunc(order, func(a, b workflow.Node) int {
		sa, sb := steps[a.ID], steps[b.ID]
		rank := func(st store.WorkflowStep) int {
			if st.StartedAt.IsZero() || st.Status == "skipped" {
				return 1
			}
			return 0
		}
		if d := rank(sa) - rank(sb); d != 0 {
			return d
		}
		return sa.StartedAt.Compare(sb.StartedAt)
	})
	approvals, err := s.st.RunApprovals(run.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	var views []stepView
	for _, n := range order {
		st := steps[n.ID]
		t, _ := workflow.TypeOf(n.Type)
		v := stepView{Name: workflow.DisplayName(n), Label: t.Label, Status: st.Status, Port: st.Port, Output: st.Output, Error: st.Error, AgentRun: st.AgentRun}
		if !st.FinishedAt.IsZero() && !st.StartedAt.IsZero() && st.Status != "skipped" {
			v.Took = st.FinishedAt.Sub(st.StartedAt).Round(time.Second).String()
		}
		if v.Port == "out" {
			v.Port = ""
		}
		v.Approval = approvals[n.ID]
		views = append(views, v)
	}
	what := "pull request"
	if run.Kind == workflow.Issue {
		what = "issue"
	}
	s.render(w, r, http.StatusOK, "workflow_run.html", map[string]any{"Title": fmt.Sprintf("Run on #%d · %s", run.Item, ws.Name),
		"WS": ws, "Run": run, "Steps": views, "What": what})
}
