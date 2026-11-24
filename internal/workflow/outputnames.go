package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Every step that produces something keeps it under a tag, which is always its block's name in snake
// case (Value Proposition: value_proposition). Names are unique within a workflow, so tags are; renaming
// a block rewrites the references to its tag in the workflow's other blocks (Retag).

// HasOutput reports whether a node type produces an output that later steps can use. The two that
// post (GitHub Comment, Post to Discord) are endpoints: they keep nothing under a tag.
func HasOutput(typ string) bool {
	switch typ {
	case Agent, Decision, CaseEntry, Condition, TestRun, Approval:
		return true
	}
	return false
}

// maxTagLen is the longest an output tag may be (see tagNameRE).
const maxTagLen = 40

// Slug turns a block's name into an output tag: lower case, every run of characters that are not
// letters or digits one _, trimmed, at most 40 characters, and starting with a letter. "" when the
// name has no letter or digit at all.
func Slug(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else if b.Len() > 0 && !strings.HasSuffix(b.String(), "_") {
			b.WriteByte('_')
		}
	}
	s := strings.Trim(b.String(), "_")
	if s != "" && s[0] >= '0' && s[0] <= '9' {
		s = "step_" + s
	}
	if len(s) > maxTagLen {
		s = strings.TrimRight(s[:maxTagLen], "_")
	}
	return s
}

// withSuffix is base with _n after it, cut so the whole stays within the limit.
func withSuffix(base string, n int) string {
	suffix := fmt.Sprintf("_%d", n)
	if len(base)+len(suffix) > maxTagLen {
		base = strings.TrimRight(base[:maxTagLen-len(suffix)], "_")
	}
	return base + suffix
}

// freeTag is base, or base_2, base_3 … when the name is taken or is a built-in tag.
func freeTag(kind, base string, used map[string]bool) string {
	if base == "" {
		base = "step"
	}
	tag := base
	for n := 2; used[tag] || builtIn(tag, kind); n++ {
		tag = withSuffix(base, n)
	}
	return tag
}

// StoredOutputTo is the output tag saved in a node's settings, or "".
func StoredOutputTo(n Node) string {
	if !HasOutput(n.Type) || len(n.Settings) == 0 {
		return ""
	}
	var s struct {
		OutputTo string `json:"output_to"`
	}
	json.Unmarshal(n.Settings, &s)
	return s.OutputTo
}

// OutputNames is the output tag of each node that has one, by node id: its name's slug. Names are
// kept unique, but a graph from before may have two alike (or a name that is a built-in tag), and then
// the later gets _2, _3 … so that no two blocks share a tag.
func (g Graph) OutputNames(kind string) map[string]string {
	names := map[string]string{}
	used := map[string]bool{}
	for _, n := range g.Nodes {
		if HasOutput(n.Type) {
			tag := freeTag(kind, Slug(DisplayName(n)), used)
			names[n.ID], used[tag] = tag, true
		}
	}
	return names
}

// DefaultName is the name a new block of type typ gets: its type's label, or the label and 2, 3 … if a
// block of the workflow other than except (the new one) has a name with that tag already.
func (g Graph) DefaultName(typ, except string) string {
	t, _ := TypeOf(typ)
	taken := map[string]bool{}
	for _, n := range g.Nodes {
		if n.ID != except {
			taken[Slug(DisplayName(n))] = true
		}
	}
	name := t.Label
	for i := 2; taken[Slug(name)]; i++ {
		name = fmt.Sprintf("%s %d", t.Label, i)
	}
	return name
}

