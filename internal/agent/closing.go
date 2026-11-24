package agent

import (
	"context"
	"strings"
	"time"

	"github.com/eidonix-ai/autobot/internal/store"
)

// The closing step "report": research is one session, whose notes go to the scratchpad, and the
// final report is written from those notes in a fresh session, so that it is not limited by what
// is still in the research context.

const (
	// saveTurnPrompt is the one more turn the research session gets once its work is over.
	saveTurnPrompt = "Before your report is written: save to your scratchpad anything the report needs that is not there yet. Only scratchpad_write is available on this turn."

	// The writing session's prompt: universal across all presets with a closing step. Any
	// task-specific organization (such as Gather Information's angles or a review's lenses) lives in
	// the preset's own instruction, not hardcoded here.
	writerLead = "Your research is done and its notes are in your scratchpad. Write the final report now from those notes: " +
		"organized and detailed, high-importance findings first, every claim with its citation (file and lines, pull request " +
		"or issue numbers, links), then open questions and what could not be found. Open the report with a short \"Key findings\" section, " +
		"and follow any organization or grouping specified in the original task above. " +
		"Read the scratchpad index, then the entries you need; verify a citation in the code if unsure. " +
		"Call finish with two parts. "
	defaultSummary = "The summary is at most about 300 words: one opening line, then the 5 to 8 findings that matter most " +
		"(one line each with its citation), then the most important open questions; no inventory of files. "
	formatSummary = "The summary follows this format exactly, filling every field; keep the labels; write 'None.' where a field allows it and there is nothing:\n"
	reportIsFull  = "The report is the full report."

	saveTurnTime = 3 * time.Minute // the save turn's own time, whatever the research had left

	researchSummary = "Research summary"
)

// writerPrompt is the writing session's prompt for a block with no summary format.
var writerPrompt = writerPromptFor("")

// writerPromptFor is the prompt that follows the original task in the writing session's first message.
// With a summary format the summary follows it exactly; without one it is a short summary of about
// 300 words.
func writerPromptFor(format string) string {
	if strings.TrimSpace(format) == "" {
		return writerLead + defaultSummary + reportIsFull
	}
	return writerLead + formatSummary + strings.TrimSpace(format) + "\n" + reportIsFull
}

// Writer is the session that writes a run's report from the scratchpad.
type Writer struct {
	// Config is its run: Model, System, Tools (read-only), MaxTurns, RunID, Pad and Window. Its Task
	// is made here, and its saving is the research's.
	Config Config
	// Conv is its new conversation, from NewWriterConversation.
	Conv *Conversation
	// Intro is the block's rendered Instruction and Done when.
	Intro string
	// SummaryFormat is the block's rendered summary format: the summary follows it exactly. "" for the default.
	SummaryFormat string
	// Time is its own time, and WarnPercent how much of it is left when it is warned (0: not).
	Time        time.Duration
	WarnPercent int
}

// WriterTask is the writing session's first message: the original task framed as background context,
// the writing instructions, and the scratchpad index (plus sub-agent summaries if any).
func WriterTask(intro, summaryFormat string, pad Pad, subs SubView) string {
	task := "## Original task (for context only — research is complete; do not re-run research or spawn sub-agents)\n\n" +
		intro + "\n\n## Your job now\n\n" + writerPromptFor(summaryFormat) + "\n\nYour notes so far:\n" + Index(pad, "")
	if subs != nil {
		if infos := subs.Summaries(); len(infos) > 0 {
			task += "\n\nYour sub-agents' reports (read their notes with scratchpad_index, scratchpad_read and scratchpad_search, agent \"s1\" and so on):\n" + subLines(infos)
		}
	}
	return task
}

