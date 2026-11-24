package store

import (
	"regexp"
	"strings"
	"unicode"
)

// A run's report comes with a summary of about SummaryWords words, which is what a continuation
// opens with and what a step's output tag holds. A report written without one gets the summary made
// here, from the report itself, and marked as made.

// SummaryWords is the size of a summary, and of one made from a report.
const SummaryWords = 300

var keyFindingsRE = regexp.MustCompile(`(?i)^\s{0,3}(#{1,6})\s*\**\s*key findings\b`)
var headingRE = regexp.MustCompile(`^\s{0,3}(#{1,6})\s`)

// KeyFindings is the report's "Key findings" section, without its heading, up to the next heading
// of the same or a higher level; ok is false when the report has none or it is empty.
func KeyFindings(report string) (section string, ok bool) {
	var lines []string
	level := 0
	for _, line := range strings.Split(report, "\n") {
		if level == 0 {
			if m := keyFindingsRE.FindStringSubmatch(line); m != nil {
				level = len(m[1])
			}
			continue
		}
		if m := headingRE.FindStringSubmatch(line); m != nil && len(m[1]) <= level {
			break
		}
		lines = append(lines, line)
	}
	section = strings.TrimSpace(strings.Join(lines, "\n"))
	return section, section != ""
}

// FirstWords is the start of a text, at most n words, ending where the last word ends; cut says
// whether anything was left out.
func FirstWords(text string, n int) (head string, cut bool) {
	words, inWord := 0, false
	for i, r := range text {
		if unicode.IsSpace(r) {
			inWord = false
			continue
		}
		if !inWord {
			if words == n {
				return strings.TrimRightFunc(text[:i], unicode.IsSpace), true
			}
			words++
			inWord = true
		}
	}
	return strings.TrimSpace(text), false
}

// FallbackSummary is the summary of a report whose writer gave none: the report's Key findings
// section if it has one, else its first SummaryWords words (all of it when it is shorter). Each is
// cut to that length, and says it was not written as a summary.
func FallbackSummary(report string) string {
	report = strings.TrimSpace(report)
	if report == "" {
		return ""
	}
	text, note := report, ""
	if kf, ok := KeyFindings(report); ok {
		text, note = kf, "(No summary was written: this is the report's Key findings section.)"
	}
	head, cut := FirstWords(text, SummaryWords)
	if cut && note == "" {
		note = "(No summary was written: this is the start of the report.)"
	}
	if note == "" {
		return head // a short report is its own summary
	}
	return head + "\n\n" + note
}
