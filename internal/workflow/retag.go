package workflow

import (
	"encoding/json"
	"fmt"
	"strings"
)

// A block's tag is its name in snake case, so renaming a block changes it. The references to the old
// tag in the workflow's other blocks are rewritten to the new one in the same save.

// TagRename says that what was kept under the tag Old is now kept under New. With Keep, a value already
// kept under New stays: the older one does not replace it (the migration of reference blocks).
type TagRename struct {
	Old, New string
	Keep     bool
}

// RetagText rewrites the references to the tag old in a template to the tag new: {old}, {old.part},
// [[old]], [[old.part]], [[old|Label]] and [[old.part|Label]]. {{old}} and [[[[ are text, not references.
// The legacy {input} and {previous_output} are not touched.
func RetagText(text, old, new string) string {
	return tagRE.ReplaceAllStringFunc(text, func(m string) string {
		var open string
		switch {
		case m == "{{" || m == "}}" || m == "[[[[":
			return m
		case strings.HasPrefix(m, "{"):
			open = "{"
		default:
			open = "[["
		}
		name := m[len(open):]
		if end := strings.IndexAny(name, "}|]"); end >= 0 {
			name = name[:end]
		}
		switch {
		case name == old:
			return open + new + m[len(open)+len(old):]
		case strings.HasPrefix(name, old+"."):
			return open + new + m[len(open)+len(old):]
		}
		return m
	})
}

// Retag rewrites the references to the tag old in a block's templates (and, for a Condition, its tag
// field) to new, and reports whether anything changed. A block that was never saved has no template of
// its own to change.
func Retag(n Node, kind, old, new string) (Node, bool) {
	if len(n.Settings) == 0 {
		return n, false
	}
	changed := false
	fix := func(fields ...*string) {
		for _, f := range fields {
			if r := RetagText(*f, old, new); r != *f {
				*f, changed = r, true
			}
		}
	}
	var settings any
	switch n.Type {
	case Agent:
		s, err := AgentSettingsOf(n, kind)
		if err != nil {
			return n, false
		}
		fix(&s.Instruction, &s.Additional, &s.Goal, &s.SummaryFormat, &s.FollowUp)
		settings = s
	case Decision:
		s, err := DecisionSettingsOf(n, kind)
		if err != nil {
			return n, false
		}
		fix(&s.Context, &s.Question)
		settings = s
	case CaseEntry:
		s, err := CaseEntrySettingsOf(n)
		if err != nil {
			return n, false
		}
		fix(&s.Entry)
		settings = s
	case Post:
		s, err := SettingsOf(n, DefaultPost())
		if err != nil {
			return n, false
		}
		fix(&s.Message)
		settings = s
	case Approval:
		s, err := SettingsOf(n, DefaultApproval(kind))
		if err != nil {
			return n, false
		}
		fix(&s.Question, &s.Request)
		settings = s
	case TestRun:
		s, err := SettingsOf(n, DefaultTestRun())
		if err != nil {
			return n, false
		}
		fix(&s.Script)
		settings = s
	case Condition:
		s, err := SettingsOf(n, DefaultCondition())
		if err != nil {
			return n, false
		}
		if s.Tag == old {
			s.Tag, changed = new, true
		}
		settings = s
	case Loop:
		s, err := SettingsOf(n, DefaultLoop())
		if err != nil {
			return n, false
		}
		fix(&s.Feedback)
		settings = s
	case OpenPR:
		s, err := SettingsOf(n, DefaultOpenPR(kind))
		if err != nil {
			return n, false
		}
		fix(&s.Title, &s.Body, &s.Branch)
		settings = s
	default:
		return n, false
	}
	if !changed {
		return n, false
	}
	b, err := json.Marshal(settings)
	if err != nil {
		return n, false
	}
	n.Settings = b
	return n, true
}

// Rename is the graph with the references to the tag old in the blocks other than except rewritten to
// new, and the names of the blocks it changed.
func (g Graph) Rename(kind, except, old, new string) (Graph, []string) {
	out := Graph{Nodes: append([]Node{}, g.Nodes...), Links: g.Links}
	var changed []string
	for i, n := range out.Nodes {
		if n.ID == except {
			continue
		}
		if r, ok := Retag(n, kind, old, new); ok {
			out.Nodes[i] = r
			changed = append(changed, g.NameOf(n))
		}
	}
	return out, changed
}

