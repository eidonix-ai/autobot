package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/eidonix-ai/autobot/internal/store"
)

// SubAgents are the sub-agents of one run. The main agent starts one with spawn_agent, which waits until
// it has finished and returns its report: a fresh session on the same harness (the same model, turn
// budget, warnings, numbered results, pruning, compacting and window), inside the main agent's context and
// deadline, with the read-only tools, a private scratchpad, and no shell and no sub-agents of its own.
// Several calls in one turn run at the same time, since the harness runs a turn's calls together.
type SubAgents struct {
	St        *store.Store
	Run       int64 // the run they belong to; set once it has started
	Model     func(session string) Model
	ModelName string
	Provider  int64
	System    string // their preamble
	Tools     []Tool // what they may use: read-only
	MaxTurns  int
	Window    int
	AtOnce    int // how many may work at the same time
	PerRun    int // how many may be started in all; 0 for no limit

	mu               sync.Mutex
	running, started int
	turns            int
	reads            map[string][]string // by the arguments of the call that started them
}

// Tool is spawn_agent.
func (sa *SubAgents) Tool() Tool {
	limit := ""
	if sa.PerRun > 0 {
		limit = fmt.Sprintf(" and %d in this run", sa.PerRun)
	}
	return Tool{
		Spec: ToolSpec{Name: "spawn_agent", Description: fmt.Sprintf("Hand a separate piece of work to a sub-agent: a fresh session like yours, with read-only tools and a scratchpad of its own, "+
			"that works on the task and brings back its report. You may have up to %d sub-agents working at once%s. Each has up to %d turns of its own, so start one only for a "+
			"substantial piece of work that can be done separately. The call waits until the sub-agent has finished and returns its report and an index of its notes "+
			"(read them with scratchpad_read, agent \"s1\" and so on); several calls in one turn run at the same time.", sa.AtOnce, limit, sa.MaxTurns),
			Parameters: json.RawMessage(`{"type":"object","properties":{
				"title":{"type":"string","description":"A short name for the sub-agent's work."},
				"task":{"type":"string","description":"What to find out and bring back, with what it needs to know."}},"required":["title","task"]}`)},
		Run: sa.spawn,
		Reads: func(args json.RawMessage) []string {
			sa.mu.Lock()
			defer sa.mu.Unlock()
			return sa.reads[string(args)]
		},
	}
}

// totals is the line that ends every answer: what the run has used of its sub-agents.
func (sa *SubAgents) totals() string {
	if sa.PerRun > 0 {
		return fmt.Sprintf("Sub-agents so far: %d of %d started, %d turns used.", sa.started, sa.PerRun, sa.turns)
	}
	return fmt.Sprintf("Sub-agents so far: %d started, %d turns used.", sa.started, sa.turns)
}

func (sa *SubAgents) refuse(msg string) (string, error) {
	sa.mu.Lock()
	defer sa.mu.Unlock()
	return "error: " + msg + "\n\n" + sa.totals(), nil
}

func (sa *SubAgents) spawn(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct{ Title, Task string }
	if json.Unmarshal(raw, &a) != nil || strings.TrimSpace(a.Task) == "" {
		return sa.refuse(`spawn_agent takes {"title": a short name, "task": what to find out and bring back}`)
	}
	a.Title = strings.Join(strings.Fields(a.Title), " ")
	if a.Title == "" {
		a.Title = firstLine(a.Task, 60)
	}
	sa.mu.Lock()
	switch {
	case sa.running >= sa.AtOnce:
		sa.mu.Unlock()
		return sa.refuse(fmt.Sprintf("%d sub-agents are already working, the most you may have at once; wait for one to finish, then ask again", sa.running))
	case sa.PerRun > 0 && sa.started >= sa.PerRun:
		sa.mu.Unlock()
		return sa.refuse(fmt.Sprintf("this run's total of %d sub-agents is reached", sa.PerRun))
	}
	sa.running++
	sa.started++
	name := fmt.Sprintf("s%d", sa.started)
	sa.mu.Unlock()

	tr, pad, err := sa.work(ctx, name, a.Title, a.Task)

	sa.mu.Lock()
	defer sa.mu.Unlock()
	sa.running--
	if err != nil {
		return "error: " + err.Error() + "\n\n" + sa.totals(), nil
	}
	sa.turns += tr.Turns
	if sa.reads == nil {
		sa.reads = map[string][]string{}
	}
	sa.reads[string(raw)] = append(sa.reads[string(raw)], tr.Read...)
	notes, _ := pad.List()
	return fmt.Sprintf("Sub-agent %s %q: %s, %d turns.\n\nReport:\n%s\n\nIts notes (read any with scratchpad_read, agent %q):\n%s\n\n%s",
		name, a.Title, outcomeText(tr), tr.Turns, tr.Report, name, IndexOf(notes, ""), sa.totals()), nil
}

// outcomeText says how a sub-agent ended.
func outcomeText(tr *Transcript) string {
	switch tr.Outcome {
	case Met, Blocked:
		return "finished (" + tr.Outcome + ")"
	case Budget:
		return "stopped without finishing (out of turns or time)"
	case Unfinished:
		return "ended without finishing"
	}
	return "failed (" + tr.Error + ")"
}

// work runs one sub-agent to its end and returns its transcript and its notes.
func (sa *SubAgents) work(ctx context.Context, name, title, task string) (*Transcript, Pad, error) {
	row, err := sa.St.StartSub(sa.Run, name, title, task)
	if err != nil {
		return nil, nil, fmt.Errorf("could not start the sub-agent: %v", err)
	}
	conv := NewConversation(sa.ModelName, sa.Provider, sa.System, sa.Tools)
	pad := sa.St.SubPad(row.ID, name)
	save := func(tr *Transcript, status string) {
		conv.Write(func(evs ...store.ConvEvent) error { return sa.St.AppendConv(row.Conversation, evs...) })
		b, _ := json.Marshal(tr)
		sa.St.SaveSub(row.ID, status, string(b))
	}
	tr := Run(ctx, Config{
		Model: sa.Model(conv.Session), Task: fmt.Sprintf("You are sub-agent %s, working for a main agent. Its task for you, %q:\n\n%s\n\n"+
			"Find it out, saving what you find to your own scratchpad as you go, then call finish with your report: what you found, with its citations "+
			"(file and lines, pull request or issue numbers, links), and what you could not find.", name, title, task),
		Tools: sa.Tools, MaxTurns: sa.MaxTurns, Window: sa.Window, Pad: pad, RunID: sa.Run,
		Save: func(tr *Transcript) { save(tr, "running") },
	}, conv)
	if strings.TrimSpace(tr.Report) == "" { // it ended without finishing and said nothing: its notes are its report
		if tr.Report = NotesReport(pad); tr.Report == "" {
			tr.Report = "(no report, and no notes)"
		}
	}
	save(tr, tr.Outcome)
	return tr, pad, nil
}

// Notes are the notes of one of the run's sub-agents, by name.
func (sa *SubAgents) Notes(name string) ([]store.PadEntry, error) {
	return sa.St.SubNotes(sa.Run, name)
}

// Summaries are the run's sub-agents so far.
func (sa *SubAgents) Summaries() []SubInfo {
	rows, err := sa.St.Subs(sa.Run)
	if err != nil {
		return nil
	}
	var out []SubInfo
	for _, r := range rows {
		var tr Transcript
		json.Unmarshal([]byte(r.Summary), &tr)
		outcome := r.Status
		if r.Status == "running" {
			outcome = "running"
		}
		out = append(out, SubInfo{Name: r.Name, Title: r.Title, Outcome: outcome, Report: tr.Report})
	}
	return out
}
