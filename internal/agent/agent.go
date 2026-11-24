// Package agent is Autobot's harness: a model working toward a goal over many turns with tools,
// until it calls finish. It stops only through finish (or its budget): a reply without a tool call
// is answered with a nudge to keep going, and an optional check (a decision model) can send a "met"
// finish back to work. Tool calls of one turn run in parallel. Nothing here runs commands: tools
// that touch code or machines are given to it, and run in the connector's sandbox on donated hardware.
//
// The context is the agent's to manage. Every tool result is numbered; the agent prunes results it
// has finished with (prune_result), keeps what it needs in a scratchpad outside the context, and
// compacts when the context grows large. Nothing is ever dropped automatically, except that a
// conversation that outgrows its model's window is compacted by the harness, from the scratchpad.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/eidonix-ai/autobot/internal/store"
)

// Tool is a tool the agent may call. Run's error is shown to the model as the tool's result.
// Reads, when set, names the files a successful call read, so a report citing a file the agent
// never read can be caught.
type Tool struct {
	Spec  ToolSpec
	Run   func(ctx context.Context, args json.RawMessage) (string, error)
	Reads func(args json.RawMessage) []string
}

// How a run ended.
const (
	Met        = "met"        // finished, with the goal met
	Blocked    = "blocked"    // finished, unable to meet the goal, and saying why
	Budget     = "budget"     // out of turns or time
	Unfinished = "unfinished" // kept answering without finishing
	Failed     = "failed"     // the model could not be reached, or the run was stopped
)

// PlanItem is one step of the agent's own plan.
type PlanItem struct {
	Step   string `json:"step"`
	Status string `json:"status"` // pending, in_progress, done or dropped
	Note   string `json:"note,omitempty"`
}

// Transcript is one run: its outcome and what it cost. The conversation it added to is the
// Conversation's. It is saved after every turn. A run that ends with a written report has the
// writing session's own transcript in Writing: its turns and tokens are counted there, not here.
type Transcript struct {
	Plan    []PlanItem `json:"plan,omitempty"`
	Turns   int        `json:"turns"`
	In      int        `json:"tokens_in"`
	Out     int        `json:"tokens_out"`
	Cached  int        `json:"tokens_cached"`  // input tokens the provider answered from its cache
	Read    []string   `json:"read,omitempty"` // the files its tools have read
	Outcome string     `json:"outcome,omitempty"`
	Summary string     `json:"summary,omitempty"` // about 300 words: what a continuation opens with and a step's output tag holds
	Report  string     `json:"report,omitempty"`
	Error   string     `json:"error,omitempty"`

	Writing *Transcript `json:"writing,omitempty"`
}

// Config is one run.
type Config struct {
	Model    Model
	System   string // the system prompt, for a new conversation
	Task     string // the first message: the instruction, with the goal
	Goal     string
	Tools    []Tool // what can run now
	MaxTurns int
	// Verify, when set, judges a "met" finish; a false answer sends the agent back to work with why.
	Verify func(ctx context.Context, goal, report string) (ok bool, why string, err error)
	// Save is called with the transcript after every turn.
	Save func(*Transcript)

	RunID int64 // recorded with each turn, in the conversation's file

	// Pad is the scratchpad the scratchpad tools use.
	Pad Pad
	// Subs, when set, is the run's sub-agents: their notes can be read, by name.
	Subs SubView
	// Window is the model's context window in tokens; when 0, 128,000.
	Window int
	// Deadline is when the session's time is out, and WarnAt when it is first told how little is left:
	// at the next turn after WarnAt it gets one user message, once, whatever the turn. A session that
	// starts after WarnAt is told on its first turn. Zero WarnAt: no warning. Now is the clock (when
	// nil, the time).
	Deadline, WarnAt time.Time
	Now              func() time.Time
	// Read is what the tools of an earlier session of the same piece of work have read, so that a
	// citation of it is not marked "(not opened)".
	Read []string
	// WithSummary makes finish take a summary as well as the report: the session that writes the
	// report from the scratchpad. A finish without one is not refused: see Transcript.finish.
	WithSummary bool
}