// NameFromTag is the name a block gets so that its tag is tag: value_summary is "Value summary".
func NameFromTag(tag string) string {
	words := strings.ReplaceAll(tag, "_", " ")
	if words == "" {
		return words
	}
	return strings.ToUpper(words[:1]) + words[1:]
}

// MigrateNames brings a graph saved before tags were always names up to date: a block whose saved
// Output to differs from its name's tag is renamed so that its tag is what it was (Value summary for
// value_summary), or, when another block already has that name, keeps its name and the references to
// the old tag are rewritten to its name's. Blocks with the same name are told apart with 2, 3 …. It
// returns the graph, the tags whose values move, and whether anything changed.
func (g Graph) MigrateNames(kind string) (Graph, []TagRename, bool) {
	out := Graph{Nodes: append([]Node{}, g.Nodes...), Links: g.Links}
	var renames []TagRename
	changed := false
	taken := func(except, slug string) bool {
		for _, o := range out.Nodes {
			if o.ID != except && !IsReference(o) && Slug(DisplayName(o)) == slug {
				return true
			}
		}
		return false
	}
	for i, n := range out.Nodes {
		stored := StoredOutputTo(n)
		if IsReference(n) || stored == "" || stored == Slug(DisplayName(n)) {
			continue
		}
		changed = true
		if name := NameFromTag(stored); Slug(name) == stored && !taken(n.ID, stored) {
			out.Nodes[i].Name = name // its tag stays what it was
		} else {
			// Another block has that name: this one keeps its own, and its references move to its tag.
			tag := freeTag(kind, Slug(DisplayName(n)), map[string]bool{})
			renames = append(renames, TagRename{Old: stored, New: tag})
			out, _ = out.Rename(kind, n.ID, stored, tag)
		}
		out.Nodes[i], _ = WithOutputTo(out.Nodes[i], kind, Slug(DisplayName(out.Nodes[i])))
	}
	// Two blocks with one name (an older canvas allowed it): the later ones are numbered.
	seen := map[string]bool{}
	for i, n := range out.Nodes {
		if IsReference(n) {
			continue
		}
		slug := Slug(DisplayName(n))
		for k := 2; seen[slug]; k++ {
			changed = true
			out.Nodes[i].Name = fmt.Sprintf("%s %d", DisplayName(n), k)
			slug = Slug(out.Nodes[i].Name)
		}
		seen[slug] = true
	}
	return out, renames, changed
}

// withoutOutputTo is the node with no output tag stored in its settings.
func withoutOutputTo(n Node) (Node, error) {
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(n.Settings, &fields); err != nil {
		return n, err
	}
	delete(fields, "output_to")
	b, err := json.Marshal(fields)
	n.Settings = b
	return n, err
}

// MigrateReferences brings the reference blocks of a graph saved before they shared their source's tag
// up to date (run it after MigrateNames, so the sources' tags are settled). A reference that has a name
// of its own (Discovery 2) or a saved tag has them cleared, every reference to its old tag in the
// workflow's templates ({discovery_2.report}, [[discovery_2]], a Condition's tag) is rewritten to the
// source's, and the values kept under the old tag move to the source's, where a value the source's tag
// already has stays. It returns the graph, the tags whose values move, and whether anything changed. A
// reference whose source is gone is left as it is.
func (g Graph) MigrateReferences(kind string) (Graph, []TagRename, bool) {
	out := Graph{Nodes: append([]Node{}, g.Nodes...), Links: g.Links}
	var renames []TagRename
	changed := false
	for i, n := range g.Nodes {
		src, ok := g.Source(n)
		stored := StoredOutputTo(n)
		if !ok || (n.Name == "" && stored == "") {
			continue
		}
		changed = true
		// The tag it had: the one saved on it, else its name's.
		old := stored
		if old == "" {
			old = Slug(DisplayName(n))
		}
		out.Nodes[i].Name = ""
		if cleared, err := withoutOutputTo(out.Nodes[i]); err == nil {
			out.Nodes[i] = cleared
		}
		tag := g.OutputNames(kind)[src.ID]
		if old != "" && old != tag {
			out, _ = out.Rename(kind, "", old, tag)
			renames = append(renames, TagRename{Old: old, New: tag, Keep: true})
		}
	}
	return out, renames, changed
}