// SaveTurn gives a research session one more turn, in which it can only write to its scratchpad.
// Its cost is added to tr. It does nothing when the model cannot be reached.
func SaveTurn(ctx context.Context, cfg Config, conv *Conversation, tr *Transcript) {
	s := &session{cfg: cfg, conv: conv, tr: tr, tools: map[string]Tool{}}
	conv.add(Message{Role: "user", Content: saveTurnPrompt})
	reply, use, err := complete(ctx, cfg.Model, conv.request(), conv.Tools) // the tools array stays as it was
	if err != nil {
		return
	}
	tr.Turns++
	tr.In += use.In
	tr.Out += use.Out
	tr.Cached += use.Cached
	conv.sent(use.In)
	conv.add(reply)
	conv.recordTurn(cfg.RunID, use)
	for _, call := range reply.ToolCalls {
		out := "error: only " + availableOn([]string{"scratchpad_write"})
		if call.Function.Name == "scratchpad_write" {
			out = s.builtin("scratchpad_write", call.Function.Arguments, nil)
		}
		conv.addResult(call.ID, out)
	}
	if cfg.Save != nil {
		cfg.Save(tr)
	}
}

// RunClosing runs a research session, then writes its report from the scratchpad:
//   - the research's own report is saved to the scratchpad as a high-importance note, so that
//     nothing is lost;
//   - the research gets one more turn to save what the report needs;
//   - a new session, with only read-only tools, writes the report from the notes.
//
// The transcript returned has the research's outcome and the writing session's transcript in
// Writing; its Report and Summary are the written ones, or, when none was written, the research's
// own, or the scratchpad's high and medium notes in order. A report with no summary gets one made
// from it. A research that failed with an error gets neither the save turn nor a
// writing session.
func RunClosing(ctx context.Context, research Config, conv *Conversation, w Writer) *Transcript {
	tr := Run(ctx, research, conv)
	if research.Pad == nil {
		return tr
	}
	if strings.TrimSpace(tr.Report) != "" {
		research.Pad.Add(store.PadEntry{Author: "main", Title: researchSummary, Importance: store.High, Description: firstLine(tr.Report, 160), Body: tr.Report})
	}
	if tr.Outcome != Failed && tr.Outcome != Stopped {
		sctx, cancel := detached(ctx, saveTurnTime)
		SaveTurn(sctx, research, conv, tr)
		cancel()
		if IsStopped(ctx) { // stopped during the save turn: no report is written
			tr.Outcome, tr.Error = Stopped, context.Cause(ctx).Error()
		} else if wt := writeReport(ctx, research, tr, w); strings.TrimSpace(wt.Report) != "" {
			tr.Report, tr.Summary = wt.Report, wt.Summary // else the research's own report, and its summary, stay
		}
	}
	if strings.TrimSpace(tr.Report) == "" {
		tr.Report, tr.Summary = NotesReport(research.Pad), ""
	}
	tr.EnsureSummary()
	if research.Save != nil {
		research.Save(tr)
	}
	return tr
}

// writeReport runs the writing session and returns its transcript, whose Report and Summary are
// empty when it wrote none.
func writeReport(ctx context.Context, research Config, tr *Transcript, w Writer) *Transcript {
	cfg := w.Config
	cfg.WithSummary = true                                             // its finish takes a summary as well as the report
	cfg.Pad, cfg.Subs, cfg.Read = research.Pad, research.Subs, tr.Read // it may cite what the research read, and read its sub-agents' notes
	cfg.Task = WriterTask(w.Intro, w.SummaryFormat, research.Pad, research.Subs)
	cfg.Verify = nil
	cfg.Save = func(wt *Transcript) {
		tr.Writing = wt
		if research.Save != nil {
			research.Save(tr)
		}
	}
	wctx, cancel := detached(ctx, w.Time)
	defer cancel()
	cfg.Now = research.Now
	cfg.Deadline = time.Now().Add(w.Time)
	if cfg.Now != nil {
		cfg.Deadline = cfg.Now().Add(w.Time)
	}
	cfg.WarnAt = WarnTime(cfg.Deadline, w.Time, w.WarnPercent)
	wt := Run(wctx, cfg, w.Conv)
	tr.Writing = wt
	return wt
}

// NotesReport is a report made of the scratchpad's high and medium notes in order: what a run that
// took notes ends with when no report was written.
func NotesReport(pad Pad) string {
	all, err := pad.List()
	if err != nil {
		return ""
	}
	return store.NotesText(all)
}
