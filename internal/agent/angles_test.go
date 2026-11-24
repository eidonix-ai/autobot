package agent

import (
	"strings"
	"testing"
)

// The writing session is told to organize the report by the angles the instruction names, with
// "Nothing found" for an angle without notes, and to give the summary a line or two per angle, still
// within about 300 words; the writing task carries it after the instruction.
func TestWriterPromptOrganizesByAngles(t *testing.T) {
	for _, want := range []string{
		"organized by the angles the instruction names, if it names them",
		"after Key findings, one section per angle, in the instruction's order",
		"Group your notes by the angle at the start of their titles",
		"\"Nothing found\" under an angle that has no notes rather than leaving it out",
		"The summary then gives each angle one or two lines, with links, where there is something to say, after the opening line and the findings that matter most, and still stays within about 300 words",
		"Key findings", // what was there before stays
		"what a reader could not learn from the description",
	} {
		if !strings.Contains(writerPrompt, want) {
			t.Errorf("the writer prompt lacks %q", want)
		}
	}
	task := WriterTask("INSTRUCTION AND DONE WHEN", "", nil, nil)
	if !strings.HasPrefix(task, "INSTRUCTION AND DONE WHEN\n\n") || !strings.Contains(task, "Nothing found") {
		t.Errorf("the writing task: %.300s", task)
	}
}

// A summary format reaches the writing session exactly, after the sentence that says to follow it
// and keep the labels; the report's wording stays; the default 300-word summary is gone with it.
// Without one the prompt is the default.
func TestWriterPromptWithASummaryFormat(t *testing.T) {
	const format = "**Author:** <login>\n**Scale:** +N / −N lines in N files\n**Autobot comments:** <anything else. \"None.\" if nothing.>"
	got := writerPromptFor("\n  " + format + "\n")
	for _, want := range []string{
		"The summary follows this format exactly, filling every field; keep the labels; write 'None.' where a field allows it and there is nothing:\n" + format + "\n",
		"Open the report with a short \"Key findings\" section.",
		"The report is organized by the angles the instruction names, if it names them",
		"\"Nothing found\" under an angle that has no notes",
		"what a reader could not learn from the description",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the prompt with a format lacks %q:\n%s", want, got)
		}
	}
	for _, not := range []string{"at most about 300 words", "one or two lines, with links", "no inventory of files"} {
		if strings.Contains(got, not) {
			t.Errorf("the prompt with a format still says %q", not)
		}
	}
	if def := writerPromptFor("  \n"); def != writerPrompt || !strings.Contains(def, "The summary is at most about 300 words") || strings.Contains(def, "follows this format") {
		t.Errorf("a blank format is not the default:\n%s", def)
	}
	if task := WriterTask("INTRO", format, nil, nil); !strings.Contains(task, "\n"+format+"\n") || !strings.HasPrefix(task, "INTRO\n\n") {
		t.Errorf("the writing task lacks the format:\n%s", task)
	}
}
