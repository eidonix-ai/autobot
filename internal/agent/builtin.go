package agent

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/eidonix-ai/autobot/internal/store"
)

// The agent's own tools, handled by the harness itself: finish and update_plan (agent.go), pruning
// results, compacting, and the scratchpad, which lives outside the conversation.

// Pad is a scratchpad: the notes of one thread. *store.Pad is one. Entries are only ever added.
type Pad interface {
	Add(store.PadEntry) (int, error)
	List() ([]store.PadEntry, error)
}

var pruneSpec = ToolSpec{Name: "prune_result", Description: "Drop tool results you have finished with from your context. Every tool result starts with its number, " +
	"[result N]; each one named here is replaced by its number, its first line and its length. Prune only once the context line shows your context is past half its window: " +
	"before that, pruning only costs a cache miss and throws detail away. Save what matters to your scratchpad first, " +
	"and prune several results at once: each prune makes the next request slower to start.",
	Parameters: json.RawMessage(`{"type":"object","properties":{"results":{"type":"array","items":{"type":"integer"},"description":"The numbers of the results to prune."}},"required":["results"]}`)}

var compactSpec = ToolSpec{Name: "compact", Description: "Start over with a small context: your earlier messages are dropped and you continue from the task, " +
	"what you carry forward, your scratchpad index and your plan. Save what you need to the scratchpad first.",
	Parameters: json.RawMessage(`{"type":"object","properties":{"carry_forward":{"type":"string","description":"What you need to carry on: where you are, what you decided, what is next."}},"required":["carry_forward"]}`)}

var padWriteSpec = ToolSpec{Name: "scratchpad_write", Description: "Save a note to your scratchpad, which is kept outside your context and survives pruning and compacting. " +
	"Save each finding worth keeping as you go, with its citation (file and lines, pull request or issue numbers, links).",
	Parameters: json.RawMessage(`{"type":"object","properties":{
		"title":{"type":"string","description":"A short title."},
		"importance":{"type":"string","enum":["low","medium","high"],"description":"How much the final report needs it. Be honest: high is for what the report cannot do without."},
		"description":{"type":"string","description":"One line saying what the note holds."},
		"body":{"type":"string","description":"The note itself, with its citations."}},"required":["title","importance","description","body"]}`)}

var padIndexSpec = ToolSpec{Name: "scratchpad_index", Description: "List your scratchpad: one line per note, with its number, importance, title and description.",
	Parameters: json.RawMessage(`{"type":"object","properties":{"importance":{"type":"string","enum":["low","medium","high"],"description":"Only notes of this importance."}}}`)}

var padReadSpec = ToolSpec{Name: "scratchpad_read", Description: "Read notes of your scratchpad in full, by number.",
	Parameters: json.RawMessage(`{"type":"object","properties":{"entries":{"type":"array","items":{"type":"integer"},"description":"The numbers of the notes."}},"required":["entries"]}`)}