const (
	maxNudges     = 3
	maxRejected   = 3
	maxResult     = 24_000 // characters of a tool's result shown to the model
	maxCalls      = 16     // tool calls one turn may make; the rest are refused, to be made next turn
	defaultWindow = 128_000
	forceAt       = 85 // percent of the window past which the agent is told to compact
)

var finishSpec = ToolSpec{Name: "finish", Description: "End the work. Call it when the goal is met, or when you are blocked and cannot meet it. " +
	"The report is your final answer: complete, specific, and saying what you did and found. Do not call it before the goal is met unless you are truly blocked.",
	Parameters: json.RawMessage(`{"type":"object","properties":{
		"status":{"type":"string","enum":["met","blocked"],"description":"met: the goal is met. blocked: it cannot be met, and the report says why."},
		"report":{"type":"string","description":"The final answer."}},"required":["status","report"]}`)}

// finishWriterSpec is finish for the session that writes the report from the scratchpad: a summary
// of the findings that matter most, and the full report.
var finishWriterSpec = ToolSpec{Name: "finish", Description: "End the work. Call it once the report is written, or when you are blocked and cannot write it. " +
	"It takes two parts, both required. The summary is at most about 300 words: the 5 to 8 findings that matter most, one line each with its citation, " +
	"then the most important open questions; no inventory of files. The report is the full report: it opens with a short \"Key findings\" section, then the detail.",
	Parameters: json.RawMessage(`{"type":"object","properties":{
		"status":{"type":"string","enum":["met","blocked"],"description":"met: the goal is met. blocked: it cannot be met, and the report says why."},
		"summary":{"type":"string","description":"At most about 300 words: the 5 to 8 findings that matter most, one line each with its citation, then the most important open questions."},
		"report":{"type":"string","description":"The full report, opening with a short Key findings section, then the detail."}},"required":["status","summary","report"]}`)}

// finishSummarySpec is finish for a research session whose report is written next, from its notes.
var finishSummarySpec = ToolSpec{Name: "finish", Description: "End the research. Call it when the goal is met, or when you are blocked and cannot meet it. " +
	"The report here is only a short summary of where you got to, a few paragraphs: the final report is written next, from your scratchpad notes, " +
	"so keep your findings in the scratchpad and do not repeat them in full here.",
	Parameters: finishSpec.Parameters}

var planSpec = ToolSpec{Name: "update_plan", Description: "Set your plan: the steps to meet the goal, each with its status and an optional note. Replaces the previous plan.",
	Parameters: json.RawMessage(`{"type":"object","properties":{"steps":{"type":"array","items":{"type":"object","properties":{
		"step":{"type":"string"},"status":{"type":"string"},"note":{"type":"string"}},
		"required":["step","status"]}}},"required":["steps"]}`)}

const nudge = "Keep working toward the goal with your tools. When the goal is met, call finish with status met and your report; " +
	"if you truly cannot meet it, call finish with status blocked and say why."

// notAvailable is the answer to a call of a tool the conversation was given but cannot run now.
const notAvailable = "error: this tool is not available now"

// turnsWarned is how many turns the agent has left when it is told so, once (Derek, 2026-11-23:
// warning at a quarter of the turns made it stop exploring far too early).
const turnsWarned = 5

// lastTurn is the agent's last message: on that turn finish is its only tool.
const lastTurn = "This is your last turn: call finish now with your report of what you have found."

// session is one Run: a conversation being added to.
type session struct {
	cfg       Config
	conv      *Conversation
	tr        *Transcript
	tools     map[string]Tool
	lineShown bool // this turn's answers already carry the context line
}

func (s *session) now() time.Time {
	if s.cfg.Now != nil {
		return s.cfg.Now()
	}
	return time.Now()
}

