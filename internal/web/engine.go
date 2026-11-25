package web

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/eidonix-ai/autobot/internal/agent"
	"github.com/eidonix-ai/autobot/internal/github"
	"github.com/eidonix-ai/autobot/internal/index"
	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// The engine runs a workflow on one pull request or issue, from one of its event nodes. A step
// runs once everything linked into it has finished, and only if at least one of those links fired:
// a link fires when its node finished on that output (a decision fires only its choice's links).
// A step none of whose links fired is skipped, and so on downstream, which is how a decision routes
// and how a fork rejoins. Independent steps run at the same time. Each step's output is the
// {input} of the steps it links to (several, when links merge, each under its node's name), and a
// step's "Output to" tag is kept for the case. A dry run posts nothing and keeps no output tags
// (they stay in memory), and it never reads or writes the item's case file: it has a case file of its
// own, opened where a real run would open the item's, that gets everything the real one would.

const maxDiff = 200_000 // characters of a pull request's diff given to {pr:diff}

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
	diff     string // the pull request's diff, when it was read from the local copy rather than from files' patches
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

// localItem is a pull request or issue from Autobot's own copies: the item from the local copy of
// pull requests and issues, a pull request's diff from the local copy of the code. It is nil when
// they do not have it in full; the caller then reads GitHub.
func (s *Server) localItem(ctx context.Context, ws *store.Workspace, kind string, number int) *item {
	if !index.Ready(s.st, ws.ID) {
		return nil
	}
	d, err := s.st.ItemIndex(ws.ID).Read(number)
	if err != nil || d.Slim || d.Remote || d.Kind != kind {
		return nil
	}
	path := "issues"
	if kind == workflow.PR {
		path = "pull"
	}
	it := &item{kind: kind, number: number, title: d.Title, state: d.State, body: d.Body, author: d.Author,
		url: fmt.Sprintf("https://github.com/%s/%s/%d", ws.Name, path, number), assoc: d.Association, labels: d.Labels}
	if kind != workflow.PR {
		return it
	}
	it.pr = &github.PullRequest{Number: number, URL: it.url, Title: d.Title, Body: d.Body, Author: d.Author, Draft: d.Draft, State: d.State,
		Association: d.Association, Labels: d.Labels, HeadSHA: d.HeadSHA, Base: d.Base, Additions: d.Additions, Deletions: d.Deletions}
	if s.repos == nil || !s.repos.Ready(ws.ID) || d.HeadSHA == "" {
		return nil
	}
	token, err := s.token(ws)
	if err != nil {
		return nil
	}
	if err := s.repos.Ensure(ctx, ws.ID, ws.Name, token, d.HeadSHA, number); err != nil {
		return nil
	}
	r := s.repos.Repo(ws.ID, token)
	base, err := r.Branch(ctx, d.Base)
	if err != nil {
		return nil
	}
	if it.diff, err = r.Diff(ctx, base, d.HeadSHA); err != nil || it.diff == "" {
		return nil
	}
	for _, f := range d.Files {
		it.files = append(it.files, github.ChangedFile{Path: f.Path, Status: f.Status, Additions: f.Additions, Deletions: f.Deletions})
	}
	return it
}

// values are an item's built-in tags.
func (it *item) values(ws *store.Workspace) map[string]string {
	v := map[string]string{"repo": ws.Name}
	n := strconv.Itoa(it.number)
	if it.kind == workflow.PR {
		diff := it.diff
		if diff == "" {
			diff = github.Diff(it.files)
		}
		if len(diff) > maxDiff {
			diff = strings.ToValidUTF8(diff[:maxDiff], "") + "\n[the diff goes on; read the rest with the pr_diff tool]"
		}
		var files []string
		for _, f := range it.files {
			files = append(files, fmt.Sprintf("%s (%s, +%d -%d)", f.Path, f.Status, f.Additions, f.Deletions))
		}
		v["pr"], v["pr:number"], v["pr:title"], v["pr:body"], v["pr:author"] = it.url, n, it.title, it.body, it.author
		v["pr:diff"], v["pr:files"] = diff, strings.Join(files, "\n")
	} else {
		v["issue"], v["issue:number"], v["issue:title"], v["issue:body"], v["issue:author"] = it.url, n, it.title, it.body, it.author
	}
	return v
}

// verdict says whether the event's conditions would have started the workflow on the item, and
// how each stood. A manual run goes ahead either way; this says what the poller would have done.
//
// optOut is the workspace's opt-out tag, and counts says whether a comment by someone can opt the item
// out (its author, or a maintainer); the tag in the description always does.
func verdict(n workflow.Node, it *item, caseExists bool, optOut string, counts func(author string) bool) (bool, []string) {
	ev, _ := workflow.EventSettingsOf(n)
	ev = ev.WithOptOut(optOut)
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
	case workflow.PRBacklog:
		if !ev.IncludeDrafts {
			check(!it.pr.Draft, "it is ready for review", "it is a draft")
		}
		check(!caseExists, "it has no case file yet", "it already has a case file")
	case workflow.IssueBacklog:
		check(!caseExists, "it has no case file yet", "it already has a case file")
	}
	stopped, by := workflow.Stops(it.body, ev.StopTag), "its description"
	for _, c := range it.comments {
		if !stopped && workflow.Stops(c.Body, ev.StopTag) && counts(c.Author) {
			stopped, by = true, "a comment by "+c.Author
		}
	}
	if ev.StopTag == "" {
		note("the workspace has no opt-out tag")
	} else {
		check(!stopped, "no opt-out tag “"+ev.StopTag+"”", "the opt-out tag “"+ev.StopTag+"” is in "+by)
	}
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
	discord *DiscordTrigger // set when run.Kind == workflow.Discord
	lfa     *LFATrigger     // set when workflow.IsLFA(run.Kind)
	builtIn map[string]string
	tags    map[string]string // output tags: the case's, then this run's
	by      *store.User
	// ctx is the run's: a person's stop cancels it (see stop.go), and every step's context derives from it.
	// It is nil for a state built by hand in a test.
	ctx       context.Context
	loopCount map[string]int // jumps taken per Loop node id during this run
}

// context is the run's context.
func (rs *runState) context() context.Context {
	if rs.ctx == nil {
		return context.Background()
	}
	return rs.ctx
}

// stopped says whether a person has stopped the run. From then on nothing it runs may write a tag, a
// case entry or a post.
func (rs *runState) stopped() bool { return rs.ctx != nil && agent.IsStopped(rs.ctx) }

