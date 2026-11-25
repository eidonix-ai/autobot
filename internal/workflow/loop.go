package workflow

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Loop is an explicit bounded jump back to an earlier step on the same canvas. Because the jump
// target is stored in settings rather than as a cyclic link, the canvas stays a readable DAG while
// still supporting bounded dev/review retry cycles. When max_tries jumps have already happened,
// Loop finishes on its "limit" output port instead of jumping again.
const Loop = "loop"

const (
	MinLoopTries = 1
	MaxLoopTries = 10
)

// LoopSettings are a Loop node's settings.
type LoopSettings struct {
	Target   string `json:"target"`             // node id of an upstream step to jump back to
	MaxTries int    `json:"max_tries"`          // maximum number of jumps back (1..10); total attempts = 1 + MaxTries
	Feedback string `json:"feedback,omitempty"` // template rendered into {loop:feedback} when jumping back
}

// DefaultLoop is a new Loop node's settings: up to 2 retries (3 total attempts), forwarding {input.report}.
func DefaultLoop() LoopSettings {
	return LoopSettings{MaxTries: 2, Feedback: "{input.report}"}
}

// Check confirms a Loop node's settings are complete.
func (s LoopSettings) Check() error {
	switch {
	case strings.TrimSpace(s.Target) == "":
		return errors.New("choose an upstream block to jump back to")
	case s.MaxTries < MinLoopTries || s.MaxTries > MaxLoopTries:
		return fmt.Errorf("max retries must be between %d and %d", MinLoopTries, MaxLoopTries)
	case len(s.Feedback) > maxTemplate:
		return fmt.Errorf("the feedback template is limited to %d characters", maxTemplate)
	}
	return nil
}

// LoopFromForm reads a Loop node's settings from its form.
func LoopFromForm(get func(string) string) LoopSettings {
	n, _ := strconv.Atoi(strings.TrimSpace(get("max_tries")))
	return LoopSettings{
		Target:   strings.TrimSpace(get("target")),
		MaxTries: n,
		Feedback: strings.TrimSpace(get("feedback")),
	}
}

// LoopTargets returns the valid upstream target blocks for a Loop node: every non-event ancestor
// in the graph, in canvas order.
func (g Graph) LoopTargets(loopID string) []Node {
	up := g.Ancestors(loopID)
	var out []Node
	for _, n := range g.Nodes {
		if !up[n.ID] {
			continue
		}
		if t, _ := TypeOf(n.Type); t.Event {
			continue
		}
		out = append(out, n)
	}
	return out
}

// ReachableFrom returns the set of node IDs reachable forward from startID (including startID).
func (g Graph) ReachableFrom(startID string) map[string]bool {
	next := map[string][]string{}
	for _, l := range g.Links {
		next[l.From] = append(next[l.From], l.To)
	}
	seen := map[string]bool{startID: true}
	queue := []string{startID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, to := range next[id] {
			if !seen[to] {
				seen[to] = true
				queue = append(queue, to)
			}
		}
	}
	return seen
}