// WarnTime is the moment a session that must be done by deadline, having limit to work in, is warned:
// when percent of the limit is left. Zero when percent is 0 (or less): no warning.
func WarnTime(deadline time.Time, limit time.Duration, percent int) time.Time {
	if percent <= 0 {
		return time.Time{}
	}
	return deadline.Add(-limit * time.Duration(percent) / 100)
}

func (s *session) window() int {
	if s.cfg.Window > 0 {
		return s.cfg.Window
	}
	return defaultWindow
}

// turnSpecs adds what the turn needs to say and returns which tools may be called on it, or nil for
// all. The tools array sent never changes from turn to turn, since a changed array makes the provider
// start over on the whole request: a turn that allows less says so in its message, and a call
// outside it is answered with an error and not run. The last turn allows only finish; when the next
// request is estimated past 85% of the window, only what the agent needs to save its work and compact.
func (s *session) turnSpecs(turn int) (allowed []string, last, forced bool) {
	if s.cfg.MaxTurns > 1 && turn == s.cfg.MaxTurns-1 {
		allowed = []string{"finish"}
		s.conv.add(Message{Role: "user", Content: lastTurn + " Only " + availableOn(allowed) + "."})
		return allowed, true, false
	}
	if est := s.conv.estimate(); hasTool(s.conv.Tools, "compact") && est*100 > s.window()*forceAt {
		s.conv.add(Message{Role: "user", Content: fmt.Sprintf("Your context is at %d%% of its window. Save to your scratchpad what you still need, then call compact. Only %s.",
			est*100/s.window(), availableOn(forcedTools))})
		return forcedTools, false, true
	}
	return nil, false, false
}

// availableOn names the tools a narrowed turn allows: "finish is available on this turn".
func availableOn(names []string) string {
	list, verb := names[0], "is"
	switch len(names) {
	case 1:
	case 2:
		list, verb = names[0]+" and "+names[1], "are"
	default:
		list, verb = strings.Join(names[:len(names)-1], ", ")+" and "+names[len(names)-1], "are"
	}
	return list + " " + verb + " available on this turn"
}

// Run works until the agent finishes or runs out of turns or time (ctx), and returns the run's
// transcript. It adds to conv, or, when conv is nil, to a new one made from cfg.System and cfg.Tools;
// cfg.Task is its next message. A conversation sends the system prompt and tool definitions it was
// started with, whatever cfg says; a tool it has that cfg does not answers that it is not available.
func Run(ctx context.Context, cfg Config, conv *Conversation) *Transcript {
	tr := run(ctx, cfg, conv)
	tr.EnsureSummary()
	return tr
}

// EnsureSummary gives a transcript that has a report but no summary one made from the report.
func (tr *Transcript) EnsureSummary() {
	if strings.TrimSpace(tr.Summary) == "" {
		tr.Summary = store.FallbackSummary(tr.Report)
	}
}

