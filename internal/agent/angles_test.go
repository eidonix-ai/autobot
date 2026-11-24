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
	task := WriterTask("INSTRUCTION AND DONE WHEN", nil, nil)
	if !strings.HasPrefix(task, "INSTRUCTION AND DONE WHEN\n\n") || !strings.Contains(task, "Nothing found") {
		t.Errorf("the writing task: %.300s", task)
	}
}