// CheckName confirms name may be the name of node n: it has a letter or digit, its tag (the name in
// snake case) is no other block's, and, for a block that has a tag, no built-in tag's.
func (g Graph) CheckName(kind string, n Node, name string) error {
	tag := Slug(name)
	if tag == "" {
		return errors.New("the name needs a letter or a digit: its tag is the name in lower case with underscores")
	}
	for _, o := range g.Nodes {
		if o.ID != n.ID && Slug(DisplayName(o)) == tag {
			return fmt.Errorf("another block is already named %s: names are unique in a workflow, since each gives its output the tag {%s}", DisplayName(o), tag)
		}
	}
	if HasOutput(n.Type) && builtIn(tag, kind) {
		return fmt.Errorf("{%s} is a built-in tag: choose another name, since the block's output is kept under it", tag)
	}
	return nil
}

// defaultSettings is what a node type's settings are before anything is saved.
func defaultSettings(typ, kind string) any {
	switch typ {
	case Agent:
		return DefaultAgent(kind)
	case Decision:
		return DefaultDecision(kind)
	case CaseEntry:
		return DefaultCaseEntry()
	case Condition:
		return DefaultCondition()
	case TestRun:
		return DefaultTestRun()
	case Approval:
		return DefaultApproval(kind)
	}
	return nil
}

// WithOutputTo returns the node with its output tag stored in its settings, which are the type's
// defaults when it has none yet. Nodes without an output come back as they are.
func WithOutputTo(n Node, kind, tag string) (Node, error) {
	if !HasOutput(n.Type) {
		return n, nil
	}
	fields := map[string]json.RawMessage{}
	raw := n.Settings
	if len(raw) == 0 {
		raw, _ = json.Marshal(defaultSettings(n.Type, kind))
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return n, err
	}
	fields["output_to"], _ = json.Marshal(tag)
	b, err := json.Marshal(fields)
	n.Settings = b
	return n, err
}

// Ancestors are the blocks with a path of links leading into the block id: the steps that run
// before it. The block itself is not among them.
func (g Graph) Ancestors(id string) map[string]bool {
	from := map[string][]string{} // a block, and the blocks linked into it
	for _, l := range g.Links {
		from[l.To] = append(from[l.To], l.From)
	}
	up := map[string]bool{}
	var walk func(string)
	walk = func(id string) {
		for _, f := range from[id] {
			if !up[f] {
				up[f] = true
				walk(f)
			}
		}
	}
	walk(id)
	delete(up, id) // a loop is refused, but never trust that here
	return up
}

// CheckOutputRef confirms that tag is the output tag of a step node id may use, upstream or of another
// event (a Condition's rule reads it), saying which step it comes from when that one does not run
// before the node.
func (g Graph) CheckOutputRef(kind, id, tag string) error {
	if _, ok := g.usable(kind, id)[tag]; ok {
		return nil
	}
	if from, ok := g.elsewhere(kind, id)[tag]; ok {
		return fmt.Errorf("{%s} comes from %s, which does not run before this block", tag, from)
	}
	return fmt.Errorf("{%s} is no step's output tag in this workflow", tag)
}

// chains says, for each block, which event blocks reach it by links (an event block is in its own
// chain): the set of nodes reachable from one event is that event's chain, and a block may be in several.
func (g Graph) chains() map[string]map[string]bool {
	next := map[string][]string{}
	for _, l := range g.Links {
		next[l.From] = append(next[l.From], l.To)
	}
	in := map[string]map[string]bool{}
	for _, e := range g.Nodes {
		if t, _ := TypeOf(e.Type); !t.Event {
			continue
		}
		seen := map[string]bool{e.ID: true}
		queue := []string{e.ID}
		for len(queue) > 0 {
			id := queue[0]
			queue = queue[1:]
			if in[id] == nil {
				in[id] = map[string]bool{}
			}
			in[id][e.ID] = true
			for _, to := range next[id] {
				if !seen[to] {
					seen[to] = true
					queue = append(queue, to)
				}
			}
		}
	}
	return in
}

// sharesChain reports whether two blocks are in the chain of one event block.
func sharesChain(chains map[string]map[string]bool, a, b string) bool {
	for e := range chains[a] {
		if chains[b][e] {
			return true
		}
	}
	return false
}
