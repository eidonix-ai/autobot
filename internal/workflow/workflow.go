// Package workflow is a workspace's workflows: one for pull requests and one for issues, each a
// set of nodes placed on a canvas and joined by links. An event node says when a workflow runs on an
// item; the nodes it links to (agents, decisions, and later testing and posting) run after it. A link
// runs from a named output of one node to a named input of another, so a node can fan out (one output
// to several nodes, or a decision with one output per choice) and links can collapse into one input.
// The one shape refused for now is a loop, because nothing runs loops yet; how a node with several
// incoming links waits for them is the engine's to decide, and nothing here fixes it.
package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// The workflow kinds, one tab each.
const (
	PR    = "pr"
	Issue = "issue"
)

// Kinds lists the workflow kinds in tab order.
var Kinds = []string{PR, Issue}

// KindLabel names a kind for a tab.
func KindLabel(kind string) string {
	if kind == PR {
		return "Pull requests"
	}
	return "Issues"
}

// NodeType is a kind of node that can be placed on a workflow's canvas.
type NodeType struct {
	Type    string   `json:"type"`
	Kind    string   `json:"kind"` // the workflow it belongs to; "" for steps, which go in any
	Label   string   `json:"label"`
	Event   bool     `json:"event"`   // starts a chain: no input
	Outputs []string `json:"outputs"` // a new node's outputs, filled by TypesFor
}

// Types are every node type, in menu order.
var Types = []NodeType{
	{Type: "pr_ready", Kind: PR, Label: "PR Ready", Event: true},
	{Type: "pr_draft", Kind: PR, Label: "PR Draft", Event: true},
	{Type: "pr_activity", Kind: PR, Label: "PR Activity", Event: true},
	{Type: "issue_opened", Kind: Issue, Label: "Issue Opened", Event: true},
	{Type: Agent, Label: "Agent"},
	{Type: Decision, Label: "Decision"},
	{Type: CaseEntry, Label: "Case Entry"},
	{Type: GitHubComment, Label: "GitHub Comment"},
	{Type: Condition, Label: "Condition"},
	{Type: TestRun, Label: "Test Run"},
}

// TypesFor lists the node types a kind's canvas offers.
func TypesFor(kind string) []NodeType {
	var out []NodeType
	for _, t := range Types {
		if t.Kind == kind || t.Kind == "" {
			t.Outputs = Outputs(Node{Type: t.Type})
			out = append(out, t)
		}
	}
	return out
}

// TypeOf looks a node type up.
func TypeOf(typ string) (NodeType, bool) {
	for _, t := range Types {
		if t.Type == typ {
			return t, true
		}
	}
	return NodeType{}, false
}

// Graph is a workflow: its nodes and the links between them.
type Graph struct {
	Nodes []Node `json:"nodes"`
	Links []Link `json:"links"`
}

// Node is one node on the canvas.
type Node struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Name     string          `json:"name,omitempty"` // shown above the node; its type's label when empty
	X        float64         `json:"x"`
	Y        float64         `json:"y"`
	Settings json.RawMessage `json:"settings,omitempty"`
}

// Link joins an output of one node to an input of another.
type Link struct {
	From string `json:"from"`
	Out  string `json:"out"` // the output's name: "out", or a decision's choice
	To   string `json:"to"`
	In   string `json:"in"` // the input's name: "in"
}

// Outputs are the named outputs of a node: a decision has one per choice, every other node "out".
// (A decision's default choices are the same in either kind of workflow.)
func Outputs(n Node) []string {
	if n.Type == Decision {
		if s, err := DecisionSettingsOf(n, PR); err == nil && len(s.Choices) > 0 {
			out := make([]string, len(s.Choices))
			for i, c := range s.Choices {
				out[i] = c.Name
			}
			return out
		}
	}
	switch n.Type {
	case Condition:
		return []string{"yes", "no"}
	case TestRun:
		return []string{"passed", "failed"}
	}
	return []string{"out"}
}

// Inputs are the named inputs of a node; an event node has none.
func Inputs(n Node) []string {
	if t, _ := TypeOf(n.Type); t.Event {
		return nil
	}
	return []string{"in"}
}

// DisplayName is what the canvas shows above a node.
func DisplayName(n Node) string {
	if n.Name != "" {
		return n.Name
	}
	t, _ := TypeOf(n.Type)
	return t.Label
}

