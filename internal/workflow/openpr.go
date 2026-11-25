package workflow

import (
	"errors"
	"fmt"
	"strings"
)

// OpenPR pushes the patch captured from an earlier sandboxed Agent step onto a branch and opens a
// GitHub pull request (ready or draft). In Trial mode (the default) or a dry run it writes a
// preview into the case file and touches neither git nor GitHub.
const OpenPR = "open_pr"

// OpenPRSettings are an Open PR node's settings.
type OpenPRSettings struct {
	Title    string `json:"title"`
	Body     string `json:"body"`
	Branch   string `json:"branch"`
	Base     string `json:"base,omitempty"` // empty: repository's default branch
	Draft    bool   `json:"draft"`
	Trial    bool   `json:"trial"`
	OutputTo string `json:"output_to,omitempty"`
}

// DefaultOpenPR is a new Open PR node's settings, in trial mode until turned off.
func DefaultOpenPR(kind string) OpenPRSettings {
	if kind == PR {
		return OpenPRSettings{
			Title:  "Follow-up for #{pr:number}: {pr:title}",
			Body:   "Follow-up for #{pr:number}\n\n{input.summary}\n\n[[case|Case File]]",
			Branch: "autobot/pr-{pr:number}",
			Trial:  true,
		}
	}
	return OpenPRSettings{
		Title:  "{issue:title}",
		Body:   "Fixes #{issue:number}\n\n{input.summary}\n\n[[case|Case File]]",
		Branch: "autobot/issue-{issue:number}",
		Trial:  true,
	}
}

// Check confirms an Open PR node's settings are complete.
func (s OpenPRSettings) Check() error {
	switch {
	case strings.TrimSpace(s.Title) == "":
		return errors.New("write the pull request title")
	case len(s.Title) > 256:
		return errors.New("the pull request title is limited to 256 characters")
	case strings.TrimSpace(s.Body) == "":
		return errors.New("write the pull request description")
	case len(s.Body) > maxTemplate:
		return fmt.Errorf("the pull request description is limited to %d characters", maxTemplate)
	case strings.TrimSpace(s.Branch) == "":
		return errors.New("write the branch name")
	case len(s.Branch) > 120:
		return errors.New("the branch name is limited to 120 characters")
	}
	return nil
}

// OpenPRFromForm reads an Open PR node's settings from its form.
func OpenPRFromForm(get func(string) string) OpenPRSettings {
	return OpenPRSettings{
		Title:  strings.TrimSpace(get("title")),
		Body:   strings.TrimSpace(get("body")),
		Branch: strings.TrimSpace(get("branch")),
		Base:   strings.TrimSpace(get("base")),
		Draft:  get("draft") == "on",
		Trial:  get("trial") == "on",
	}
}

// Summary is the short line shown on the canvas card.
func (s OpenPRSettings) Summary() string {
	kind := "Ready PR"
	if s.Draft {
		kind = "Draft PR"
	}
	if s.Trial {
		kind += " (trial)"
	}
	return kind
}

// SanitizeBranch turns a rendered branch template into a safe git branch name.
func SanitizeBranch(raw string) string {
	s := strings.TrimSpace(raw)
	var b strings.Builder
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '/' || r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
	}
	out := b.String()
	for strings.Contains(out, "..") {
		out = strings.ReplaceAll(out, "..", "-")
	}
	for strings.Contains(out, "//") {
		out = strings.ReplaceAll(out, "//", "/")
	}
	out = strings.Trim(out, "/.-")
	out = strings.TrimSuffix(out, ".lock")
	if out == "" {
		return "autobot/patch"
	}
	return out
}