// errStopped is how a step ends that a person's stop cut short.
func (rs *runState) errStopped() error {
	if rs.ctx != nil {
		if cause := context.Cause(rs.ctx); cause != nil {
			return cause
		}
	}
	return agent.ErrStopped
}

// startWorkflow starts a run of a kind's workflow on an item from an event node, and returns its id.
// A workspace that already has as many runs going as its settings allow gets a queued run instead.
func (s *Server) startWorkflow(ctx context.Context, ws *store.Workspace, kind, eventNode string, number int, dry bool, by *store.User) (int64, error) {
	_, g, err := s.loadWorkflow(ws, kind)
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
	rs.builtIn["repo:opt_out"] = s.optOut(ws)
	var caseFile *store.Case // whether the item has a case file is part of the event's verdict
	if c, err := s.st.CaseByNumber(ws.ID, number); err == nil {
		caseFile = c
		// A dry run reads nothing of the real case file: its tags start empty, and it has a case of its own.
		if !dry {
			if rs.tags, err = s.st.CaseValues(c.UUID); err != nil {
				return 0, err
			}
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return 0, err
	}
	if c, err := s.st.Charter(ws.ID, 0); err == nil {
		rs.builtIn["charter"] = c.Body
	}
	rs.builtIn["machines"] = s.machinesSummary(ws)

	token, _ := s.token(ws)
	pass, lines := verdict(*ev, it, caseFile != nil, s.optOut(ws), s.stopRule(ctx, ws, token, it))
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
	id, queued, err := s.admit(r, ids, by)
	if err != nil {
		return 0, err
	}
	if queued { // over the workspace's limit: it starts from its record when a slot is free (see queue.go)
		return id, nil
	}
	run, err := s.st.WorkflowRun(id)
	if err != nil {
		s.st.FinishWorkflowRun(id, "failed")
		s.releaseSlot(id)
		return 0, err
	}
	rs.run = run
	s.launch(rs)
	return id, nil
}

// startDiscordWorkflow starts a Discord workflow run from a Discord Mention event node.
func (s *Server) startDiscordWorkflow(ctx context.Context, ws *store.Workspace, eventNode string, trig DiscordTrigger, dry bool, by *store.User) (int64, error) {
	_, g, err := s.loadWorkflow(ws, workflow.Discord)
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
	it := &item{kind: workflow.Discord, author: trig.Author, title: trig.Channel, body: trig.Content, url: trig.URL}
	rs := &runState{ws: ws, g: g, it: it, discord: &trig, builtIn: trig.values(ws), tags: map[string]string{}, by: by}
	if c, err := s.st.Charter(ws.ID, 0); err == nil {
		rs.builtIn["charter"] = c.Body
	}
	rs.builtIn["machines"] = s.machinesSummary(ws)

	graph, _ := json.Marshal(g)
	tdata, _ := json.Marshal(trig)
	var ids []string
	for _, n := range g.Nodes {
		ids = append(ids, n.ID)
	}
	ch := rs.builtIn["discord:channel"]
	if ch == "" {
		ch = "(manual test)"
	}
	note := fmt.Sprintf("Discord mention in %s by %s\n\n%s", ch, trig.Author, trig.Content)
	if trig.URL != "" {
		note += "\n\n" + trig.URL
	}
	r := store.WorkflowRun{
		WorkspaceID: ws.ID,
		Kind:        workflow.Discord,
		EventNode:   eventNode,
		Item:        0,
		TriggerData: string(tdata),
		DryRun:      dry,
		Graph:       string(graph),
		Note:        note,
	}
	id, queued, err := s.admit(r, ids, by)
	if err != nil {
		return 0, err
	}
	if queued {
		return id, nil
	}
	run, err := s.st.WorkflowRun(id)
	if err != nil {
		s.st.FinishWorkflowRun(id, "failed")
		s.releaseSlot(id)
		return 0, err
	}
	rs.run = run
	s.launch(rs)
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
	if rs.builtIn == nil {
		rs.builtIn = map[string]string{}
	}
	if rs.builtIn["loop:attempt"] == "" {
		rs.builtIn["loop:attempt"] = "1"
	}
	nodes := map[string]workflow.Node{}
	incoming := map[string][]workflow.Link{}
	for _, n := range rs.g.Nodes {
		nodes[n.ID] = rs.g.Named(n)
		if t, _ := workflow.TypeOf(n.Type); t.Event && steps[n.ID].Status == "pending" {
			st := steps[n.ID]
			st.Status, st.StartedAt, st.FinishedAt = "skipped", now, now
			if n.ID == rs.run.EventNode {
				st.Status, st.Port = "done", "out"
				if rs.run.Kind == workflow.Discord && rs.discord != nil {
					ch := rs.builtIn["discord:channel"]
					st.Output = fmt.Sprintf("%s in %s by %s: %s", workflow.DisplayName(n), ch, rs.discord.Author, oneLineOf(rs.discord.Content, 120))
				} else if workflow.IsLFA(rs.run.Kind) && rs.lfa != nil {
					st.Output = fmt.Sprintf("%s · %s: %s", rs.lfa.PageTitle, rs.lfa.Title, oneLineOf(rs.lfa.Prompt, 120))
				} else {
					st.Output = fmt.Sprintf("%s #%d: %s", workflow.DisplayName(n), rs.it.number, rs.it.title)
				}
			}
			save(st)
		}
	}
	for _, l := range rs.g.Links {
		incoming[l.To] = append(incoming[l.To], l)
	}
	// inputsFrom are the sources of a step: the steps linked into it, each with whether its link fired
	// (it finished, on the output the link leaves from) or is missing (failed, skipped, or went another
	// way). A Node (Junction) only connects, so the step after it has the Node's own sources as its
	// own, as if their links went to it directly; a step reached twice counts once, and as fired if
	// any of its links fired.
	var inputsFrom func(id string) []source
	inputsFrom = func(id string) []source {
		var out []source
		add := func(s source) {
			for i, o := range out {
				if o.node.ID == s.node.ID {
					out[i].missing = o.missing && s.missing
					return
				}
			}
			out = append(out, s)
		}
		for _, l := range incoming[id] {
			src := steps[l.From]
			if nodes[l.From].Type == workflow.Junction {
				for _, s := range inputsFrom(l.From) {
					add(s)
				}
				continue
			}
			add(source{node: nodes[l.From], step: src, missing: src.Status != "done" || src.Port != l.Out})
		}
		return out
	}
	type finished struct {
		id string
		o  stepOutcome
	}
	results := make(chan finished)
	running := 0
	// A run counts against its workspace's PR/Issue limit (see queue.go) except while everything
	// going on is steps waiting for a person's answer or machine-bound test steps (which have their
	// own 1-per-machine queue so hardware tests never wait behind or occupy PR/Issue review slots).
	holding, slotFree := s.holdsSlot(rs.run.ID), 0
	isSlotFree := func(n workflow.Node) bool {
		switch n.Type {
		case workflow.Approval, workflow.TestRun, workflow.Post:
			return true
		case workflow.Agent:
			if a, err := workflow.AgentSettingsOf(n, rs.run.Kind); err == nil && len(a.Machines) > 0 {
				return true
			}
		}
		return false
	}
	// A resumed run picks up its steps that were waiting for a person.
	for _, n := range rs.g.Nodes {
		if steps[n.ID].Status == "waiting" && n.Type == workflow.Approval {
			running++
			slotFree++
			go func(n workflow.Node) {
				o := stepOutcome{err: errors.New("the step crashed")}
				defer func() { results <- finished{n.ID, o} }()
				defer survive(fmt.Sprintf("workflow run %d step %s", rs.run.ID, n.ID), nil)
				ctx, cancel := context.WithTimeout(rs.context(), 15*24*time.Hour)
				defer cancel()
				o = s.awaitApproval(ctx, rs, n)
			}(n)
		}
	}
	for {
		for progressed := true; progressed; {
			progressed = false
			for _, n := range rs.g.Nodes {
				if rs.stopped() { // a stopped run starts nothing more
					break
				}
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
				if len(fired) > 0 && !holding && !isSlotFree(n) {
					if !s.acquireSlot(rs.context(), rs.ws.ID, rs.run.Kind, rs.run.ID) { // stopped while it waited
						progressed = false
						break
					}
					holding = true
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
				if isSlotFree(n) {
					slotFree++
				}
				sources := inputsFrom(n.ID) // copied here: the steps change while this one runs
				// The tags every step that has finished has kept, so far: this step sees them all, whether
				// they came down its own branch or another, and the run goes on adding to the map.
				tags := maps.Clone(rs.tags)
				builtIn := maps.Clone(rs.builtIn)
				go func(n workflow.Node, sources []source) {
					o := stepOutcome{err: errors.New("the step crashed")}
					defer func() { results <- finished{n.ID, o} }()
					defer survive(fmt.Sprintf("workflow run %d step %s", rs.run.ID, n.ID), nil)
					o = s.runStep(rs, n, sources, tags, builtIn)
				}(nodes[n.ID], sources) // the block as the canvas names it
			}
		}
		if holding && running > 0 && running == slotFree {
			s.releaseSlot(rs.run.ID)
			holding = false
		}
		if running == 0 {
			break
		}
		f := <-results
		running--
		if isSlotFree(nodes[f.id]) {
			slotFree--
		}
		st := steps[f.id]
		if rs.stopped() { // whatever it came to, it ended after the stop: it writes nothing
			st.Status, st.FinishedAt, st.Port, st.Output, st.Error = "stopped", time.Now(), "", "", ""
			if f.o.agentRun != 0 {
				st.AgentRun = f.o.agentRun
			}
			save(st)
			continue
		}
		st.FinishedAt, st.Port, st.Output, st.AgentRun = time.Now(), f.o.port, f.o.output, f.o.agentRun
		st.Status = "done"
		if f.o.err != nil {
			st.Status, st.Error, st.Port = "failed", f.o.err.Error(), ""
		} else if f.o.note != "" {
			st.Error = f.o.note
		}
		// Loop jump: if the Loop node still has retries left, reset every step reachable from its
		// GOTO target back to pending (along with the Loop itself) and pass feedback into the next pass.
		if st.Status == "done" && f.o.jumpTo != "" {
			if rs.loopCount == nil {
				rs.loopCount = map[string]int{}
			}
			if rs.loopCount[f.id] < f.o.jumpMax {
				rs.loopCount[f.id]++
				rs.builtIn["loop:attempt"] = strconv.Itoa(rs.loopCount[f.id] + 1)
				rs.builtIn["loop:feedback"] = f.o.jumpFeedback
				for id := range rs.g.ReachableFrom(f.o.jumpTo) {
					cur := steps[id]
					if id != f.id && cur.Status == "running" {
						continue
					}
					save(store.WorkflowStep{NodeID: id, Status: "pending"})
				}
				continue
			}
			st.Port = "limit"
			st.Error = fmt.Sprintf("max retries reached (%d/%d)", rs.loopCount[f.id], f.o.jumpMax)
		}
		save(st)
		if st.Status == "done" {
			if f.o.patch != "" {
				rs.builtIn["run:patch"] = f.o.patch
				if f.o.baseCommit != "" {
					rs.builtIn["run:base_commit"] = f.o.baseCommit
				}
			}
			s.keepTag(rs, nodes[f.id], st.Output, f.o.tags)
			s.writeResult(rs, nodes[f.id], f.o)
		}
	}
	if rs.stopped() { // the person's stop marks the run, its steps and its approvals
		by := ""
		var stop *agent.StopError
		if errors.As(context.Cause(rs.ctx), &stop) {
			by = stop.By
		}
		s.markStopped(rs.run.ID, by)
		return
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

// source is a finished step linked into another, as the step after it sees it: one whose link fired,
// or a missing one (it failed, was skipped, or went another way).
type source struct {
	node    workflow.Node
	step    store.WorkflowStep
	missing bool
}

// why says why a missing source gave nothing.
func (src source) why() string {
	switch src.step.Status {
	case "failed":
		if msg := oneLineOf(src.step.Error, 120); msg != "" {
			return "failed: " + msg
		}
		return "failed"
	case "done":
		return "took its “" + src.step.Port + "” output"
	}
	return src.step.Status // skipped
}

// outputParts are the suffixes of the values a step hands on, as keepTag stores an output tag's:
// "" is the whole, then the summary, the report and the notes.
var outputParts = []string{"", ".summary", ".report", ".scratchpad"}

// inputValues are the values of {input} and its parts for a step, from the links that fired into
// it: one step's as they are, several each under the name of the node it came from. Then come the
// steps linked in that failed or were stopped, each under its name with why ("## Correctness (failed:
// the model could not be reached)"), so that a step that reads a join knows a source is missing. A step that is not an agent hands on its output, as both its summary and its report, and
// has no notes.
func (s *Server) inputValues(rs *runState, sources []source) map[string]string {
	var fired, missing []source
	for _, src := range sources {
		switch {
		case !src.missing:
			fired = append(fired, src)
		case src.step.Status == "failed" || src.step.Status == "stopped":
			// Only a step that broke is named: a branch not taken (skipped, or another output
			// of a decision) is how routing works, not a gap.
			missing = append(missing, src)
		}
	}
	all := make([]map[string]string, len(fired))
	for i, src := range fired {
		all[i] = s.stepParts(rs, src)
	}
	out := map[string]string{}
	for _, suffix := range outputParts {
		var parts []string
		switch {
		case len(fired) == 1:
			if text := all[0][suffix]; text != "" || len(missing) == 0 {
				parts = append(parts, text)
			}
		default:
			for i, src := range fired {
				parts = append(parts, "## "+workflow.DisplayName(src.node)+"\n\n"+all[i][suffix])
			}
		}
		for _, src := range missing {
			parts = append(parts, "## "+workflow.DisplayName(src.node)+" ("+src.why()+")")
		}
		out["input"+suffix] = strings.Join(parts, "\n\n")
	}
	return out
}

// firedOnly are the sources whose links fired.
func firedOnly(sources []source) []source {
	return slices.DeleteFunc(slices.Clone(sources), func(src source) bool { return src.missing })
}

// stepParts are what a finished step hands on, by suffix. An agent's are rebuilt from its run, so
// that they survive a restart of Autobot between the steps.
func (s *Server) stepParts(rs *runState, src source) map[string]string {
	if src.node.Type == workflow.Agent && src.step.AgentRun != 0 {
		a, err := s.agentSettings(src.node, rs.run.Kind)
		run, rerr := s.st.AgentRun(src.step.AgentRun)
		if err == nil && rerr == nil {
			_, parts := s.agentParts(a.EffectiveClosing(rs.run.Kind) == workflow.ClosingReport, run)
			return parts
		}
	}
	out := src.step.Output
	return map[string]string{"": out, ".summary": out, ".report": out, ".scratchpad": ""}
}

// keepTag stores a finished step's output under its output tag (its own, or its name's: see
// OutputNames): for the rest of the run, and for the case unless the run is dry. An Agent's output
// has parts (parts, by suffix: "" is the tag itself, ".summary" and so on), each kept under the
// tag's name and the suffix; any other step has just its output. Every one replaces the value the
// last write left.
func (s *Server) keepTag(rs *runState, n workflow.Node, output string, parts map[string]string) {
	name := rs.g.OutputNames(rs.run.Kind)[n.ID]
	if name == "" || rs.stopped() {
		return
	}
	if parts == nil {
		parts = map[string]string{"": output}
	}
	for suffix, v := range parts {
		rs.tags[name+suffix] = v
	}
	if rs.run.DryRun || rs.run.Kind == workflow.Discord { // dry runs and Discord mentions keep tags in memory
		return
	}
	c, err := s.runCase(rs)
	if errors.Is(err, store.ErrNotFound) {
		if c, err = s.caseForRun(rs); err != nil {
			log.Printf("workflow run %d: %v", rs.run.ID, err)
			return
		}
	}
	for suffix, v := range parts {
		if err == nil {
			err = s.st.SetCaseValue(c.UUID, name+suffix, v, n.ID)
		}
	}
	if err != nil {
		log.Printf("workflow run %d: keeping {%s}: %v", rs.run.ID, name, err)
	}
}

// writeResult adds a finished Agent's or Decision's result to the case file, as a finalized entry under
// the step's name, unless its "Write to the case file" box is off. A run adds an entry every time: the
// case file is a record, nothing is replaced. A dry run writes to its own case file, and a step that
// took no part writes nothing (an agent with no conversation to continue). An Agent's entry is a heading with the
// step's name, its summary (its report when it has none), and links to the full report and the notes as
// published pages; a Decision's is one line: the choice, how sure the model was, and what the choice means.
func (s *Server) writeResult(rs *runState, n workflow.Node, o stepOutcome) {
	if rs.stopped() || rs.run.Kind == workflow.Discord {
		return
	}
	kind, name := rs.run.Kind, workflow.DisplayName(n)
	var text string
	switch n.Type {
	case workflow.Agent:
		a, err := workflow.AgentSettingsOf(n, kind)
		tag := rs.g.OutputNames(kind)[n.ID]
		if err != nil || !a.WritesToCase() || o.agentRun == 0 || tag == "" {
			return
		}
		summary := strings.TrimSpace(o.tags[".summary"])
		if summary == "" {
			summary = strings.TrimSpace(o.tags[".report"])
		}
		links := "[[" + tag + ".report|Full report]]"
		if o.tags[".scratchpad"] != "" {
			links += " · [[" + tag + ".scratchpad|Notes]]"
		}
		values := maps.Clone(rs.tags)
		s.fillLinks(s.runLinks(rs), []string{links}, values)
		text = "## " + name + "\n\n" + summary + "\n\n" + workflow.Render(links, values)
	case workflow.Decision:
		d, err := workflow.DecisionSettingsOf(n, kind)
		if err != nil || !d.WritesToCase() || o.port == "" {
			return
		}
		text = name + ": " + o.port + " (" + o.tags[".confidence"] + ")"
		if reason := o.tags[".reason"]; reason != "" {
			text += ": " + reason
		}
	default:
		return
	}
	if err := s.writeEntry(rs, n, text, true); err != nil {
		log.Printf("workflow run %d: writing %s to the case file: %v", rs.run.ID, name, err)
	}
}

// runCase is the case file a run writes to, if it has opened one: the item's for a run, the run's
// own for a dry run, which never looks at the item's.
func (s *Server) runCase(rs *runState) (*store.Case, error) {
	if rs.run.DryRun {
		return s.st.DryCase(rs.run.ID)
	}
	if workflow.IsLFA(rs.run.Kind) {
		return s.st.CaseByLFARun(rs.ws.ID, rs.run.ID)
	}
	return s.st.CaseByNumber(rs.ws.ID, rs.it.number)
}

// caseForRun opens the case file a run writes to: the item's, or for a dry run a case of its own.
func (s *Server) caseForRun(rs *runState) (*store.Case, error) {
	et, err := time.LoadLocation("America/New_York")
	if err != nil {
		et = time.UTC
	}
	opened := fmt.Sprintf("Opened by a workflow run, %s.", time.Now().In(et).Format("Jan 2, 2006 3:04 PM MST"))
	if rs.run.DryRun {
		c, _, err := s.st.OpenDryCase(rs.ws, rs.run.ID, rs.it.number, rs.it.kind, rs.it.title, store.DryHeadline(rs.run.ID)+" "+opened)
		return c, err
	}
	if workflow.IsLFA(rs.run.Kind) {
		head := opened
		if rs.lfa != nil && rs.lfa.PageTitle != "" {
			head = fmt.Sprintf("Large feature workflow (%s): %s. %s", rs.lfa.PageTitle, rs.lfa.Title, opened)
		}
		c, _, err := s.st.OpenLFACase(rs.ws, rs.run.ID, rs.it.title, head)
		return c, err
	}
	c, _, err := s.st.OpenCase(rs.ws, rs.it.number, rs.it.kind, rs.it.title, opened)
	return c, err
}

// stepOutcome is how a step ended: the output it took and what it produced. note is something to
// show beside a step that still counts as done, such as an agent that finished blocked.
type stepOutcome struct {
	port, output, note string
	agentRun           int64
	err                error
	// tags are the values an Agent's or a Decision's output tag takes, by suffix (see agentParts); nil
	// for a step whose output is the tag's value.
	tags map[string]string
	// patch & baseCommit are captured from a sandboxed Agent step (when files were modified).
	patch, baseCommit string
	// jumpTo, jumpMax & jumpFeedback are returned by a Loop step.
	jumpTo       string
	jumpMax      int
	jumpFeedback string
}

// runStep runs one step with the tags kept so far (those of the case and of the steps of this run
// that have finished) and the output of the steps linked into it.
func (s *Server) runStep(rs *runState, n workflow.Node, sources []source, tags, builtIn map[string]string) stepOutcome {
	if n.Type == workflow.Junction { // a Node does no work: its output is what came into it
		return stepOutcome{port: "out", output: s.inputValues(rs, firedOnly(sources))["input"]}
	}
	values := map[string]string{}
	for k, v := range tags {
		values[k] = v
	}
	for k, v := range builtIn {
		values[k] = v
	}
	maps.Copy(values, s.inputValues(rs, sources))
	kind := rs.run.Kind
	if kind != workflow.Discord {
		if c, err := s.runCase(rs); err == nil {
			maps.Copy(values, s.caseTags(kind, c)) // {case}, {case:link}, {pr:case_file}: a dry run's are its own case's
		}
	}
	// A [[tag]] in a template becomes a link to a page that keeps the tag's value as it is now.
	s.fillLinks(s.runLinks(rs), s.nodeTemplates(n, kind), values)
	limit := 4 * time.Hour
	switch n.Type {
	case workflow.Approval:
		limit = 15 * 24 * time.Hour // a person may take up to two weeks
	case workflow.Agent:
		if a, err := workflow.AgentSettingsOf(n, kind); err == nil {
			limit = agentStepTimeout(a.MaxMinutes)
		}
	}
	ctx, cancel := context.WithTimeout(rs.context(), limit)
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
		// The tag is the choice; its parts are how sure the model was and why the choice means what it does.
		parts := map[string]string{"": out, ".confidence": "", ".reason": ""}
		for _, c := range ans.Choices {
			if c.Name == ans.Choice {
				parts[".confidence"] = fmt.Sprintf("%.0f%%", 100*c.P)
			}
		}
		for _, c := range d.Choices {
			if c.Name == ans.Choice {
				parts[".reason"] = c.Description
			}
		}
		return stepOutcome{port: ans.Choice, output: out, tags: parts}
	case workflow.CaseEntry:
		c, err := workflow.CaseEntrySettingsOf(n)
		if err == nil {
			err = c.Check()
		}
		if err != nil {
			return stepOutcome{err: err}
		}
		text := strings.TrimSpace(workflow.Render(c.Entry, values))
		if err := s.writeEntry(rs, n, text, c.Finalize); err != nil {
			return stepOutcome{err: err}
		}
		if rs.run.DryRun {
			return stepOutcome{port: "out", output: text, note: "dry run: written to the dry run's case file"}
		}
		return stepOutcome{port: "out", output: text}
	case workflow.Agent:
		a, err := s.agentSettings(n, kind)
		if err == nil {
			err = a.Check(kind)
		}
		if err != nil {
			return stepOutcome{err: err}
		}
		return s.agentStep(ctx, rs, n, a, values)
	case workflow.Post:
		p, err := workflow.SettingsOf(n, workflow.DefaultPostFor(kind))
		if err == nil {
			err = p.Check()
		}
		if err != nil {
			return stepOutcome{err: err}
		}
		return s.postStep(ctx, rs, n, p, strings.TrimSpace(workflow.Render(p.Message, values)))
	case workflow.Condition:
		c, err := workflow.SettingsOf(n, workflow.DefaultCondition())
		if err == nil {
			err = c.Check(kind)
		}
		if err != nil {
			return stepOutcome{err: err}
		}
		f := workflow.ItemFacts{Author: rs.it.author, Association: rs.it.assoc, Labels: rs.it.labels, Title: rs.it.title, Body: rs.it.body, Tags: tags}
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
	case workflow.Approval:
		a, err := workflow.SettingsOf(n, workflow.DefaultApproval(kind))
		if err == nil {
			err = a.CheckRun()
		}
		if err != nil {
			return stepOutcome{err: err}
		}
		return s.approvalStep(ctx, rs, n, a, strings.TrimSpace(workflow.Render(a.Request, values)), strings.TrimSpace(workflow.Render(a.Question, values)))
	case workflow.Loop:
		l, err := workflow.SettingsOf(n, workflow.DefaultLoop())
		if err == nil {
			err = l.Check()
		}
		if err != nil {
			return stepOutcome{err: err}
		}
		if !rs.g.Ancestors(n.ID)[l.Target] {
			return stepOutcome{err: errors.New("the Loop target is not an upstream block of this Loop")}
		}
		fb := strings.TrimSpace(workflow.Render(l.Feedback, values))
		return stepOutcome{port: "limit", output: fb, jumpTo: l.Target, jumpMax: l.MaxTries, jumpFeedback: fb}
	case workflow.OpenPR:
		op, err := workflow.SettingsOf(n, workflow.DefaultOpenPR(kind))
		if err == nil {
			err = op.Check()
		}
		if err != nil {
			return stepOutcome{err: err}
		}
		return s.openPRStep(ctx, rs, n, op, values)
	}
	return stepOutcome{err: fmt.Errorf("a %s block cannot run yet", n.Type)}
}

// postStep says the text in each destination the block has ticked, GitHub then Discord: the one text,
// rendered once, so a [[link]] in it is the same in both. A destination that cannot be reached for a
// reason that is not a failure (a private repository stays out of Discord) is skipped and noted; one
// that fails does not stop the other from being tried, and fails the step, which reports both.
func (s *Server) postStep(ctx context.Context, rs *runState, n workflow.Node, p workflow.PostSettings, text string) stepOutcome {
	if text == "" {
		return stepOutcome{err: errors.New("the message came out empty")}
	}
	if rs.stopped() {
		return stepOutcome{err: rs.errStopped()}
	}
	if rs.run.Kind == workflow.Discord {
		return s.discordReplyStep(ctx, rs, p.Trial, text)
	}
	if p.Once && !rs.run.DryRun && !(p.GitHub && p.Trial && !p.Discord) {
		already, err := s.st.PostAlreadySent(rs.ws.ID, rs.run.Kind, rs.run.Item, n.ID, rs.run.ID)
		if err != nil {
			return stepOutcome{err: err}
		}
		if already {
			return stepOutcome{port: "out", output: text, note: "already posted on this item; skipped"}
		}
	}
	var reports []string
	failed := false
	report := func(where string, o stepOutcome) {
		switch {
		case errors.Is(o.err, errPrivateRepo):
			reports = append(reports, where+": skipped, "+o.err.Error())
		case o.err != nil:
			failed = true
			reports = append(reports, where+": "+o.err.Error())
		default:
			reports = append(reports, where+": "+o.note)
		}
	}
	if p.GitHub {
		report("GitHub", s.commentStep(ctx, rs, n, p.Trial, text))
	}
	if p.Discord {
		if rs.run.DryRun {
			report("Discord", stepOutcome{note: "dry run: not posted"})
		} else {
			report("Discord", s.discordStep(ctx, rs, p.Summary, text))
		}
	}
	if failed {
		return stepOutcome{err: errors.New(strings.Join(reports, "; "))}
	}
	return stepOutcome{port: "out", output: text, note: strings.Join(reports, "; ")}
}

// commentStep posts a comment on the item, or, in a trial or a dry run, writes what it would post
// into the case file (a dry run's own). Every run adds a comment; none is ever edited.
func (s *Server) commentStep(ctx context.Context, rs *runState, n workflow.Node, trial bool, text string) stepOutcome {
	if rs.stopped() {
		return stepOutcome{err: rs.errStopped()}
	}
	if trial || rs.run.DryRun {
		if err := s.writeEntry(rs, n, "**Would post on GitHub:**\n\n"+text, true); err != nil {
			return stepOutcome{err: err}
		}
		if rs.run.DryRun {
			return stepOutcome{port: "out", output: text, note: "dry run: written to the dry run's case file, not posted"}
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
	_, url, err := s.gh.Comment(ctx, token, rs.ws.Name, rs.it.number, text)
	if err != nil {
		return stepOutcome{err: err}
	}
	return stepOutcome{port: "out", output: text, note: "posted " + url}
}

// openPRStep pushes the patch captured from an earlier sandboxed Agent step to a branch and opens a
// GitHub pull request (ready or draft). In trial mode or dry runs it writes a preview to the case file.
func (s *Server) openPRStep(ctx context.Context, rs *runState, n workflow.Node, op workflow.OpenPRSettings, values map[string]string) stepOutcome {
	if rs.stopped() {
		return stepOutcome{err: rs.errStopped()}
	}
	patch := values["run:patch"]
	baseCommit := strings.TrimSpace(values["run:base_commit"])
	if strings.TrimSpace(patch) == "" {
		return stepOutcome{err: errors.New("no code patch was produced by an earlier sandboxed Agent step")}
	}
	title := strings.TrimSpace(workflow.Render(op.Title, values))
	body := strings.TrimSpace(workflow.Render(op.Body, values))
	branch := workflow.SanitizeBranch(workflow.Render(op.Branch, values))
	base := strings.TrimSpace(workflow.Render(op.Base, values))
	kindWord := "Ready PR"
	if op.Draft {
		kindWord = "Draft PR"
	}
	if op.Trial || rs.run.DryRun {
		var files []string
		for _, line := range strings.Split(patch, "\n") {
			if strings.HasPrefix(line, "diff --git ") {
				if parts := strings.Fields(line); len(parts) >= 4 {
					files = append(files, "- `"+strings.TrimPrefix(parts[3], "b/")+"`")
				}
			}
		}
		preview := fmt.Sprintf("**Would open %s**\n\n- **Title:** %s\n- **Branch:** `%s` → `%s`\n- **Changed files (%d):**\n%s\n\n---\n\n%s",
			kindWord, title, branch, cmp.Or(base, "(default branch)"), len(files), strings.Join(files, "\n"), body)
		if err := s.writeEntry(rs, n, preview, true); err != nil {
			return stepOutcome{err: err}
		}
		if rs.run.DryRun {
			return stepOutcome{port: "out", output: title, note: "dry run: written to the dry run's case file, not pushed"}
		}
		return stepOutcome{port: "out", output: title, note: "trial run: written to the case file, no branch pushed or PR opened"}
	}
	if s.repos == nil {
		return stepOutcome{err: errors.New("git mirror support is not available on this server")}
	}
	token, err := s.token(rs.ws)
	if err != nil {
		return stepOutcome{err: err}
	}
	if token == "" {
		return stepOutcome{err: errors.New("opening a pull request needs the workspace's GitHub identity: set it in the workspace's Settings")}
	}
	if base == "" {
		if base, err = s.gh.DefaultBranch(ctx, token, rs.ws.Name); err != nil {
			return stepOutcome{err: fmt.Errorf("finding the default branch: %w", err)}
		}
	}
	if err := s.repos.PushPatch(ctx, rs.ws.ID, rs.ws.Name, token, baseCommit, branch, patch, title); err != nil {
		return stepOutcome{err: err}
	}
	num, prURL, err := s.gh.CreatePullRequest(ctx, token, rs.ws.Name, title, body, branch, base, op.Draft)
	if err != nil {
		return stepOutcome{err: err}
	}
	_ = s.writeEntry(rs, n, fmt.Sprintf("Opened %s [#%d](%s) from branch `%s` → `%s`.", kindWord, num, prURL, branch, base), true)
	return stepOutcome{port: "out", output: prURL, note: fmt.Sprintf("opened %s #%d: %s", kindWord, num, prURL)}
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
	var run *store.TestRun
	for {
		run, err = s.st.QueueRun(m, script, ref, commit, private, t.GPUs, time.Duration(t.Minutes)*time.Minute, rs.by)
		var ie *store.InputError
		if !errors.As(err, &ie) || !strings.Contains(ie.Msg, "already running a test") {
			break
		}
		select {
		case <-ctx.Done():
			if rs.stopped() {
				return stepOutcome{err: rs.errStopped()}
			}
			return stepOutcome{err: fmt.Errorf("stopped waiting for %s to become free", m.Name)}
		case <-time.After(machinePoll):
		}
	}
	if err != nil {
		return stepOutcome{err: err}
	}
	deadline := time.After(time.Duration(t.Minutes)*time.Minute + 30*time.Minute) // waiting for the machine counts too
	for {
		select {
		case <-ctx.Done():
			s.st.RequestCancel(run.ID) // the test run on the machine stops too
			if rs.stopped() {
				return stepOutcome{err: rs.errStopped()}
			}
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
	if rs.stopped() {
		return rs.errStopped()
	}
	c, err := s.runCase(rs)
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

// agentStepTimeout is the workflow step's timeout for an Agent block: at least 4 hours (covering
// waiting for an earlier run on the same thread, sandbox setup, the run itself, overtime grace,
// save turn and closing writer), and scaled up for long MaxMinutes blocks (up to 240m).
func agentStepTimeout(maxMinutes int) time.Duration {
	if maxMinutes <= 0 {
		return 4 * time.Hour
	}
	return max(4*time.Hour, time.Duration(maxMinutes)*2*time.Minute+45*time.Minute)
}

// agentStep runs an agent and waits for it to end. A block that continues another's conversation
// first waits for any run in progress in that conversation to end (for as long as its own time
// limit), and takes no part, with a note, when the item has no conversation and it is set to skip.
func (s *Server) agentStep(ctx context.Context, rs *runState, n workflow.Node, a workflow.AgentSettings, values map[string]string) stepOutcome {
	a, err := withSourceModel(rs.g, rs.run.Kind, a)
	if err != nil {
		return stepOutcome{err: err}
	}
	limit := time.Duration(a.MaxMinutes) * time.Minute
	waited := time.Now()
	var id int64
	for {
		id, err = s.startAgent(rs.context(), rs.ws, rs.run.Kind, n, a, values, rs.by, false, rs.it.number)
		if errors.Is(err, errMachineBusy) {
			select {
			case <-ctx.Done():
				if rs.stopped() {
					return stepOutcome{err: rs.errStopped()}
				}
				return stepOutcome{err: errors.New("stopped waiting for a test machine to become free")}
			case <-time.After(machinePoll):
				continue
			}
		}
		if !errors.Is(err, store.ErrThreadBusy) {
			break
		}
		if time.Since(waited) > limit {
			return stepOutcome{err: fmt.Errorf("waited %d minutes for the run in progress in the conversation to end", a.MaxMinutes)}
		}
		select {
		case <-ctx.Done():
			if rs.stopped() {
				return stepOutcome{err: rs.errStopped()}
			}
			return stepOutcome{err: errors.New("stopped waiting for the conversation's run in progress")}
		case <-time.After(convWait):
		}
	}
	if errors.Is(err, errNoConversation) {
		return stepOutcome{port: "out", note: errNoConversation.Error()}
	}
	if err != nil {
		return stepOutcome{err: err}
	}
	// The page of a running step links to its agent run, and a Stop there stops the whole workflow run.
	if err := s.st.SetStepAgentRun(rs.run.ID, n.ID, id); err != nil {
		log.Printf("workflow run %d: %v", rs.run.ID, err)
	}
	for {
		select {
		case <-ctx.Done():
		case <-time.After(agentPoll):
		}
		if rs.stopped() { // the agent was stopped with the run; give it a moment to close its session
			s.running.wait(liveKey{agent: true, id: id})
			return stepOutcome{agentRun: id, err: rs.errStopped()}
		}
		if ctx.Err() != nil {
			return stepOutcome{agentRun: id, err: errors.New("the agent did not end in time")}
		}
		run, err := s.st.AgentRun(id)
		if err != nil {
			return stepOutcome{agentRun: id, err: err}
		}
		if run.Status == "running" {
			continue
		}
		if !hasReport(run) && run.Status != agent.Stopped {
			// No report: the step fails, so nothing after it decides or posts from nothing (Derek,
			// 2026-11-24: a Decision chose "yes" from "(The agent ended without a report…)").
			var tr agent.Transcript
			json.Unmarshal([]byte(run.Summary), &tr)
			return stepOutcome{agentRun: id, err: fmt.Errorf("the agent ended without a report: %s", cmp.Or(tr.Error, run.Status))}
		}
		tr, parts := s.agentParts(a.EffectiveClosing(rs.run.Kind) == workflow.ClosingReport, run)
		if run.Status == agent.Failed {
			return stepOutcome{agentRun: id, err: fmt.Errorf("the agent failed: %s", tr.Error)}
		}
		// The step's own output is the report; the output tag, and {input} for the next step, have
		// more: see agentParts.
		o := stepOutcome{port: "out", output: parts[".report"], agentRun: id, tags: parts, patch: tr.Patch, baseCommit: tr.BaseCommit}
		if run.Status != agent.Met {
			o.note = "the agent ended " + run.Status + ": " + tr.Error
		}
		return o
	}
}

// hasReport says whether an agent run ended with a report.
func hasReport(run *store.AgentRun) bool {
	var tr agent.Transcript
	json.Unmarshal([]byte(run.Summary), &tr)
	return strings.TrimSpace(tr.Report) != ""
}

// agentParts are what an Agent step hands on from its run, by suffix:
//   - ".summary" and ".report" are the run's summary and its full report, and ".scratchpad" its
//     thread's notes in full (the main agent's: sub-agents' notes are kept apart);
//   - "" is the whole, under headings: Summary, Report, and Notes when there are any. A block whose
//     report is not written from the notes (closing none) has one report, which is also its
//     summary, so its whole is the report with the Notes after it.
//
// It also returns the run's transcript.
func (s *Server) agentParts(closing bool, run *store.AgentRun) (agent.Transcript, map[string]string) {
	var tr agent.Transcript
	json.Unmarshal([]byte(run.Summary), &tr)
	if tr.Report == "" {
		tr.Report = "(The agent ended without a report: " + tr.Error + ".)"
	}
	tr.EnsureSummary() // a run from before summaries has only its report
	var notes string
	if run.Thread != 0 {
		if list, err := s.st.Pad(run.Thread).List(); err == nil {
			notes = store.ScratchpadText(list)
		} else {
			log.Printf("agent run %d: reading its notes: %v", run.ID, err)
		}
	}
	summary, whole := tr.Summary, "## Summary\n\n"+tr.Summary+"\n\n## Report\n\n"+tr.Report
	if !closing {
		summary, whole = tr.Report, tr.Report
	}
	if notes != "" {
		whole += "\n\n## Notes\n\n" + notes
	}
	return tr, map[string]string{"": whole, ".summary": summary, ".report": tr.Report, ".scratchpad": notes}
}

// runWorkflow starts a run from the event node in the URL, on the item (or Discord message) in the form.
func (s *Server) runWorkflow(w http.ResponseWriter, r *http.Request) {
	ws, kind, _, g, i, ok := s.findNode(w, r)
	if !ok {
		return
	}
	n := g.Named(g.Nodes[i])
	fail := func(msg string) {
		s.renderTest(w, r, http.StatusBadRequest, ws, kind, n, nil, map[string]any{"Error": msg, "Form": r.PostForm})
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	if workflow.IsLFA(kind) {
		title := strings.TrimSpace(r.PostForm.Get("title"))
		prompt := strings.TrimSpace(r.PostForm.Get("prompt"))
		if title == "" {
			fail("Give this feature run a title.")
			return
		}
		if prompt == "" {
			fail("Write a prompt or direction for the Kick Off.")
			return
		}
		pageID := workflow.LFAPageID(kind)
		pageTitle := "Large feature"
		if p, err := s.st.WorkflowPage(ws.ID, pageID); err == nil {
			pageTitle = p.Title
		}
		trig := LFATrigger{
			PageID:    pageID,
			PageTitle: pageTitle,
			Title:     title,
			Prompt:    prompt,
			Base:      strings.TrimSpace(r.PostForm.Get("base")),
			Author:    me(r).Name(),
		}
		id, err := s.startLFAWorkflow(ctx, ws, kind, n.ID, trig, r.PostForm.Get("dry_run") == "on", me(r))
		if err != nil {
			fail("Not started: " + err.Error() + ".")
			return
		}
		http.Redirect(w, r, fmt.Sprintf("%s/workflow-runs/%d", ws.Path(), id), http.StatusSeeOther)
		return
	}
	if kind == workflow.Discord {
		msg := strings.TrimSpace(r.PostForm.Get("message"))
		if msg == "" {
			fail("Write a message to test.")
			return
		}
		author := strings.TrimSpace(r.PostForm.Get("author"))
		if author == "" {
			author = me(r).Name()
		}
		ch := strings.TrimPrefix(strings.TrimSpace(r.PostForm.Get("channel")), "#")
		if ch == "" {
			ch = "general"
		}
		trig := DiscordTrigger{
			Channel: ch,
			Author:  author,
			Content: msg,
			Context: strings.TrimSpace(r.PostForm.Get("context")),
		}
		id, err := s.startDiscordWorkflow(ctx, ws, n.ID, trig, r.PostForm.Get("dry_run") == "on", me(r))
		if err != nil {
			fail("Not started: " + err.Error() + ".")
			return
		}
		http.Redirect(w, r, fmt.Sprintf("%s/workflow-runs/%d", ws.Path(), id), http.StatusSeeOther)
		return
	}
	num, _ := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(r.PostForm.Get("item")), "#"))
	if num <= 0 {
		fail("Give the number of the pull request or issue.")
		return
	}
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
		v := stepView{Name: g.NameOf(n), Label: t.Label, Status: st.Status, Port: st.Port, Output: st.Output, Error: st.Error, AgentRun: st.AgentRun}
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
	title := fmt.Sprintf("Run on #%d · %s", run.Item, ws.Name)
	heading := fmt.Sprintf("Run on %s #%d", what, run.Item)
	switch run.Kind {
	case workflow.Issue:
		what = "issue"
		heading = fmt.Sprintf("Run on %s #%d", what, run.Item)
	case workflow.Discord:
		what = "Discord"
		trig := parseDiscordTrigger(run.TriggerData)
		ch := trig.Channel
		if ch != "" && !strings.HasPrefix(ch, "#") {
			ch = "#" + ch
		}
		if ch == "" {
			ch = "Discord"
		}
		title = fmt.Sprintf("Discord run (%s) · %s", ch, ws.Name)
		heading = fmt.Sprintf("Discord mention in %s", ch)
	default:
		if workflow.IsLFA(run.Kind) {
			trig := parseLFATrigger(run.TriggerData)
			what = cmp.Or(trig.PageTitle, "Large feature")
			title = fmt.Sprintf("%s: %s · %s", what, trig.Title, ws.Name)
			heading = fmt.Sprintf("%s · %s", what, trig.Title)
		}
	}
	data := map[string]any{"Title": title, "Heading": heading, "WS": ws, "Run": run, "Steps": views, "What": what}
	if !run.DryRun && workflow.IsLFA(run.Kind) {
		if c, err := s.st.CaseByLFARun(ws.ID, run.ID); err == nil {
			data["LFACasePath"] = casePath(ws, c)
		}
	}
	if run.Status == "queued" {
		data["QueueText"] = "Queued"
		if place, ok := s.queuePlaces(ws.ID)[run.ID]; ok {
			data["QueueText"] = fmt.Sprintf("Queued (%s in line)", ordinal(place))
		}
	}
	if run.DryRun { // the dry run's own case file, once it has opened one
		if c, err := s.st.DryCase(run.ID); err == nil {
			data["DryCase"] = c
		}
	}
	s.render(w, r, http.StatusOK, "workflow_run.html", data)
}