func run(ctx context.Context, cfg Config, conv *Conversation) *Transcript {
	if conv == nil {
		conv = NewConversation("", 0, cfg.System, cfg.Tools)
	}
	tr := &Transcript{Read: slices.Clone(cfg.Read)}
	s := &session{cfg: cfg, conv: conv, tr: tr, tools: map[string]Tool{}}
	for _, t := range cfg.Tools {
		s.tools[t.Spec.Name] = t
	}
	conv.repair()
	conv.add(Message{Role: "user", Content: cfg.Task})
	save := func() {
		if cfg.Save != nil {
			cfg.Save(tr)
		}
	}
	nudges, rejected, warned := 0, 0, false
	for turn := 0; ; turn++ {
		if turn >= cfg.MaxTurns {
			tr.Outcome, tr.Error = Budget, fmt.Sprintf("stopped after %d turns without finishing", cfg.MaxTurns)
			save()
			return tr
		}
		if !warned && !cfg.WarnAt.IsZero() && !s.now().Before(cfg.WarnAt) {
			warned = true
			left := max(1, int(math.Ceil(cfg.Deadline.Sub(s.now()).Minutes())))
			conv.add(Message{Role: "user", Content: fmt.Sprintf("About %d minute%s left: finish with your report soon.", left, map[bool]string{true: "", false: "s"}[left == 1])})
		}
		// The last turn can only finish, so a run that reaches its limit still ends with its report.
		// (Its request leaves out the other tools, so it misses the provider's cache, once.)
		allowed, last, forced := s.turnSpecs(turn)
		reply, use, err := complete(ctx, cfg.Model, conv.request(), conv.Tools)
		if err != nil && tooLong(err) {
			// The provider refused the request as longer than its model's context: compact from the
			// scratchpad, and ask once more.
			s.compact(carriedByAutobot)
			allowed, last, forced = s.turnSpecs(turn)
			reply, use, err = complete(ctx, cfg.Model, conv.request(), conv.Tools)
		}
		if err != nil {
			tr.Outcome, tr.Error = Failed, err.Error()
			if ctx.Err() != nil {
				tr.Outcome, tr.Error = Budget, "stopped at the time limit without finishing"
			}
			save()
			return tr
		}
		tr.Turns++
		tr.In += use.In
		tr.Out += use.Out
		tr.Cached += use.Cached
		conv.sent(use.In)
		conv.add(reply)
		conv.recordTurn(cfg.RunID, use)
		if len(reply.ToolCalls) == 0 {
			if forced { // told to compact and did not: Autobot does it from the scratchpad
				s.compact(carriedByAutobot)
				save()
				continue
			}
			if last && strings.TrimSpace(reply.Content) != "" { // it answered in words: that is its report
				tr.Outcome, tr.Report, tr.Summary, tr.Error = Budget, reply.Content, "", fmt.Sprintf("stopped after %d turns without finishing", cfg.MaxTurns)
				save()
				return tr
			}
			if nudges++; nudges > maxNudges {
				tr.Outcome, tr.Report, tr.Summary, tr.Error = Unfinished, reply.Content, "", "kept answering without calling finish"
				save()
				return tr
			}
			conv.add(Message{Role: "user", Content: nudge})
			save()
			continue
		}
		nudges = 0
		s.lineShown = false
		results := make([]string, len(reply.ToolCalls))
		var finish *ToolCall
		var carry *string
		var wg sync.WaitGroup
		for i, call := range reply.ToolCalls {
			name := call.Function.Name
			if i >= maxCalls && name != "finish" {
				results[i] = fmt.Sprintf("error: at most %d tool calls run in one turn; make this one in your next turn", maxCalls)
				continue
			}
			if allowed != nil && !slices.Contains(allowed, name) {
				results[i] = "error: only " + availableOn(allowed)
				continue
			}
			switch name {
			case "finish":
				c := call
				finish = &c
				results[i] = "" // filled below, once the other calls of this turn are done
			case "update_plan":
				results[i] = tr.setPlan(call.Function.Arguments)
			default:
				if !hasTool(conv.Tools, name) {
					results[i] = "error: there is no tool " + name
					continue
				}
				if harnessTool(name) {
					results[i] = s.builtin(name, call.Function.Arguments, &carry)
					continue
				}
				t, ok := s.tools[name]
				if !ok { // one the conversation was given, that cannot run now
					results[i] = notAvailable
					continue
				}
				wg.Add(1)
				go func(i int, t Tool, args string) {
					defer wg.Done()
					results[i] = runTool(ctx, t, args)
				}(i, t, call.Function.Arguments)
			}
		}
		wg.Wait()
		for i, call := range reply.ToolCalls {
			if t, ok := s.tools[call.Function.Name]; ok && t.Reads != nil && !strings.HasPrefix(results[i], "error:") {
				for _, p := range t.Reads(json.RawMessage(call.Function.Arguments)) {
					if !slices.Contains(tr.Read, p) {
						tr.Read = append(tr.Read, p)
					}
				}
			}
		}
		done := false
		if finish != nil {
			var verdict string
			verdict, done = tr.finish(ctx, cfg, finish.Function.Arguments, &rejected)
			for i, call := range reply.ToolCalls {
				if call.ID == finish.ID {
					results[i] = verdict
				}
			}
		}
		for i, call := range reply.ToolCalls {
			out := clip(results[i])
			if line := s.contextLine(); i == len(reply.ToolCalls)-1 && line != "" && !s.lineShown {
				out += "\n" + line // appended, never rewriting what came before
			}
			conv.addResult(call.ID, out)
		}
		if !done {
			switch {
			case carry != nil:
				s.compact(*carry)
			case forced: // told to compact and did not: Autobot does it from the scratchpad
				s.compact(carriedByAutobot)
			}
		}
		if left := cfg.MaxTurns - turn - 1; !done && left == turnsWarned && cfg.MaxTurns > 2*turnsWarned {
			conv.add(Message{Role: "user", Content: fmt.Sprintf("You have %d turns left: finish with your report within them.", left)})
		}
		save()
		if done {
			return tr
		}
	}
}

