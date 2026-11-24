package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
)

// More step blocks: a comment on GitHub, a route by simple facts, and a test on a machine.
const (
	GitHubComment = "github_comment"
	Condition     = "condition"
	TestRun       = "test_run"
)

// GitHubCommentSettings are a GitHub Comment node's settings. In a trial run nothing is posted:
// the comment it would have posted goes into the case file instead, to be read first.
type GitHubCommentSettings struct {
	Message string `json:"message"`  // a template
	Trial   bool   `json:"trial"`    // write what would be posted to the case file; post nothing
	KeepOne bool   `json:"keep_one"` // a later run edits this node's earlier comment instead of adding another
}

// DefaultGitHubComment is a new GitHub Comment node's settings: a trial, until someone decides.
func DefaultGitHubComment() GitHubCommentSettings {
	return GitHubCommentSettings{Message: "{input.report}", Trial: true, KeepOne: true}
}

// Check confirms a GitHub Comment node's settings are complete.
func (s GitHubCommentSettings) Check() error {
	switch {
	case strings.TrimSpace(s.Message) == "":
		return errors.New("write the comment")
	case len(s.Message) > maxTemplate:
		return fmt.Errorf("the comment is limited to %d characters", maxTemplate)
	}
	return nil
}

// GitHubCommentFromForm reads a GitHub Comment node's settings from its form.
func GitHubCommentFromForm(get func(string) string) GitHubCommentSettings {
	return GitHubCommentSettings{Message: strings.TrimSpace(get("message")), Trial: get("trial") == "on", KeepOne: get("keep_one") == "on"}
}

// ConditionSettings are a Condition node's settings: rules about the item, each on or off, and
// whether all of the ones on must hold or any. It routes without a model.
type ConditionSettings struct {
	Match string `json:"match"` // "all" or "any"

	Files     string `json:"files,omitempty"`      // a changed file matches this pattern (pull requests)
	MoreLines int    `json:"more_lines,omitempty"` // more changed lines than this (pull requests); 0: off
	Author    string `json:"author,omitempty"`     // "bot", "first_time", "outside" or "member"; "": off
	Label     string `json:"label,omitempty"`      // has this label
	Contains  string `json:"contains,omitempty"`   // the title or description contains this (any case)
	Tag       string `json:"tag,omitempty"`        // an output tag ...
	TagHas    string `json:"tag_has,omitempty"`    // ... contains this (any case)
}

// DefaultCondition is a new Condition node's settings.
func DefaultCondition() ConditionSettings { return ConditionSettings{Match: "all"} }

// rules counts the rules that are on.
func (s ConditionSettings) rules() int {
	n := 0
	for _, on := range []bool{s.Files != "", s.MoreLines > 0, s.Author != "", s.Label != "", s.Contains != "", s.Tag != ""} {
		if on {
			n++
		}
	}
	return n
}

// Check confirms a Condition node's settings are sensible.
func (s ConditionSettings) Check(kind string) error {
	switch {
	case s.Match != "all" && s.Match != "any":
		return errors.New("choose whether all rules must hold or any")
	case s.rules() == 0:
		return errors.New("turn on at least one rule")
	case s.MoreLines < 0:
		return errors.New("the number of lines cannot be negative")
	case kind == Issue && (s.Files != "" || s.MoreLines > 0):
		return errors.New("an issue changes no files: use the other rules")
	case s.Author != "" && s.Author != "bot" && s.Author != "first_time" && s.Author != "outside" && s.Author != "member":
		return errors.New("choose who the author is")
	case s.Tag != "" && !tagNameRE.MatchString(s.Tag):
		return errors.New("the tag is a lower-case word, such as value_decision")
	}
	if s.Files != "" {
		if _, err := path.Match(s.Files, "x"); err != nil {
			return fmt.Errorf("%q is not a file pattern", s.Files)
		}
	}
	return nil
}

