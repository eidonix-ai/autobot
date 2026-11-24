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

	// writerPrompt follows the instruction and Done when in the writing session's first message.
	writerPrompt = "Your research is done and its notes are in your scratchpad. Write the final report now, from your notes: " +
		"organized and detailed, the high-importance findings first, every claim with its citation (file and lines, pull request " +
		"or issue numbers, links), then the open questions and what could not be found. Open the report with a short \"Key findings\" section. " +
		"Read the index, then the entries you need; check a citation in the code when you are unsure. " +
		"Call finish with two parts. The summary is at most about 300 words: the 5 to 8 findings that matter most, one line each with its " +
		"citation, then the most important open questions; no inventory of files. The report is the full report."

	saveTurnTime = 3 * time.Minute // the save turn's own time, whatever the research had left

	researchSummary = "Research summary"
)

// Writer is the session that writes a run's report from the scratchpad.
type Writer struct {
	// Config is its run: Model, System, Tools (read-only), MaxTurns, RunID, Pad and Window. Its Task
	// is made here, and its saving is the research's.
	Config Config
	// Conv is its new conversation, from NewWriterConversation.
	Conv *Conversation
	// Intro is the block's rendered Instruction and Done when.
	Intro string
	// Time is its own time, and WarnPercent how much of it is left when it is warned (0: not).
	Time        time.Duration
	WarnPercent int
}

// WriterTask is the writing session's first message: the block's instruction and Done when, what
// to do, and the scratchpad index.
func WriterTask(intro string, pad Pad, subs SubView) string {
	task := intro + "\n\n" + writerPrompt + "\n\nYour notes so far:\n" + Index(pad, "")
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
	if tr.Outcome != Failed {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), saveTurnTime)
		SaveTurn(sctx, research, conv, tr)
		cancel()
		if wt := writeReport(ctx, research, tr, w); strings.TrimSpace(wt.Report) != "" {
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
	cfg.Task = WriterTask(w.Intro, research.Pad, research.Subs)
	cfg.Verify = nil
	cfg.Save = func(wt *Transcript) {
		tr.Writing = wt
		if research.Save != nil {
			research.Save(tr)
		}
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.Time)
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