// builtin runs one of the harness's own tools, other than finish and the plan. A compact call
// only notes what the agent carries forward: the compaction itself is after the turn's other calls.
func (s *session) builtin(name, args string, carry **string) (out string) {
	defer func() { // a panic in the scratchpad or the like is an error answer, as in runTool, not a dead run
		if r := recover(); r != nil {
			out = fmt.Sprintf("error: the tool failed: %v", r)
		}
	}()
	switch name {
	case "prune_result":
		return s.prune(args)
	case "compact":
		var a struct {
			Carry string `json:"carry_forward"`
		}
		if json.Unmarshal([]byte(args), &a) != nil {
			return `error: compact takes {"carry_forward": text}`
		}
		if strings.TrimSpace(a.Carry) == "" {
			a.Carry = "(nothing: your notes are in the scratchpad)"
		}
		*carry = &a.Carry
		return "Compacting."
	case "scratchpad_write":
		return s.padWrite(args)
	case "scratchpad_index":
		return s.padIndex(args)
	case "scratchpad_read":
		return s.padRead(args)
	case "scratchpad_search":
		return s.padSearch(args)
	}
	return "error: there is no tool " + name
}

// finish handles a call to finish: accepted, it sets the outcome and returns done. Finish takes the
// report: a file it cites that the agent never opened is marked "(not opened)" where it is cited,
// not held against it (Derek, 2026-11-23, after two runs lost finished reports to refusals near
// their turn limit). Only the optional check by another model (Verify) sends the agent back, and its
// report is kept meanwhile, so a run that then runs out of turns still has it.
//
// The session that writes a report (cfg.WithSummary) gives a summary too. One it leaves out is not
// a reason to refuse: the report must never be lost, so the summary is made from it (see
// store.FallbackSummary) and says so. Any other session's report is its own summary.
func (tr *Transcript) finish(ctx context.Context, cfg Config, args string, rejected *int) (string, bool) {
	var a struct{ Status, Summary, Report string }
	if json.Unmarshal([]byte(args), &a) != nil || (a.Status != Met && a.Status != Blocked) || strings.TrimSpace(a.Report) == "" {
		return "error: finish needs a status (met or blocked) and a report", false
	}
	a.Report = tr.annotate(a.Report)
	summary := a.Report
	if cfg.WithSummary {
		summary = store.FallbackSummary(a.Report)
		if strings.TrimSpace(a.Summary) != "" {
			summary = tr.annotate(strings.TrimSpace(a.Summary))
		}
	}
	if a.Status == Met {
		if cfg.Verify != nil && *rejected < maxRejected {
			ok, why, err := cfg.Verify(ctx, cfg.Goal, a.Report)
			if err == nil && !ok {
				*rejected++
				tr.Report, tr.Summary = a.Report, summary
				return "Not finished: a check of your report found the goal not yet met" + why +
					". The goal: " + cfg.Goal + "\nKeep working, then finish again.", false
			}
		}
	}
	tr.Outcome, tr.Report, tr.Summary = a.Status, a.Report, summary
	return "Finished.", true
}