// ConditionFromForm reads a Condition node's settings from its form: each rule's box, then its value.
func ConditionFromForm(get func(string) string) ConditionSettings {
	on := func(k string) bool { return get("on_"+k) == "on" }
	s := ConditionSettings{Match: get("match")}
	if on("files") {
		s.Files = strings.TrimSpace(get("files"))
	}
	if on("lines") {
		s.MoreLines, _ = strconv.Atoi(get("more_lines"))
	}
	if on("author") {
		s.Author = get("author")
	}
	if on("label") {
		s.Label = strings.TrimSpace(get("label"))
	}
	if on("contains") {
		s.Contains = strings.TrimSpace(get("contains"))
	}
	if on("tag") {
		s.Tag, s.TagHas = strings.Trim(strings.TrimSpace(get("tag")), "{}"), strings.TrimSpace(get("tag_has"))
	}
	return s
}

// ItemFacts are what a Condition knows of the item.
type ItemFacts struct {
	Files       []string // the changed files' paths
	Lines       int      // lines added and removed
	Author      string
	Association string // GitHub's word for the author: OWNER, MEMBER, COLLABORATOR, CONTRIBUTOR, FIRST_TIME_CONTRIBUTOR, FIRST_TIMER, NONE
	Labels      []string
	Title, Body string
	Tags        map[string]string
}

// Holds reports whether the condition holds for the item, with what each rule found.
func (s ConditionSettings) Holds(f ItemFacts) (bool, []string) {
	var results []bool
	var notes []string
	rule := func(ok bool, what string) {
		results = append(results, ok)
		mark := "✗"
		if ok {
			mark = "✓"
		}
		notes = append(notes, mark+" "+what)
	}
	if s.Files != "" {
		hit := ""
		for _, p := range f.Files {
			if matchPath(s.Files, p) {
				hit = p
				break
			}
		}
		rule(hit != "", "a changed file matches "+s.Files+map[bool]string{true: " (" + hit + ")", false: ""}[hit != ""])
	}
	if s.MoreLines > 0 {
		rule(f.Lines > s.MoreLines, fmt.Sprintf("%d changed lines, more than %d", f.Lines, s.MoreLines))
	}
	if s.Author != "" {
		bot := strings.HasSuffix(f.Author, "[bot]")
		var ok bool
		switch s.Author {
		case "bot":
			ok = bot
		case "first_time":
			ok = f.Association == "FIRST_TIME_CONTRIBUTOR" || f.Association == "FIRST_TIMER"
		case "outside":
			ok = !bot && f.Association != "OWNER" && f.Association != "MEMBER" && f.Association != "COLLABORATOR"
		case "member":
			ok = f.Association == "OWNER" || f.Association == "MEMBER" || f.Association == "COLLABORATOR"
		}
		rule(ok, "its author "+f.Author+" is "+map[string]string{"bot": "a bot", "first_time": "a first-time contributor",
			"outside": "from outside the project", "member": "a member of the project"}[s.Author])
	}
	if s.Label != "" {
		has := false
		for _, l := range f.Labels {
			has = has || strings.EqualFold(l, s.Label)
		}
		rule(has, "it has the label "+s.Label)
	}
	if s.Contains != "" {
		rule(strings.Contains(strings.ToLower(f.Title+"\n"+f.Body), strings.ToLower(s.Contains)), "its title or description contains “"+s.Contains+"”")
	}
	if s.Tag != "" {
		rule(strings.Contains(strings.ToLower(f.Tags[s.Tag]), strings.ToLower(s.TagHas)), "{"+s.Tag+"} contains “"+s.TagHas+"”")
	}
	all, any := true, false
	for _, r := range results {
		all, any = all && r, any || r
	}
	if s.Match == "any" {
		return any, notes
	}
	return all, notes
}

// matchPath matches a pattern against a whole path or, with no slash in it, against the file name;
// a pattern ending in /* covers everything under that folder.
func matchPath(pattern, p string) bool {
	if !strings.Contains(pattern, "/") {
		ok, _ := path.Match(pattern, path.Base(p))
		return ok
	}
	if ok, _ := path.Match(pattern, p); ok {
		return true
	}
	if dir, ok := strings.CutSuffix(pattern, "/*"); ok {
		return strings.HasPrefix(p, dir+"/")
	}
	return false
}

