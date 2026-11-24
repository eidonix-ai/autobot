package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The step nodes, which run after an event: an agent (a model with tools, working until it has an
// answer) and a decision (a model choosing one of a fixed set of answers).
const (
	Agent     = "agent"
	Decision  = "decision"
	CaseEntry = "case_entry" // writes an entry in the item's case file, and nothing else
)

// CaseEntrySettings are a case entry node's settings. The entry is a contribution to the case file
// written under the node's name: finalized, it is appended and can no longer change; left open, it
// stays a draft that the node's next run on the same item revises.
type CaseEntrySettings struct {
	Entry    string `json:"entry"` // a template
	Finalize bool   `json:"finalize"`
}

// DefaultCaseEntry is a new case entry node's settings.
func DefaultCaseEntry() CaseEntrySettings {
	return CaseEntrySettings{Entry: "{input.report}", Finalize: true}
}

// CaseEntrySettingsOf reads a case entry node's settings, or the defaults when it has none yet.
func CaseEntrySettingsOf(n Node) (CaseEntrySettings, error) {
	if len(n.Settings) == 0 {
		return DefaultCaseEntry(), nil
	}
	var s CaseEntrySettings
	if err := json.Unmarshal(n.Settings, &s); err != nil {
		return s, errors.New("Case Entry: unreadable settings")
	}
	return s, nil
}

// Check confirms a case entry's settings are complete.
func (s CaseEntrySettings) Check() error {
	switch {
	case strings.TrimSpace(s.Entry) == "":
		return errors.New("write the entry")
	case len(s.Entry) > maxTemplate:
		return fmt.Errorf("the entry is limited to %d characters", maxTemplate)
	}
	return nil
}

// CaseEntryFromForm reads a case entry's settings from its form.
func CaseEntryFromForm(get func(string) string) CaseEntrySettings {
	return CaseEntrySettings{Entry: strings.TrimSpace(get("entry")), Finalize: get("finalize") == "on"}
}

// ModelRef names a model on a provider. Which models a workspace may use is the web layer's to
// check, at the moment a node's settings are saved and again when it runs.
type ModelRef struct {
	Provider int64  `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
}

// Set reports whether a model has been chosen.
func (m ModelRef) Set() bool { return m.Provider != 0 && m.Model != "" }

// AgentSettings are an agent node's settings.
type AgentSettings struct {
	Model        ModelRef `json:"model"`
	Persona      string   `json:"persona"`       // a preset's key (its Instruction and Done when are used), or "custom"
	SystemPrompt string   `json:"system_prompt"` // a template: Additional instructions, added after the Instruction
	Instruction  string   `json:"instruction"`   // a template: the task, sent as the first message
	Goal         string   `json:"goal"`          // a template: when the work is done; the agent may finish only then
	Internet     bool     `json:"internet"`      // web search and fetching pages
	SubAgents    int      `json:"sub_agents"`    // how many sub-agents it may have working at once; 0 for none
	// SubAgentsPerRun, when above 0, is how many sub-agents one run may start in all.
	SubAgentsPerRun int `json:"sub_agents_per_run,omitempty"`
	// TimeWarnPercent is how much of the time limit is left when every session of the run still working
	// is told so, once; 0 for no warning. A block saved before it was set (nil) has 10.
	TimeWarnPercent *int     `json:"time_warn_percent,omitempty"`
	Machines        []int64  `json:"machines"`               // the workspace machines it may use
	Verify          ModelRef `json:"verify"`                 // a decision model that checks a finish claiming the goal is met; unset for none
	MaxTurns        int      `json:"max_turns"`              // model calls before it must have finished
	Closing         string   `json:"closing,omitempty"`      // Custom only: "" (finish delivers the output) or "report" (written from the scratchpad)
	ReportTurns     int      `json:"report_turns,omitempty"` // with a written report: turns the writing session has; 0 is 15
	MaxMinutes      int      `json:"max_minutes"`            // wall time before it is stopped
	Rerun           string   `json:"rerun"`                  // run again on the same item: "resume" its conversation, or start "fresh"
	FollowUp        string   `json:"follow_up"`              // resume: a template sent as the next message; "" sends the instruction again
	OutputTo        string   `json:"output_to,omitempty"`

	// ContinueOf, when set, is the id of another Agent block of the same canvas whose conversation on
	// the item this block continues: it sends its own Instruction and Done when as the next message of
	// that conversation, which keeps the model, system prompt and tools it was started with. Its own
	// Model is then only a copy of the other block's, shown read-only. NoConversation says what it
	// does when the item has none yet: "skip" (the default) takes its output with a note and lets the
	// workflow go on, "fresh" starts a conversation of its own with its Instruction.
	ContinueOf     string `json:"continue_of,omitempty"`
	NoConversation string `json:"no_conversation,omitempty"`
}

// Continues reports whether the block continues another block's conversation.
func (s AgentSettings) Continues() bool { return s.ContinueOf != "" }

// FreshWithoutConversation reports whether, with no conversation to continue, the block starts one.
func (s AgentSettings) FreshWithoutConversation() bool { return s.NoConversation == "fresh" }

// UsesMachine reports whether the agent may use a machine, for the settings form.
func (s AgentSettings) UsesMachine(id int64) bool { return slices.Contains(s.Machines, id) }

// Choice is one answer a decision can give, and one output of its node.
type Choice struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// DecisionSettings are a decision node's settings.
type DecisionSettings struct {
	Model    ModelRef `json:"model"`
	Context  string   `json:"context"`  // a template: what the decision is about
	Question string   `json:"question"` // a template
	Choices  []Choice `json:"choices"`
	Output   string   `json:"output"` // "decision": only the choice; "probabilities": each choice's probability too
	OutputTo string   `json:"output_to,omitempty"`
}

// DefaultAgent is a new agent node's settings, for a workflow of kind.
func DefaultAgent(kind string) AgentSettings {
	// A preset's Instruction and Done when are its own: the block stores only which preset.
	return AgentSettings{Persona: Personas[0].Key, MaxTurns: 60, MaxMinutes: 20, Rerun: "resume"}
}

// EffectiveInstruction is the instruction the agent is given: its preset's, or its own when Custom.
func (s AgentSettings) EffectiveInstruction(kind string) string {
	if p := PersonaOf(s.Persona); p != nil {
		return p.InstructionFor(kind)
	}
	return s.Instruction
}

// Closing steps: what happens after the agent's research.
const (
	ClosingNone   = ""       // finish delivers the output
	ClosingReport = "report" // the report is written from the scratchpad, in a fresh session
)

// DefaultTimeWarn is the share of the time limit, in percent, left when the sessions of a run are warned.
const DefaultTimeWarn = 10

// EffectiveTimeWarn is the time warning in percent of the time limit; 0 is none.
func (s AgentSettings) EffectiveTimeWarn() int {
	if s.TimeWarnPercent == nil {
		return DefaultTimeWarn
	}
	return *s.TimeWarnPercent
}

// DefaultReportTurns is how many turns the session that writes a report has, when not set.
const DefaultReportTurns = 15

// EffectiveClosing is the closing step: its preset's, or its own when Custom.
func (s AgentSettings) EffectiveClosing(kind string) string {
	if p := PersonaOf(s.Persona); p != nil {
		return p.Closing
	}
	return s.Closing
}

// EffectiveReportTurns is how many turns the writing session has.
func (s AgentSettings) EffectiveReportTurns() int {
	if s.ReportTurns <= 0 {
		return DefaultReportTurns
	}
	return s.ReportTurns
}

// EffectiveGoal is the Done when the agent is held to: its preset's, or its own when Custom.
func (s AgentSettings) EffectiveGoal(kind string) string {
	if p := PersonaOf(s.Persona); p != nil {
		return p.GoalFor(kind)
	}
	return s.Goal
}

// DefaultDecision is a new decision node's settings, for a workflow of kind.
func DefaultDecision(kind string) DecisionSettings {
	s := DecisionSettings{Output: "decision", Choices: []Choice{
		{"yes", "It should."}, {"no", "It should not."}}}
	if kind == Issue {
		s.Context, s.Question = "{issue:title}\n\n{issue:body}", "Is this issue a bug report?"
	} else {
		s.Context, s.Question = "{pr:title}\n\n{pr:body}\n\n{pr:diff}", "Does this pull request need an in-depth review?"
	}
	return s
}

// AgentSettingsOf reads an agent node's settings, or the defaults when it has none yet.
func AgentSettingsOf(n Node, kind string) (AgentSettings, error) {
	if len(n.Settings) == 0 {
		return DefaultAgent(kind), nil
	}
	var s AgentSettings
	if err := json.Unmarshal(n.Settings, &s); err != nil {
		return s, errors.New("Agent: unreadable settings")
	}
	return s, nil
}

// DecisionSettingsOf reads a decision node's settings, or the defaults when it has none yet.
func DecisionSettingsOf(n Node, kind string) (DecisionSettings, error) {
	if len(n.Settings) == 0 {
		return DefaultDecision(kind), nil
	}
	var s DecisionSettings
	if err := json.Unmarshal(n.Settings, &s); err != nil {
		return s, errors.New("Decision: unreadable settings")
	}
	return s, nil
}

const maxTemplate = 50_000

var (
	tagNameRE    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
	choiceNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)
)

// checkOutputTo checks an output tag's name: a word that is not one of the built-in tags.
func checkOutputTo(name, kind string) error {
	if name == "" {
		return nil
	}
	if !tagNameRE.MatchString(name) {
		return errors.New("an output tag is a lower-case word of up to 40 letters, digits and _, such as value_summary")
	}
	if builtIn(name, kind) {
		return fmt.Errorf("{%s} is a built-in tag; choose another name for the output", name)
	}
	return nil
}

// Check confirms an agent's settings are complete and sensible.
func (s AgentSettings) Check(kind string) error {
	switch {
	case !s.Model.Set() && !s.Continues():
		return errors.New("choose a model")
	case s.NoConversation != "" && s.NoConversation != "skip" && s.NoConversation != "fresh":
		return errors.New("choose what to do when the item has no earlier report")
	case PersonaOf(s.Persona) == nil && s.Persona != "custom":
		return errors.New("choose a preset, or Custom")
	case strings.TrimSpace(s.EffectiveInstruction(kind)) == "":
		return errors.New("write the instruction")
	case strings.TrimSpace(s.EffectiveGoal(kind)) == "":
		return errors.New("say when the work is done")
	case len(s.Instruction) > maxTemplate || len(s.SystemPrompt) > maxTemplate || len(s.FollowUp) > maxTemplate || len(s.Goal) > maxTemplate:
		return fmt.Errorf("prompts are limited to %d characters", maxTemplate)
	case s.SubAgents < 0 || s.SubAgents > 16:
		return errors.New("sub-agents at a time are 0 to 16")
	case s.SubAgentsPerRun < 0 || s.SubAgentsPerRun > 500:
		return errors.New("the limit of sub-agents per run is 1 to 500")
	case s.TimeWarnPercent != nil && (*s.TimeWarnPercent < 0 || *s.TimeWarnPercent > 50):
		return errors.New("the time warning is 0 to 50 percent of the time limit")
	case s.MaxTurns < 1 || s.MaxTurns > 500:
		return errors.New("the turn limit is 1 to 500")
	case s.MaxMinutes < 1 || s.MaxMinutes > 240:
		return errors.New("the time limit is 1 to 240 minutes")
	case s.Closing != ClosingNone && s.Closing != ClosingReport:
		return errors.New("choose a closing step")
	case s.ReportTurns < 0 || s.ReportTurns > 200:
		return errors.New("the report's turn limit is 1 to 200")
	case s.Rerun != "resume" && s.Rerun != "fresh":
		return errors.New("choose what a re-run does")
	}
	return checkOutputTo(s.OutputTo, kind)
}

// Templates are the agent's templates, for checking and filling their tags.
func (s AgentSettings) Templates(kind string) []string {
	return []string{s.EffectiveInstruction(kind), s.SystemPrompt, s.EffectiveGoal(kind), s.FollowUp}
}

// Templates are the decision's templates.
func (s DecisionSettings) Templates() []string { return []string{s.Context, s.Question} }

// Check confirms a decision's settings are complete and sensible.
func (s DecisionSettings) Check(kind string) error {
	switch {
	case !s.Model.Set():
		return errors.New("choose a decision model")
	case strings.TrimSpace(s.Question) == "":
		return errors.New("write the question")
	case len(s.Context) > maxTemplate || len(s.Question) > maxTemplate:
		return fmt.Errorf("the context and question are limited to %d characters", maxTemplate)
	case len(s.Choices) < 2 || len(s.Choices) > 12:
		return errors.New("give 2 to 12 choices, one per line, as name: description")
	case s.Output != "decision" && s.Output != "probabilities":
		return errors.New("choose what the decision outputs")
	}
	seen := map[string]bool{}
	for _, c := range s.Choices {
		if !choiceNameRE.MatchString(c.Name) {
			return fmt.Errorf("choice %q: a choice's name is a lower-case word of up to 40 letters, digits, - and _", c.Name)
		}
		if seen[c.Name] {
			return fmt.Errorf("two choices are named %s", c.Name)
		}
		if len(c.Description) > 500 {
			return fmt.Errorf("choice %s: descriptions are limited to 500 characters", c.Name)
		}
		seen[c.Name] = true
	}
	return checkOutputTo(s.OutputTo, kind)
}

// ChoiceLines writes the choices as the settings form shows them: one "name: description" a line.
func (s DecisionSettings) ChoiceLines() string {
	var b strings.Builder
	for _, c := range s.Choices {
		b.WriteString(c.Name)
		if c.Description != "" {
			b.WriteString(": " + c.Description)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// AgentFromForm reads an agent's settings from its form; the model is the web layer's to fill in.
func AgentFromForm(get func(string) string, all func(string) []string) AgentSettings {
	s := AgentSettings{
		Persona:      get("persona"),
		SystemPrompt: strings.TrimSpace(get("system_prompt")),
		Instruction:  strings.TrimSpace(get("instruction")),
		Goal:         strings.TrimSpace(get("goal")),
		Internet:     get("internet") == "on",
		Rerun:        get("rerun"),
		Closing:      get("closing"),
		FollowUp:     strings.TrimSpace(get("follow_up")),
		OutputTo:     strings.TrimSpace(get("output_to")),
		Machines:     []int64{},
	}
	if s.ContinueOf = get("continue_of"); s.ContinueOf != "" {
		s.NoConversation = get("no_conversation")
	}
	for _, m := range all("machine") {
		if id, err := strconv.ParseInt(m, 10, 64); err == nil {
			s.Machines = append(s.Machines, id)
		}
	}
	s.SubAgents, _ = strconv.Atoi(get("sub_agents"))
	s.MaxTurns, _ = strconv.Atoi(get("max_turns"))
	s.MaxMinutes, _ = strconv.Atoi(get("max_minutes"))
	s.ReportTurns, _ = strconv.Atoi(get("report_turns"))
	if get("limit_sub_agents") == "on" { // the checkbox: unchecked, whatever the number says, is no limit
		if n, err := strconv.Atoi(get("sub_agents_per_run")); err == nil && n >= 1 {
			s.SubAgentsPerRun = n
		} else {
			s.SubAgentsPerRun = -1 // refused by Check
		}
	}
	if v := strings.TrimSpace(get("time_warn_percent")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			n = -1 // refused by Check
		}
		s.TimeWarnPercent = &n
	}
	return s
}

// DecisionFromForm reads a decision's settings from its form; the model is the web layer's to
// fill in.
func DecisionFromForm(get func(string) string) DecisionSettings {
	s := DecisionSettings{
		Context:  strings.TrimSpace(get("context")),
		Question: strings.TrimSpace(get("question")),
		Output:   get("output"),
		OutputTo: strings.TrimSpace(get("output_to")),
	}
	for _, line := range strings.Split(get("choices"), "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		name, desc, _ := strings.Cut(line, ":")
		s.Choices = append(s.Choices, Choice{Name: strings.TrimSpace(name), Description: strings.TrimSpace(desc)})
	}
	return s
}

// Tag is a placeholder a template may use.
//
// Built-in tags are parents and their fields (Derek, 2026-11-23): {pr} is the pull request's link,
// and {pr:title}, {pr:diff}… are filled from it by Autobot; {repo} and {charter} likewise from
// the workspace. A tag without a colon that is not a parent is another step's output. So only a
// parent is ever asked for, and a field is visibly not something to fill in.
type Tag struct {
	Name, About string
	Auto        bool // filled in by Autobot, never typed or set
}

// Field reports whether the tag belongs under a parent: a field of it ({pr:title} of {pr}), or a
// part of an Agent's output ({gather.report} of {gather}).
func (t Tag) Field() bool { return strings.ContainsAny(t.Name, ":.") }

// OutputParts are the parts of an Agent block's output, which a template names with a dot after
// the output tag ({gather.report}). The tag itself holds all of them under headings.
var OutputParts = []Tag{
	{"summary", "the summary", false},
	{"report", "the full report", false},
	{"scratchpad", "its notes in full", false},
}

func isOutputPart(name string) bool {
	return slices.ContainsFunc(OutputParts, func(p Tag) bool { return p.Name == name })
}

var commonTags = []Tag{
	{"repo", "the repository, owner/name", true},
	{"charter", "the project charter (kept in Autobot)", true},
	{"input", "what the step before produced: its summary, report and notes", false},
	{"input.summary", "the summary", false},
	{"input.report", "the full report", false},
	{"input.scratchpad", "its notes in full", false},
}

var prTags = []Tag{
	{"pr", "the pull request's link", false},
	{"pr:number", "its number", true},
	{"pr:title", "its title", true},
	{"pr:body", "its description", true},
	{"pr:author", "its author", true},
	{"pr:diff", "its diff", true},
	{"pr:files", "the files it changes", true},
	{"pr:case_file", "its case file so far", true},
	{"pr:new_commits", "in a continued conversation: the commits pushed since its last turn, with their diff", true},
	{"pr:new_comments", "in a continued conversation: the comments and reviews posted since its last turn", true},
}

var issueTags = []Tag{
	{"issue", "the issue's link", false},
	{"issue:number", "its number", true},
	{"issue:title", "its title", true},
	{"issue:body", "its description", true},
	{"issue:author", "its author", true},
	{"issue:case_file", "its case file so far", true},
}

// oldTags are the names built-in tags had before (pr_title for pr:title), which templates written
// then still use.
var oldTags = map[string]string{
	"pr_link": "pr", "pr_number": "pr:number", "pr_title": "pr:title", "pr_body": "pr:body", "pr_author": "pr:author",
	"pr_diff": "pr:diff", "pr_files": "pr:files", "pr_new_commits": "pr:new_commits", "pr_new_comments": "pr:new_comments",
	"issue_link": "issue", "issue_number": "issue:number", "issue_title": "issue:title", "issue_body": "issue:body",
	"issue_author": "issue:author", "project_charter": "charter", "repo:charter": "charter",
	// What the step before produced was {previous_output}: its report after an agent, its output
	// after any other step, each under its name where several merge. That is {input.report} still.
	"previous_output": "input.report",
}

// Tags lists the built-in tags of a workflow kind.
func Tags(kind string) []Tag {
	if kind == Issue {
		return append(append([]Tag{}, issueTags...), commonTags...)
	}
	return append(append([]Tag{}, prTags...), commonTags...)
}

// CanonicalTag is a tag's current name: an old name (pr_title) becomes its new one (pr:title), and
// {case_file} the case file of the kind's item.
func CanonicalTag(name, kind string) string {
	if n, ok := oldTags[name]; ok {
		return n
	}
	if name == "case_file" {
		return ItemTag(kind) + ":case_file"
	}
	return name
}

// ItemTag is the parent tag of a workflow kind's item: pr or issue.
func ItemTag(kind string) string {
	if kind == Issue {
		return "issue"
	}
	return "pr"
}

// TagField is a field tag's parent (pr for pr:title), or "" for a tag that is no field.
func TagField(name string) string {
	parent, _, ok := strings.Cut(name, ":")
	if !ok {
		return ""
	}
	return parent
}

func builtIn(name, kind string) bool {
	if name == "case_link" { // an old tag, never filled: still accepted so old templates save
		return true
	}
	name = CanonicalTag(name, kind)
	for _, t := range Tags(kind) {
		if t.Name == name {
			return true
		}
	}
	return false
}

// tagRE finds the tags of a template: a word, a word:field (Autobot fills a parent's fields) or a
// word.part (a part of a step's output), in braces. Doubled braces are literal ({{word}} is the
// text {word}), and braces around anything else, as in JSON, are just text.
var tagRE = regexp.MustCompile(`\{\{|\}\}|\{([a-z][a-z0-9_]*(?:[:.][a-z][a-z0-9_]*)?)\}`)

// TemplateTags lists the tags a template uses, as written, in order, each once.
func TemplateTags(text string) []string {
	var out []string
	for _, m := range tagRE.FindAllStringSubmatch(text, -1) {
		if m[1] != "" && !has(out, m[1]) {
			out = append(out, m[1])
		}
	}
	return out
}

// Render fills a template's tags from values, which are keyed by current names; an old name is read
// as its new one. A tag with no value becomes empty text.
func Render(text string, values map[string]string) string {
	return tagRE.ReplaceAllStringFunc(text, func(m string) string {
		switch m {
		case "{{":
			return "{"
		case "}}":
			return "}"
		}
		name := m[1 : len(m)-1]
		if v, ok := values[name]; ok {
			return v
		}
		if n, ok := oldTags[name]; ok {
			return values[n]
		}
		if name == "case_file" {
			return values["pr:case_file"] + values["issue:case_file"]
		}
		return ""
	})
}

// OutputSet is the output tags a workflow's nodes set: true for an Agent's, which has parts
// ({gather.report}), false for a Decision's, which is one value.
type OutputSet map[string]bool

// OutputTags lists the output tags set by the workflow's nodes other than except.
func (g Graph) OutputTags(kind, except string) OutputSet {
	out := OutputSet{}
	for _, n := range g.Nodes {
		if n.ID == except {
			continue
		}
		switch n.Type {
		case Agent:
			if s, err := AgentSettingsOf(n, kind); err == nil && s.OutputTo != "" {
				out[s.OutputTo] = true
			}
		case Decision:
			if s, err := DecisionSettingsOf(n, kind); err == nil && s.OutputTo != "" {
				if _, taken := out[s.OutputTo]; !taken {
					out[s.OutputTo] = false
				}
			}
		}
	}
	return out
}

// OutputTagList is what the Tags list shows of the outputs: each tag, with an Agent's parts after
// it, in order.
func OutputTagList(outputs OutputSet) []Tag {
	var list []Tag
	for _, name := range slices.Sorted(maps.Keys(outputs)) {
		if !outputs[name] {
			list = append(list, Tag{name, "another step's output", false})
			continue
		}
		list = append(list, Tag{name, "its summary, report and notes", false})
		for _, p := range OutputParts {
			list = append(list, Tag{name + "." + p.Name, p.About, false})
		}
	}
	return list
}

// CheckTemplates confirms that every tag the templates use is built in or set by another node of
// the workflow (outputs). It is checked when a node's settings are saved, not on every save of the
// canvas, so removing a node is never refused because another uses its output.
//
// A colon is for the fields Autobot fills from a built-in parent ({pr:title}); a dot is for a part
// of an Agent's output ({gather.report}), which no other tag has.
func CheckTemplates(kind string, outputs OutputSet, templates ...string) error {
	for _, t := range templates {
		for _, tag := range TemplateTags(t) {
			if err := checkTag(kind, outputs, tag); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkTag(kind string, outputs OutputSet, tag string) error {
	if builtIn(tag, kind) {
		return nil
	}
	if parent, part, dotted := strings.Cut(tag, "."); dotted {
		isAgent, isOutput := outputs[parent]
		switch {
		case parent == "input":
			return fmt.Errorf("{%s} is not a part of {input}: see the Tags list for its parts", tag)
		case builtIn(parent, kind):
			return fmt.Errorf("{%s} is not a tag here: the fields of the built-in {%s} use a colon; see the Tags list", tag, parent)
		case !isOutput:
			return fmt.Errorf("{%s} is not a tag here: {%s} is not another step's output tag; see the Tags list", tag, parent)
		case !isAgent:
			return fmt.Errorf("{%s} is not a tag here: a decision's output is one value, written {%s}", tag, parent)
		case !isOutputPart(part):
			return fmt.Errorf("{%s} is not a part of an output: see the Tags list for the parts of {%s}", tag, parent)
		}
		return nil
	}
	if parent := TagField(tag); parent != "" {
		isAgent, isOutput := outputs[parent]
		switch {
		case isAgent || parent == "input":
			return fmt.Errorf("{%s} is not a tag here: the parts of a step's output use a dot, such as {%s.report}", tag, parent)
		case isOutput:
			return fmt.Errorf("{%s} is not a tag here: a decision's output is one value, written {%s}", tag, parent)
		}
		return fmt.Errorf("{%s} is not a tag here: see the Tags list for the fields of {%s}", tag, parent)
	}
	if _, ok := outputs[tag]; !ok {
		return fmt.Errorf("{%s} is not a tag here: use a built-in tag or another step's output tag, or write {{%s}} for the text itself", tag, tag)
	}
	return nil
}

// stepSummary is the one line a step node shows on the canvas. models names a model reference.
func stepSummary(n Node, kind string, models func(ModelRef) string) string {
	switch n.Type {
	case Agent:
		s, err := AgentSettingsOf(n, kind)
		if err != nil || (!s.Model.Set() && !s.Continues()) {
			return "Choose a model"
		}
		var parts []string
		if !s.Continues() {
			parts = append(parts, models(s.Model))
		}
		if s.OutputTo != "" {
			parts = append(parts, "→ {"+s.OutputTo+"}")
		}
		return strings.Join(parts, " · ")
	case Decision:
		s, err := DecisionSettingsOf(n, kind)
		if err != nil || !s.Model.Set() {
			return "Choose a model"
		}
		q := s.Question
		if utf8.RuneCountInString(q) > 60 {
			q = string([]rune(q)[:59]) + "…"
		}
		return q
	case GitHubComment:
		s, err := SettingsOf(n, DefaultGitHubComment())
		switch {
		case err != nil:
			return ""
		case s.Trial:
			return "Trial: writes what it would post"
		case s.KeepOne:
			return "Posts, keeping one comment"
		}
		return "Posts a comment"
	case DiscordPost:
		if s, err := SettingsOf(n, DefaultDiscordPost()); err == nil && s.Summary {
			return "Sets the case's summary"
		}
		return "Posts in the case's thread"
	case Approval:
		s, err := SettingsOf(n, DefaultApproval(kind))
		if err != nil {
			return ""
		}
		return fmt.Sprintf("Waits up to %d hours", s.Hours)
	case Condition:
		s, err := SettingsOf(n, DefaultCondition())
		if err != nil || s.rules() == 0 {
			return "Turn on a rule"
		}
		if s.rules() == 1 {
			return "1 rule"
		}
		return fmt.Sprintf("%d rules, %s of them", s.rules(), s.Match)
	case TestRun:
		s, err := SettingsOf(n, DefaultTestRun())
		if err != nil || s.Machine == 0 {
			return "Choose a machine"
		}
		first, _, _ := strings.Cut(strings.TrimSpace(s.Script), "\n")
		if utf8.RuneCountInString(first) > 50 {
			first = string([]rune(first)[:49]) + "…"
		}
		return first
	case CaseEntry:
		s, err := CaseEntrySettingsOf(n)
		if err != nil {
			return ""
		}
		if s.Finalize {
			return "Appends a finalized entry"
		}
		return "Keeps a draft entry"
	}
	return ""
}

// Summary is the one line any node shows on the canvas; models names a model reference.
func Summary(n Node, kind string, models func(ModelRef) string) string {
	if t, _ := TypeOf(n.Type); t.Event {
		if s, err := EventSettingsOf(n); err == nil {
			return s.Summary()
		}
		return ""
	}
	return stepSummary(n, kind, models)
}
