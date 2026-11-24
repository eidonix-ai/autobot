package agent

import (
	"context"
	"strings"
	"testing"
)

// The writing session's prompt is universal: it frames the original task as completed background
// context, asks for high-importance findings first with citations and a Key findings section, and
// does not leak Gather-Information-specific angle rules into other presets.
func TestWriterPromptIsUniversalAndClear(t *testing.T) {
	for _, want := range []string{
		"Your research is done and its notes are in your scratchpad.",
		"Key findings",
		"follow any organization or grouping specified in the original task above",
		"at most about 300 words",
	} {
		if !strings.Contains(writerPrompt, want) {
			t.Errorf("the writer prompt lacks %q", want)
		}
	}
	for _, leaked := range []string{"by the angles", "what a reader could not learn from the description"} {
		if strings.Contains(writerPrompt, leaked) {
			t.Errorf("the generic writer prompt leaks %q", leaked)
		}
	}
	task := WriterTask("INSTRUCTION AND DONE WHEN", "", nil, nil)
	if !strings.Contains(task, "Original task (for context only") || !strings.Contains(task, "INSTRUCTION AND DONE WHEN") || !strings.Contains(task, "## Your job now") {
		t.Errorf("the writing task: %.400s", task)
	}
}

// A summary format reaches the writing session verbatim, replacing the default 300-word summary
// instruction; a blank format falls back to the default.
func TestWriterPromptWithASummaryFormat(t *testing.T) {
	const format = "**Author:** <login>\n**Scale:** +N / −N lines in N files\n**Autobot comments:** <anything else. \"None.\" if nothing.>"
	got := writerPromptFor("\n  " + format + "\n")
	for _, want := range []string{
		"The summary follows this format exactly, filling every field; keep the labels; write 'None.' where a field allows it and there is nothing:\n" + format + "\n",
		"Open the report with a short \"Key findings\" section",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the prompt with a format lacks %q:\n%s", want, got)
		}
	}
	for _, not := range []string{"at most about 300 words", "no inventory of files"} {
		if strings.Contains(got, not) {
			t.Errorf("the prompt with a format still says %q", not)
		}
	}
	if def := writerPromptFor("  \n"); def != writerPrompt || !strings.Contains(def, "The summary is at most about 300 words") || strings.Contains(def, "follows this format") {
		t.Errorf("a blank format is not the default:\n%s", def)
	}
	if task := WriterTask("INTRO", format, nil, nil); !strings.Contains(task, "\n"+format+"\n") || !strings.Contains(task, "INTRO") {
		t.Errorf("the writing task lacks the format:\n%s", task)
	}
}

// Filenames inside http(s):// URLs are not marked "(not opened)", while real file citations still are.
func TestAnnotateSkipsURLs(t *testing.T) {
	tr := &Transcript{Read: []string{"a.go"}}
	in := "See https://github.com/o/r/blob/main/README.md and [doc](https://example.com/guide.json) plus a.go:1 and b.go:2."
	want := "See https://github.com/o/r/blob/main/README.md and [doc](https://example.com/guide.json) plus a.go:1 and b.go:2 (not opened)."
	if got := tr.annotate(in); got != want {
		t.Fatalf("\ngot:  %q\nwant: %q", got, want)
	}
}

// NewPlainConversation offers only the finish tool (for text-only presets such as Editor).
func TestPlainConversationHasFinishOnly(t *testing.T) {
	c := NewPlainConversation("m", 1, "plain sys")
	if len(c.Tools) != 1 || c.Tools[0].Name != "finish" {
		t.Fatalf("plain tools: %+v", c.Tools)
	}
	m := &script{replies: []Message{finishCall("polished text")}}
	tr := Run(context.Background(), Config{Model: m, MaxTurns: 3, Task: "edit this", NoCitationMarks: true}, c)
	if tr.Outcome != Met || tr.Report != "polished text" {
		t.Fatalf("%+v", tr)
	}
}