// TestRunSettings are a Test Run node's settings: a script on one of the workspace's machines,
// with the item's code (a pull request's head) in its folder. It passes when the script exits 0.
type TestRunSettings struct {
	Machine int64  `json:"machine"`
	Script  string `json:"script"` // a template; bash, with -e and -x
	GPUs    int    `json:"gpus"`
	Minutes int    `json:"minutes"`
}

// DefaultTestRun is a new Test Run node's settings.
func DefaultTestRun() TestRunSettings {
	return TestRunSettings{Script: "make test", Minutes: 30}
}

// Check confirms a Test Run node's settings are complete; the machine being the workspace's is the
// web layer's to check.
func (s TestRunSettings) Check() error {
	switch {
	case s.Machine == 0:
		return errors.New("choose a machine")
	case strings.TrimSpace(s.Script) == "":
		return errors.New("write the script")
	case len(s.Script) > 64<<10:
		return errors.New("the script is limited to 64 KB")
	case s.GPUs < 0 || s.GPUs > 8:
		return errors.New("GPUs are 0 to 8")
	case s.Minutes < 1 || s.Minutes > 120:
		return errors.New("the time limit is 1 to 120 minutes")
	}
	return nil
}

// TestRunFromForm reads a Test Run node's settings from its form.
func TestRunFromForm(get func(string) string) TestRunSettings {
	s := TestRunSettings{Script: strings.TrimSpace(get("script"))}
	s.Machine, _ = strconv.ParseInt(get("machine"), 10, 64)
	s.GPUs, _ = strconv.Atoi(get("gpus"))
	s.Minutes, _ = strconv.Atoi(get("minutes"))
	return s
}

// SettingsOf reads a node's settings into the type of def, or returns def when it has none yet.
func SettingsOf[T any](n Node, def T) (T, error) {
	if len(n.Settings) == 0 {
		return def, nil
	}
	var s T
	if err := json.Unmarshal(n.Settings, &s); err != nil {
		t, _ := TypeOf(n.Type)
		return s, fmt.Errorf("%s: unreadable settings", t.Label)
	}
	return s, nil
}

// Discord and people.
const (
	DiscordPost = "discord_post"
	Approval    = "approval"
)

// DiscordPostSettings are a Post to Discord node's settings: a message in the case's forum post, or
// the post's first message (its summary) replaced.
type DiscordPostSettings struct {
	Message string `json:"message"` // a template
	Summary bool   `json:"summary"` // replace the post's first message instead of adding one
}

// DefaultDiscordPost is a new Post to Discord node's settings.
func DefaultDiscordPost() DiscordPostSettings {
	return DiscordPostSettings{Message: "{input.report}"}
}

// Check confirms a Post to Discord node's settings are complete.
func (s DiscordPostSettings) Check() error {
	switch {
	case strings.TrimSpace(s.Message) == "":
		return errors.New("write the message")
	case len(s.Message) > maxTemplate:
		return fmt.Errorf("the message is limited to %d characters", maxTemplate)
	}
	return nil
}

// ApprovalSettings are a Human Approval node's settings: what is asked, and how long to wait.
type ApprovalSettings struct {
	Request string `json:"request"` // a template
	Hours   int    `json:"hours"`
}

// DefaultApproval is a new Human Approval node's settings, for a workflow of kind.
func DefaultApproval(kind string) ApprovalSettings {
	if kind == Issue {
		return ApprovalSettings{Request: "Go on with {issue}?", Hours: 24}
	}
	return ApprovalSettings{Request: "Run {pr} on the hardware?", Hours: 24}
}

// Check confirms a Human Approval node's settings are complete.
func (s ApprovalSettings) Check() error {
	switch {
	case strings.TrimSpace(s.Request) == "":
		return errors.New("write what is asked")
	case len(s.Request) > 4000:
		return errors.New("the request is limited to 4,000 characters")
	case s.Hours < 1 || s.Hours > 336:
		return errors.New("the time limit is 1 to 336 hours (two weeks)")
	}
	return nil
}
