package workflow

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Every step that produces something keeps it under a tag: its "Output to". A new block gets the tag
// from its name (Value Proposition: value_proposition), which it keeps when the block is renamed,
// since templates may use it. A block saved before it was set behaves as if its tag were its name's.

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

// OutputNames is the output tag of each node that has one, by node id: the saved one, or for a block
// without (saved before the tag was set) its name's slug, made unique in node order.
func (g Graph) OutputNames(kind string) map[string]string {
	names := map[string]string{}
	used := map[string]bool{}
	for _, n := range g.Nodes {
		if tag := StoredOutputTo(n); tag != "" {
			names[n.ID], used[tag] = tag, true
		}
	}
	for _, n := range g.Nodes {
		if _, ok := names[n.ID]; HasOutput(n.Type) && !ok {
			tag := freeTag(kind, Slug(DisplayName(n)), used)
			names[n.ID], used[tag] = tag, true
		}
	}
	return names
}

// FreeOutputTag is a tag for the node except to take from base (a name's slug): base itself if no
// other block has it and it is no built-in tag, else base_2, base_3 …
func (g Graph) FreeOutputTag(kind, base, except string) string {
	used := map[string]bool{}
	for id, tag := range g.OutputNames(kind) {
		if id != except {
			used[tag] = true
		}
	}
	return freeTag(kind, base, used)
}

// CheckOutputTag confirms tag may be the output tag of node n: a lower-case word that is no built-in
// tag, and no other block's.
func (g Graph) CheckOutputTag(kind string, n Node, tag string) error {
	if err := checkOutputTo(tag, kind); err != nil {
		return err
	}
	for id, other := range g.OutputNames(kind) {
		if id == n.ID || other != tag {
			continue
		}
		for _, o := range g.Nodes {
			if o.ID == id {
				return fmt.Errorf("{%s} is already the output of %s: choose another name for this output", tag, DisplayName(o))
			}
		}
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

// CheckOutputRef confirms that tag is the output tag of a step upstream of node id (a Condition's
// rule reads it), saying which step it comes from when that one does not run before the node.
func (g Graph) CheckOutputRef(kind, id, tag string) error {
	if _, ok := g.OutputTags(kind, id)[tag]; ok {
		return nil
	}
	if from, ok := g.elsewhere(kind, id)[tag]; ok {
		return fmt.Errorf("{%s} comes from %s, which does not run before this block", tag, from)
	}
	return fmt.Errorf("{%s} is no step's output tag in this workflow", tag)
}
