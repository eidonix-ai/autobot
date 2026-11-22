// Package workflow is a workspace's workflows: one for pull requests and one for issues, each a
// set of nodes placed on a canvas and joined in chains. A chain starts at an event node, which says
// when a workflow runs on an item; later nodes (review, test, decide, post) follow it in a straight
// line. The canvas is free in layout and strict in meaning: each node has at most one link in and one
// link out, and an event node has none in, so running a chain is doing its steps in order.
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
	Type  string `json:"type"`
	Kind  string `json:"kind"` // the workflow it belongs to
	Label string `json:"label"`
	Event bool   `json:"event"` // starts a chain: no input
}

// Types are every node type, in menu order.
var Types = []NodeType{
	{Type: "pr_ready", Kind: PR, Label: "PR Ready", Event: true},
	{Type: "pr_draft", Kind: PR, Label: "PR Draft", Event: true},
	{Type: "pr_activity", Kind: PR, Label: "PR Activity", Event: true},
	{Type: "issue_opened", Kind: Issue, Label: "Issue Opened", Event: true},
}

// TypesFor lists the node types a kind's canvas offers.
func TypesFor(kind string) []NodeType {
	var out []NodeType
	for _, t := range Types {
		if t.Kind == kind {
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
	X        float64         `json:"x"`
	Y        float64         `json:"y"`
	Settings json.RawMessage `json:"settings,omitempty"`
}

// Link joins one node's output to the next node's input.
type Link struct {
	From string `json:"from"`
	To   string `json:"to"`
}

var idRE = regexp.MustCompile(`^[a-z0-9]{1,24}$`)

// MaxNodes caps a workflow's size.
const MaxNodes = 100

// Check confirms a graph is a valid workflow of kind: known node types for the kind, unique ids,
// settings that check out, and links that form straight chains starting at event nodes.
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
		if !ok || t.Kind != kind {
			return fmt.Errorf("a %s workflow has no node type %q", KindLabel(kind), n.Type)
		}
		if !idRE.MatchString(n.ID) {
			return fmt.Errorf("node id %q is not valid", n.ID)
		}
		if _, dup := byID[n.ID]; dup {
			return fmt.Errorf("two nodes share the id %s", n.ID)
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
		byID[n.ID] = n
	}
	out, in := map[string]string{}, map[string]string{}
	for _, l := range g.Links {
		from, okf := byID[l.From]
		_, okt := byID[l.To]
		if !okf || !okt || l.From == l.To {
			return errors.New("a link joins nodes that are not on the canvas")
		}
		if t, _ := TypeOf(byID[l.To].Type); t.Event {
			return errors.New("an event node starts a chain; nothing can link into it")
		}
		if _, ok := out[l.From]; ok {
			return fmt.Errorf("%s already links to a next step; a chain does not branch", label(from))
		}
		if _, ok := in[l.To]; ok {
			return fmt.Errorf("%s already has a step before it; chains do not merge", label(byID[l.To]))
		}
		out[l.From], in[l.To] = l.To, l.From
	}
	// Straight lines with one link in and out cannot branch or merge; a loop is the one shape left.
	for id := range byID {
		seen := map[string]bool{}
		for at := id; ; {
			next, ok := out[at]
			if !ok {
				break
			}
			if seen[next] {
				return errors.New("a chain cannot loop back on itself")
			}
			seen[next], at = true, next
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
	}
	s.Branches = strings.TrimSpace(get("branches"))
	s.Authors = get("authors")
	s.SkipMaintain = get("skip_maintainers") == "on"
	s.MaxLines, _ = strconv.Atoi(get("max_lines"))
	s.OverLimit = get("over_limit")
	s.WaitCI = get("wait_ci")
	return s
}