var padSearchSpec = ToolSpec{Name: "scratchpad_search", Description: "Search the titles, descriptions and bodies of your notes with a regular expression (Go syntax; start it with (?i) to ignore case). " +
	"Answers with the matching notes' index lines and the lines of their bodies that match.",
	Parameters: json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string","description":"A regular expression."}},"required":["pattern"]}`)}

// researchSpecs are the agent's own tools in a research session; writerSpecs in the session that
// writes the report from the scratchpad, which reads it but adds nothing and never compacts.
var (
	researchSpecs = []ToolSpec{finishSpec, planSpec, pruneSpec, compactSpec, padWriteSpec, padIndexSpec, padReadSpec, padSearchSpec}
	writerSpecs   = []ToolSpec{finishSpec, planSpec, pruneSpec, padIndexSpec, padReadSpec, padSearchSpec}
)

// forcedTools are what the agent is offered on the turn it is told to compact.
var forcedTools = []string{"scratchpad_write", "update_plan", "compact"}

// harnessTool reports whether a tool is the harness's own, run by it and not by a Tool.
func harnessTool(name string) bool {
	return slices.ContainsFunc(researchSpecs, func(s ToolSpec) bool { return s.Name == name })
}

func hasTool(specs []ToolSpec, name string) bool {
	return slices.ContainsFunc(specs, func(s ToolSpec) bool { return s.Name == name })
}

// Importance is a note's importance as written: low, medium or high, "med" for medium; ok is false
// for any other word, which is kept as medium.
func Importance(word string) (imp string, ok bool) {
	switch w := strings.ToLower(strings.TrimSpace(word)); w {
	case store.Low, store.Medium, store.High:
		return w, true
	case "med":
		return store.Medium, true
	}
	return store.Medium, false
}

// indexLine is one note as the index lists it: #7 [high] Title — description, and its author when
// it is not the main agent.
func indexLine(e store.PadEntry) string {
	line := fmt.Sprintf("#%d [%s] %s — %s", e.N, e.Importance, e.Title, e.Description)
	if e.Author != "main" {
		line += " (by " + e.Author + ")"
	}
	return line
}

// Index lists a scratchpad's notes, optionally only those of one importance; the text says so when
// there are none.
func Index(pad Pad, importance string) string {
	if pad == nil {
		return "(no scratchpad)"
	}
	all, err := pad.List()
	if err != nil {
		return "(the scratchpad could not be read: " + err.Error() + ")"
	}
	var lines []string
	for _, e := range all {
		if importance == "" || e.Importance == importance {
			lines = append(lines, indexLine(e))
		}
	}
	if len(lines) == 0 {
		return "(the scratchpad has no notes)"
	}
	return strings.Join(lines, "\n")
}

// kilo writes a token count in thousands: 412k, 1,000k.
func kilo(n int) string {
	s := strconv.Itoa((n + 500) / 1000)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s + "k"
}

// contextLine is what the agent is shown of its context once it passes half the window.
func (s *session) contextLine() string {
	if s.conv.lastIn*2 <= s.window() {
		return ""
	}
	return fmt.Sprintf("[context: %s of %s tokens]", kilo(s.conv.lastIn), kilo(s.window()))
}

// withContext adds the context line, if there is one, to a tool's answer.
func (s *session) withContext(answer string) string {
	if line := s.contextLine(); line != "" {
		s.lineShown = true
		return answer + "\n" + line
	}
	return answer
}

func (s *session) prune(args string) string {
	var a struct {
		Results []int `json:"results"`
	}
	if json.Unmarshal([]byte(args), &a) != nil || len(a.Results) == 0 {
		return `error: prune_result takes {"results": [numbers]}`
	}
	done := s.conv.prune(a.Results)
	var missed []string
	for _, n := range slices.Compact(slices.Sorted(slices.Values(a.Results))) {
		if !slices.Contains(done, n) {
			missed = append(missed, strconv.Itoa(n))
		}
	}
	out := fmt.Sprintf("Pruned %d results.", len(done))
	if len(missed) > 0 {
		out += " Not pruned (no such result, or already pruned): " + strings.Join(missed, ", ") + "."
	}
	return s.withContext(out)
}

func (s *session) padWrite(args string) string {
	if s.cfg.Pad == nil {
		return "error: there is no scratchpad"
	}
	var a struct{ Title, Importance, Description, Body string }
	if json.Unmarshal([]byte(args), &a) != nil {
		return `error: scratchpad_write takes {"title", "importance", "description", "body"}`
	}
	if strings.TrimSpace(a.Title) == "" && strings.TrimSpace(a.Body) == "" {
		return "error: a note needs a title and a body"
	}
	// A note is never refused for its label or its form: losing it is worse than a wrong label.
	if strings.TrimSpace(a.Title) == "" {
		a.Title = firstLine(a.Body, 60)
	}
	if strings.TrimSpace(a.Description) == "" {
		a.Description = firstLine(a.Body, 160)
	}
	imp, ok := Importance(a.Importance)
	n, err := s.cfg.Pad.Add(store.PadEntry{Author: "main", Title: strings.Join(strings.Fields(a.Title), " "), Importance: imp, Description: a.Description, Body: a.Body})
	if err != nil {
		return "error: the note could not be saved: " + err.Error()
	}
	out := fmt.Sprintf("Saved as entry %d.", n)
	if !ok {
		out += fmt.Sprintf(" Importance %q is not low, medium or high, so it was saved as medium.", a.Importance)
	}
	return out
}

func (s *session) padIndex(args string) string {
	var a struct{ Importance string }
	json.Unmarshal([]byte(args), &a)
	imp := ""
	if strings.TrimSpace(a.Importance) != "" {
		var ok bool
		if imp, ok = Importance(a.Importance); !ok {
			return "error: importance is low, medium or high"
		}
	}
	return Index(s.cfg.Pad, imp)
}

func (s *session) padRead(args string) string {
	var a struct{ Entries []int }
	if json.Unmarshal([]byte(args), &a) != nil || len(a.Entries) == 0 {
		return `error: scratchpad_read takes {"entries": [numbers]}`
	}
	if s.cfg.Pad == nil {
		return "error: there is no scratchpad"
	}
	all, err := s.cfg.Pad.List()
	if err != nil {
		return "error: the scratchpad could not be read: " + err.Error()
	}
	var parts, missing []string
	for _, n := range a.Entries {
		i := slices.IndexFunc(all, func(e store.PadEntry) bool { return e.N == n })
		if i < 0 {
			missing = append(missing, strconv.Itoa(n))
			continue
		}
		e := all[i]
		parts = append(parts, indexLine(e)+"\n\n"+e.Body)
	}
	out := strings.Join(parts, "\n\n---\n\n")
	if len(missing) > 0 {
		out += "\n\nNo such entries: " + strings.Join(missing, ", ") + "."
	}
	return strings.TrimSpace(out)
}

const maxSearchLines = 8 // matching body lines shown for one note

func (s *session) padSearch(args string) string {
	var a struct{ Pattern string }
	if json.Unmarshal([]byte(args), &a) != nil || a.Pattern == "" {
		return `error: scratchpad_search takes {"pattern": a regular expression}`
	}
	re, err := regexp.Compile(a.Pattern)
	if err != nil {
		return "error: not a regular expression: " + err.Error()
	}
	if s.cfg.Pad == nil {
		return "error: there is no scratchpad"
	}
	all, err := s.cfg.Pad.List()
	if err != nil {
		return "error: the scratchpad could not be read: " + err.Error()
	}
	var out []string
	for _, e := range all {
		var hits []string
		for i, line := range strings.Split(e.Body, "\n") {
			if re.MatchString(line) && len(hits) < maxSearchLines {
				hits = append(hits, fmt.Sprintf("  line %d: %s", i+1, firstLine(line, 300)))
			}
		}
		if len(hits) == 0 && !re.MatchString(e.Title) && !re.MatchString(e.Description) {
			continue
		}
		out = append(out, strings.Join(append([]string{indexLine(e)}, hits...), "\n"))
	}
	if len(out) == 0 {
		return "No note matches."
	}
	return strings.Join(out, "\n")
}

// firstLine is a text's first line, at most max characters.
func firstLine(s string, max int) string {
	first, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	if r := []rune(first); len(r) > max {
		first = string(r[:max]) + "…"
	}
	return strings.TrimSpace(first)
}

// carriedByAutobot is what a conversation compacted by Autobot, not by the agent, carries forward.
const carriedByAutobot = "(compacted by Autobot: your notes are in the scratchpad)"

// compact replaces the conversation by the task and what the agent carries forward, with its
// scratchpad index and its plan.
func (s *session) compact(carry string) {
	var b strings.Builder
	b.WriteString("Your earlier work was compacted. What you carried forward:\n")
	b.WriteString(strings.TrimSpace(carry))
	b.WriteString("\n\nYour scratchpad (read notes with scratchpad_read, search them with scratchpad_search):\n")
	b.WriteString(Index(s.cfg.Pad, ""))
	b.WriteString("\n\nYour plan:\n")
	if len(s.tr.Plan) == 0 {
		b.WriteString("(none yet)")
	}
	for _, p := range s.tr.Plan {
		fmt.Fprintf(&b, "- [%s] %s", p.Status, p.Step)
		if p.Note != "" {
			b.WriteString(" (" + p.Note + ")")
		}
		b.WriteString("\n")
	}
	s.conv.compactTo(Message{Role: "user", Content: s.cfg.Task}, Message{Role: "user", Content: strings.TrimRight(b.String(), "\n")})
}

// tooLong reports whether a provider refused a request for being longer than its model's context.
func tooLong(err error) bool {
	s := strings.ToLower(err.Error())
	if !strings.Contains(s, " 400 ") {
		return false
	}
	for _, w := range []string{"context length", "context_length_exceeded", "maximum context", "too long", "context window"} {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}