func has(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

var idRE = regexp.MustCompile(`^[a-z0-9]{1,24}$`)

// MaxNodes caps a workflow's size.
const MaxNodes = 100

// Check confirms a graph is a valid workflow of kind: known node types for the kind, unique ids,
// event settings that check out and step settings that can be read (a step placed but not yet set
// up is allowed; it cannot run), and links without loops between ports the nodes have.
func (g Graph) Check(kind string) error {
	if kind != PR && kind != Issue {
		return fmt.Errorf("no workflow kind %q", kind)
	}
	if len(g.Nodes) > MaxNodes {
		return fmt.Errorf("a workflow is limited to %d nodes", MaxNodes)
	}
	byID := map[string]Node{}
	for _, n := range g.Nodes {
		t, ok := TypeOf(n.Type)
		if !ok || (t.Kind != kind && t.Kind != "") {
			return fmt.Errorf("a %s workflow has no node type %q", KindLabel(kind), n.Type)
		}
		if !idRE.MatchString(n.ID) {
			return fmt.Errorf("node id %q is not valid", n.ID)
		}
		if _, dup := byID[n.ID]; dup {
			return fmt.Errorf("two nodes share the id %s", n.ID)
		}
		if len([]rune(n.Name)) > 60 || strings.ContainsAny(n.Name, "\n\r\t") {
			return fmt.Errorf("node names are one line of up to 60 characters")
		}
		if n.X < -1e6 || n.X > 1e6 || n.Y < -1e6 || n.Y > 1e6 {
			return fmt.Errorf("node %s is placed out of range", n.ID)
		}
		if t.Event {
			s, err := EventSettingsOf(n)
			if err != nil {
				return err
			}
			if err := s.Check(n.Type); err != nil {
				return fmt.Errorf("%s: %w", t.Label, err)
			}
		}
		var err error
		switch n.Type {
		case Agent:
			_, err = AgentSettingsOf(n, kind)
		case Decision:
			_, err = DecisionSettingsOf(n, kind)
		case CaseEntry:
			_, err = CaseEntrySettingsOf(n)
		case GitHubComment:
			_, err = SettingsOf(n, DefaultGitHubComment())
		case Condition:
			_, err = SettingsOf(n, DefaultCondition())
		case TestRun:
			_, err = SettingsOf(n, DefaultTestRun())
		}
		if err != nil {
			return err
		}
		byID[n.ID] = n
	}
	next := map[string][]string{}
	seen := map[Link]bool{}
	for _, l := range g.Links {
		from, okf := byID[l.From]
		to, okt := byID[l.To]
		if !okf || !okt || l.From == l.To {
			return errors.New("a link joins nodes that are not on the canvas")
		}
		if t, _ := TypeOf(to.Type); t.Event {
			return errors.New("an event node starts a workflow; nothing can link into it")
		}
		if !has(Outputs(from), l.Out) || !has(Inputs(to), l.In) {
			return fmt.Errorf("a link uses an output or input that %s or %s does not have", label(from), label(to))
		}
		if seen[l] {
			return errors.New("two links join the same output and input")
		}
		seen[l] = true
		next[l.From] = append(next[l.From], l.To)
	}
	// Any shape but a loop: walk from every node, and fail on reaching a node already on the path.
	const (
		unvisited = iota
		onPath
		done
	)
	mark := map[string]int{}
	var visit func(string) error
	visit = func(id string) error {
		switch mark[id] {
		case onPath:
			return errors.New("links cannot loop back to an earlier step (not yet supported)")
		case done:
			return nil
		}
		mark[id] = onPath
		for _, n := range next[id] {
			if err := visit(n); err != nil {
				return err
			}
		}
		mark[id] = done
		return nil
	}
	for id := range byID {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func label(n Node) string {
	if t, ok := TypeOf(n.Type); ok {
		return t.Label
	}
	return n.Type
}

// EventSettings are an event node's settings: when the workflow runs on an item.
type EventSettings struct {
	Mode          string `json:"mode"`           // "summoned": only when summoned; "always": whenever the event happens
	SummonPhrase  string `json:"summon_phrase"`  // a comment containing it summons Autobot
	ReviewRequest bool   `json:"review_request"` // PRs: a review request to the workspace's GitHub user also summons
	StopTag       string `json:"stop_tag"`       // the item or any comment containing it stops the run
	TrustedOnly   bool   `json:"trusted_only"`   // honor summons and the stop tag only from the author and collaborators

	// PR Activity only: what activity on an open pull request runs it, and how long to wait for a
	// burst of activity to settle first.
	Activity     []string `json:"activity,omitempty"`
	QuietMinutes int      `json:"quiet_minutes,omitempty"`
	CaseExists   bool     `json:"case_exists,omitempty"` // only for a pull request that already has a case file

	// Pull requests only: which pull requests it handles.
	Branches     string `json:"branches,omitempty"` // target branches, comma separated; "" is the default branch
	Authors      string `json:"authors,omitempty"`  // "all", "skip_bots" or "only_bots"
	SkipMaintain bool   `json:"skip_maintainers,omitempty"`
	MaxLines     int    `json:"max_lines,omitempty"`  // changed lines above which OverLimit applies; 0 is no limit
	OverLimit    string `json:"over_limit,omitempty"` // "skip" or "summoned"
	WaitCI       string `json:"wait_ci,omitempty"`    // "no", "finished" or "passed"
}

// Activities are the kinds of activity on an open pull request that can run PR Activity, in the
// order its settings list them.
var Activities = []struct{ Key, Label string }{
	{"comments", "New comments"},
	{"pushes", "New pushes (commits)"},
	{"reviews", "Reviews submitted"},
	{"checks", "CI finished"},
	{"edits", "Title or description edited"},
	{"labels", "Labels changed"},
}

// DefaultEvent is a new event node's settings.
func DefaultEvent(typ string) EventSettings {
	s := EventSettings{Mode: "always", SummonPhrase: "@autobot review", StopTag: "Autobot: ignore", TrustedOnly: true}
	t, _ := TypeOf(typ)
	if t.Kind == Issue {
		s.SummonPhrase = "@autobot"
		return s
	}
	s.ReviewRequest, s.Authors, s.OverLimit, s.WaitCI = true, "skip_bots", "summoned", "finished"
	if typ == "pr_activity" {
		s.Activity, s.QuietMinutes = []string{"comments", "pushes"}, 15
	}
	return s
}

// EventSettingsOf reads an event node's settings, or the defaults when it has none yet.
func EventSettingsOf(n Node) (EventSettings, error) {
	t, _ := TypeOf(n.Type)
	if len(n.Settings) == 0 {
		return DefaultEvent(n.Type), nil
	}
	var s EventSettings
	if err := json.Unmarshal(n.Settings, &s); err != nil {
		return s, fmt.Errorf("%s: unreadable settings", t.Label)
	}
	return s, nil
}

var branchRE = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)

// Check confirms the settings are complete and sensible for an event node of type typ.
func (s EventSettings) Check(typ string) error {
	t, _ := TypeOf(typ)
	kind := t.Kind
	switch {
	case s.Mode != "summoned" && s.Mode != "always":
		return errors.New("choose when it runs: only if summoned, or always")
	case strings.TrimSpace(s.StopTag) == "" || len(s.StopTag) > 100:
		return errors.New("the stop tag must be 1 to 100 characters")
	case strings.TrimSpace(s.SummonPhrase) == "" || len(s.SummonPhrase) > 100:
		return errors.New("the summon phrase must be 1 to 100 characters")
	case Stops(s.SummonPhrase, s.StopTag):
		return errors.New("the stop tag matches the summon phrase itself, so every summons would stop the run")
	}
	if kind == Issue {
		return nil
	}
	if typ == "pr_activity" {
		if len(s.Activity) == 0 {
			return errors.New("choose at least one kind of activity")
		}
		for _, a := range s.Activity {
			if !knownActivity(a) {
				return fmt.Errorf("no kind of activity %q", a)
			}
		}
		if s.QuietMinutes < 0 || s.QuietMinutes > 24*60 {
			return errors.New("the quiet period is 0 to 1440 minutes")
		}
	}
	switch {
	case s.Authors != "all" && s.Authors != "skip_bots" && s.Authors != "only_bots":
		return errors.New("choose which authors' pull requests it handles")
	case s.MaxLines < 0 || s.MaxLines > 10_000_000:
		return errors.New("the size limit must be a number of changed lines, or 0 for none")
	case s.OverLimit != "skip" && s.OverLimit != "summoned":
		return errors.New("choose what happens to pull requests over the size limit")
	case s.WaitCI != "no" && s.WaitCI != "finished" && s.WaitCI != "passed":
		return errors.New("choose whether to wait for CI")
	}
	for _, b := range strings.Split(s.Branches, ",") {
		if b = strings.TrimSpace(b); b != "" && !branchRE.MatchString(b) {
			return fmt.Errorf("%q is not a branch name", b)
		}
	}
	return nil
}

func knownActivity(key string) bool {
	for _, a := range Activities {
		if a.Key == key {
			return true
		}
	}
	return false
}

// HasActivity reports whether the settings include a kind of activity, for the settings form.
func (s EventSettings) HasActivity(key string) bool {
	for _, a := range s.Activity {
		if a == key {
			return true
		}
	}
	return false
}

// Summary is the one line an event node shows on the canvas.
func (s EventSettings) Summary() string {
	parts := []string{"Always runs"}
	if s.Mode == "summoned" {
		parts[0] = "Only if summoned"
	}
	if s.CaseExists {
		parts = append(parts, "with a case file")
	}
	parts = append(parts, "stops on “"+s.StopTag+"”")
	return strings.Join(parts, " · ")
}

// EventFromForm reads an event node's settings from its settings form; all returns every value of
// a field (the activity checklist).
func EventFromForm(typ string, get func(string) string, all func(string) []string) EventSettings {
	t, _ := TypeOf(typ)
	kind := t.Kind
	s := EventSettings{
		Mode:         get("mode"),
		SummonPhrase: strings.TrimSpace(get("summon_phrase")),
		StopTag:      strings.TrimSpace(get("stop_tag")),
		TrustedOnly:  get("trusted_only") == "on",
	}
	if kind == Issue {
		return s
	}
	s.ReviewRequest = get("review_request") == "on"
	if typ == "pr_activity" {
		s.Activity = all("activity")
		s.QuietMinutes, _ = strconv.Atoi(get("quiet_minutes"))
		s.CaseExists = get("case_exists") == "on"
	}
	s.Branches = strings.TrimSpace(get("branches"))
	s.Authors = get("authors")
	s.SkipMaintain = get("skip_maintainers") == "on"
	s.MaxLines, _ = strconv.Atoi(get("max_lines"))
	s.OverLimit = get("over_limit")
	s.WaitCI = get("wait_ci")
	return s
}