func (tr *Transcript) setPlan(args string) string {
	var a struct {
		Steps []PlanItem `json:"steps"`
	}
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return "error: the plan must be {\"steps\": [{\"step\": …, \"status\": …}]}"
	}
	tr.Plan = a.Steps
	return "Plan updated."
}

func runTool(ctx context.Context, t Tool, args string) (out string) {
	defer func() {
		if r := recover(); r != nil {
			out = fmt.Sprintf("error: the tool failed: %v", r)
		}
	}()
	if strings.TrimSpace(args) == "" {
		args = "{}"
	}
	res, err := t.Run(ctx, json.RawMessage(args))
	if err != nil {
		return "error: " + err.Error()
	}
	return res
}

// complete asks the model, trying again on a failure that may pass.
func complete(ctx context.Context, m Model, msgs []Message, tools []ToolSpec) (Message, Usage, error) {
	var err error
	for try, wait := 0, 2*time.Second; try < 4; try, wait = try+1, wait*3 {
		var reply Message
		var use Usage
		if reply, use, err = m.Complete(ctx, msgs, tools); err == nil {
			return reply, use, nil
		}
		if ctx.Err() != nil || !transient(err) {
			return Message{}, Usage{}, err
		}
		select {
		case <-ctx.Done():
			return Message{}, Usage{}, ctx.Err()
		case <-time.After(wait):
		}
	}
	return Message{}, Usage{}, err
}

// transient reports whether a model error may pass: too many requests, a server error, no answer.
func transient(err error) bool {
	s := err.Error()
	// "reading the answer": the connection broke while the answer came; asking again is safe.
	for _, code := range []string{" 429 ", " 500 ", " 502 ", " 503 ", " 504 ", "could not reach", "reading the answer"} {
		if strings.Contains(s, code) {
			return true
		}
	}
	return errors.Is(err, context.DeadlineExceeded)
}

// citedRE finds files a report names: a path or file name with a source-like extension, perhaps
// followed by a line number.
var citedRE = regexp.MustCompile(`\b([\w./-]*[\w-]+\.(?:go|mod|py|rs|ts|tsx|js|jsx|c|h|cc|cpp|hpp|java|kt|swift|rb|php|cs|sh|ya?ml|toml|json|md|sql|proto))\b`)

// citedLines is what may follow a cited file: its line or lines, and the end of a code span.
var citedLines = regexp.MustCompile("^(?::\\d+(?:[-–]\\d+)?)?`?")

// annotate marks each place the report cites a file the agent's tools never read: "(not opened)"
// after the citation, its line numbers and any code span around it.
func (tr *Transcript) annotate(report string) string {
	var b strings.Builder
	last := 0
	for _, ix := range citedRE.FindAllStringSubmatchIndex(report, -1) {
		c := strings.TrimPrefix(strings.TrimPrefix(report[ix[2]:ix[3]], "./"), "/")
		if slices.ContainsFunc(tr.Read, func(r string) bool { return samePath(r, c) }) {
			continue
		}
		end := ix[1] + len(citedLines.FindString(report[ix[1]:]))
		b.WriteString(report[last:end])
		b.WriteString(" (not opened)")
		last = end
	}
	b.WriteString(report[last:])
	return b.String()
}

// samePath reports whether two paths name the same file, one perhaps given relative to a folder.
func samePath(a, b string) bool {
	a, b = strings.TrimPrefix(a, "/"), strings.TrimPrefix(b, "/")
	return a == b || strings.HasSuffix(a, "/"+b) || strings.HasSuffix(b, "/"+a)
}

// clip keeps a long result's beginning and end, which is where the useful parts usually are.
func clip(s string) string {
	if len(s) <= maxResult {
		return s
	}
	head, tail := s[:maxResult*2/3], s[len(s)-maxResult/3:]
	return strings.ToValidUTF8(head, "") + fmt.Sprintf("\n[… %d characters left out …]\n", len(s)-len(head)-len(tail)) + strings.ToValidUTF8(tail, "")
}
