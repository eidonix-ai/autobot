package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
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
	return CaseEntrySettings{Entry: "{previous_output}", Finalize: true}
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

// Tools an agent may be given.
var AgentTools = []struct{ Key, Label string }{
	{"web_search", "Web search"},
	{"fetch_url", "Fetch web pages"},
	{"read_repo", "Read the repository"},
}

// AgentSettings are an agent node's settings.
type AgentSettings struct {
	Model        ModelRef `json:"model"`
	SystemPrompt string   `json:"system_prompt"` // a template
	Instruction  string   `json:"instruction"`   // a template: the task, sent as the first message
	Tools        []string `json:"tools"`
	MaxTurns     int      `json:"max_turns"`   // model calls before it must answer
	MaxMinutes   int      `json:"max_minutes"` // wall time before it is stopped
	Rerun        string   `json:"rerun"`       // run again on the same item: "resume" its conversation, or start "fresh"
	FollowUp     string   `json:"follow_up"`   // resume: a template sent as the next message; "" sends the instruction again
	OutputTo     string   `json:"output_to,omitempty"`
}

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
	s := AgentSettings{
		SystemPrompt: "You are Autobot, a careful reviewer for {repo}. The project's charter:\n\n{project_charter}",
		Tools:        []string{"web_search", "fetch_url", "read_repo"},
		MaxTurns:     30, MaxMinutes: 15, Rerun: "resume",
	}
	if kind == Issue {
		s.Instruction = "Look into this issue: {issue_link}\n\n{issue_title}\n\n{issue_body}"
	} else {
		s.Instruction = "Review this pull request: {pr_link}\n\n{pr_title}\n\n{pr_body}\n\n{pr_diff}"
	}
	return s
}

// DefaultDecision is a new decision node's settings, for a workflow of kind.
func DefaultDecision(kind string) DecisionSettings {
	s := DecisionSettings{Output: "decision", Choices: []Choice{
		{"yes", "It should."}, {"no", "It should not."}}}
	if kind == Issue {
		s.Context, s.Question = "{issue_title}\n\n{issue_body}", "Is this issue a bug report?"
	} else {
		s.Context, s.Question = "{pr_title}\n\n{pr_body}\n\n{pr_diff}", "Does this pull request need an in-depth review?"
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
	case !s.Model.Set():
		return errors.New("choose a model")
	case strings.TrimSpace(s.Instruction) == "":
		return errors.New("write the instruction")
	case len(s.Instruction) > maxTemplate || len(s.SystemPrompt) > maxTemplate || len(s.FollowUp) > maxTemplate:
		return fmt.Errorf("prompts are limited to %d characters", maxTemplate)
	case s.MaxTurns < 1 || s.MaxTurns > 200:
		return errors.New("the turn limit is 1 to 200")
	case s.MaxMinutes < 1 || s.MaxMinutes > 120:
		return errors.New("the time limit is 1 to 120 minutes")
	case s.Rerun != "resume" && s.Rerun != "fresh":
		return errors.New("choose what a re-run does")
	}
	for _, t := range s.Tools {
		if !knownTool(t) {
			return fmt.Errorf("no tool %q", t)
		}
	}
	return checkOutputTo(s.OutputTo, kind)
}

func knownTool(key string) bool {
	for _, t := range AgentTools {
		if t.Key == key {
			return true
		}
	}
	return false
}

// HasTool reports whether the agent has a tool, for the settings form.
func (s AgentSettings) HasTool(key string) bool { return has(s.Tools, key) }

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
		SystemPrompt: strings.TrimSpace(get("system_prompt")),
		Instruction:  strings.TrimSpace(get("instruction")),
		Tools:        all("tool"),
		Rerun:        get("rerun"),
		FollowUp:     strings.TrimSpace(get("follow_up")),
		OutputTo:     strings.TrimSpace(get("output_to")),
	}
	if s.Tools == nil {
		s.Tools = []string{}
	}
	s.MaxTurns, _ = strconv.Atoi(get("max_turns"))
	s.MaxMinutes, _ = strconv.Atoi(get("max_minutes"))
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
type Tag struct{ Name, About string }

var commonTags = []Tag{
	{"repo", "the repository, owner/name"},
	{"project_charter", "the project charter"},
	{"case_file", "the case file so far"},
	{"case_link", "the case file's address, for contributors' agents"},
	{"previous_output", "the output of the step this one follows"},
}

var prTags = []Tag{
	{"pr_link", "the pull request's address"},
	{"pr_number", "its number"},
	{"pr_title", "its title"},
	{"pr_body", "its description"},
	{"pr_author", "its author"},
	{"pr_diff", "its diff"},
	{"pr_files", "the files it changes"},
}

var issueTags = []Tag{
	{"issue_link", "the issue's address"},
	{"issue_number", "its number"},
	{"issue_title", "its title"},
	{"issue_body", "its description"},
	{"issue_author", "its author"},
}

// Tags lists the built-in tags of a workflow kind.
func Tags(kind string) []Tag {
	if kind == Issue {
		return append(append([]Tag{}, issueTags...), commonTags...)
	}
	return append(append([]Tag{}, prTags...), commonTags...)
}

func builtIn(name, kind string) bool {
	for _, t := range Tags(kind) {
		if t.Name == name {
			return true
		}
	}
	return false
}

// tagRE finds the tags of a template: a word in braces. Doubled braces are literal ({{word}} is the
// text {word}), and braces around anything but a word, as in JSON, are just text.
var tagRE = regexp.MustCompile(`\{\{|\}\}|\{([a-z][a-z0-9_]*)\}`)

// TemplateTags lists the tags a template uses, in order, each once.
func TemplateTags(text string) []string {
	var out []string
	for _, m := range tagRE.FindAllStringSubmatch(text, -1) {
		if m[1] != "" && !has(out, m[1]) {
			out = append(out, m[1])
		}
	}
	return out
}

// Render fills a template's tags from values; a tag with no value becomes empty text.
func Render(text string, values map[string]string) string {
	return tagRE.ReplaceAllStringFunc(text, func(m string) string {
		switch m {
		case "{{":
			return "{"
		case "}}":
			return "}"
		}
		return values[m[1:len(m)-1]]
	})
}

// OutputTags lists the output tags set by the workflow's nodes other than except.
func (g Graph) OutputTags(kind, except string) map[string]bool {
	out := map[string]bool{}
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
				out[s.OutputTo] = true
			}
		}
	}
	return out
}

// CheckTemplates confirms that every tag the templates use is built in or set by another node of
// the workflow (outputs). It is checked when a node's settings are saved, not on every save of the
// canvas, so removing a node is never refused because another uses its output.
func CheckTemplates(kind string, outputs map[string]bool, templates ...string) error {
	for _, t := range templates {
		for _, tag := range TemplateTags(t) {
			if !builtIn(tag, kind) && !outputs[tag] {
				return fmt.Errorf("{%s} is not a tag here: use a built-in tag or another step's output tag, or write {{%s}} for the text itself", tag, tag)
			}
		}
	}
	return nil
}

// stepSummary is the one line a step node shows on the canvas. models names a model reference.
func stepSummary(n Node, kind string, models func(ModelRef) string) string {
	switch n.Type {
	case Agent:
		s, err := AgentSettingsOf(n, kind)
		if err != nil || !s.Model.Set() {
			return "Choose a model"
		}
		parts := []string{models(s.Model)}
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
