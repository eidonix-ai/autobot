// Package agent is Autobot's harness: a model working toward a goal over many turns with tools,
// until it calls finish. It stops only through finish (or its budget): a reply without a tool call
// is answered with a nudge to keep going, finish with "met" is refused while the agent's own plan
// has open items, and an optional check (a decision model) can send it back to work. Tool calls of
// one turn run in parallel. Nothing here runs commands: tools that touch code or machines are
// given to it, and run in the connector's sandbox on donated hardware.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
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
// Conversation's. It is saved after every turn.
type Transcript struct {
	Plan    []PlanItem `json:"plan,omitempty"`
	Turns   int        `json:"turns"`
	In      int        `json:"tokens_in"`
	Out     int        `json:"tokens_out"`
	Cached  int        `json:"tokens_cached"`  // input tokens the provider answered from its cache
	Read    []string   `json:"read,omitempty"` // the files its tools have read, in this conversation
	Outcome string     `json:"outcome,omitempty"`
	Report  string     `json:"report,omitempty"`
	Error   string     `json:"error,omitempty"`
}

// Config is one run.
type Config struct {
	Model    Model
	System   string // the system prompt, for a new conversation (a resumed one keeps its own)
	Task     string // the next message: the instruction, with the goal
	Goal     string
	Tools    []Tool // what can run now; a resumed conversation sends its own stored definitions
	MaxTurns int
	// Verify, when set, judges a "met" finish; a false answer sends the agent back to work with why.
	Verify func(ctx context.Context, goal, report string) (ok bool, why string, err error)
	// Save is called with the transcript after every turn.
	Save func(*Transcript)

	RunID int64 // recorded with each turn and with the end of the run, in the conversation's file
	// What the run saw of the item (its head commit and when), recorded when it ends so that a later
	// run of the conversation knows what is new.
	Head string
	Seen time.Time
	// Once a request is estimated at BatchTokens (when 0: 60% of Window, the model's context window
	// in tokens, when known, else 100,000) the conversation's old tool results are shortened, in one batch.
	Window      int
	BatchTokens int
}

const (
	maxNudges    = 3
	maxRejected  = 3
	maxResult    = 24_000 // characters of a tool's result shown to the model
	keepTurns    = 6      // when old tool results are shortened, those of the last turns are kept in full
	maxCalls     = 16     // tool calls one turn may make; the rest are refused, to be made next turn
	defaultBatch = 100_000
)

// batchAt is the estimated size, in tokens, of a request past which old tool results are shortened.
func (cfg Config) batchAt() int {
	switch {
	case cfg.BatchTokens > 0:
		return cfg.BatchTokens
	case cfg.Window > 0:
		return cfg.Window * 6 / 10
	}
	return defaultBatch
}

var finishSpec = ToolSpec{Name: "finish", Description: "End the work. Call it when the goal is met, or when you are blocked and cannot meet it. " +
	"The report is your final answer: complete, specific, and saying what you did and found. Do not call it before the goal is met unless you are truly blocked.",
	Parameters: json.RawMessage(`{"type":"object","properties":{
		"status":{"type":"string","enum":["met","blocked"],"description":"met: the goal is met. blocked: it cannot be met, and the report says why."},
		"report":{"type":"string","description":"The final answer."}},"required":["status","report"]}`)}

var planSpec = ToolSpec{Name: "update_plan", Description: "Set your plan: the steps to meet the goal, each with its status and an optional note. Replaces the previous plan.",
	Parameters: json.RawMessage(`{"type":"object","properties":{"steps":{"type":"array","items":{"type":"object","properties":{
		"step":{"type":"string"},"status":{"type":"string"},"note":{"type":"string"}},
		"required":["step","status"]}}},"required":["steps"]}`)}

const nudge = "Keep working toward the goal with your tools. When the goal is met, call finish with status met and your report; " +
	"if you truly cannot meet it, call finish with status blocked and say why."

// notAvailable is the answer to a call of a tool the conversation was given but cannot run now.
const notAvailable = "error: this tool is not available now"

// Run works until the agent finishes or runs out of turns or time (ctx), and returns the run's
// transcript. It adds to conv, a conversation to continue (cfg.Task is then its next message), or,
// when conv is nil, to a new one made from cfg.System and cfg.Tools. A continued conversation sends
// the system prompt and tool definitions it was started with, whatever cfg says; a tool it has
// that cfg does not (a sandbox that is gone) answers that it is not available now.
func Run(ctx context.Context, cfg Config, conv *Conversation) *Transcript {
	if conv == nil {
		conv = NewConversation("", 0, cfg.System, cfg.Tools)
	}
	tr := &Transcript{Read: slices.Clone(conv.Read)}
	conv.repair()
	conv.add(Message{Role: "user", Content: cfg.Task})
	tools := map[string]Tool{}
	for _, t := range cfg.Tools {
		tools[t.Spec.Name] = t
	}
	save := func() {
		if cfg.Save != nil {
			cfg.Save(tr)
		}
	}
	defer func() { conv.endRun(cfg.RunID, cfg.Head, cfg.Seen, tr.Read) }() // the caller's last save writes it
	nudges, rejected, afterBatch := 0, 0, 0
	for turn := 0; ; turn++ {
		if turn >= cfg.MaxTurns {
			tr.Outcome, tr.Error = Budget, fmt.Sprintf("stopped after %d turns without finishing", cfg.MaxTurns)
			save()
			return tr
		}
		// A request that has grown too large has its old tool results shortened, once, for good. Never
		// every turn: each time the start of the conversation changes, the provider's cache starts over.
		// Over the limit even after that, it waits until the request has grown by a fifth of the limit.
		if est := conv.tokens(); est > cfg.batchAt() && est >= afterBatch+cfg.batchAt()/5 && conv.shortenOld() {
			afterBatch = conv.tokens()
		}
		reply, use, err := complete(ctx, cfg.Model, conv.request(), conv.Tools)
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
		conv.add(reply)
		conv.recordTurn(cfg.RunID, use)
		if len(reply.ToolCalls) == 0 {
			if nudges++; nudges > maxNudges {
				tr.Outcome, tr.Report, tr.Error = Unfinished, reply.Content, "kept answering without calling finish"
				save()
				return tr
			}
			conv.add(Message{Role: "user", Content: nudge})
			save()
			continue
		}
		nudges = 0
		results := make([]string, len(reply.ToolCalls))
		var finish *ToolCall
		var wg sync.WaitGroup
		for i, call := range reply.ToolCalls {
			if i >= maxCalls && call.Function.Name != "finish" {
				results[i] = fmt.Sprintf("error: at most %d tool calls run in one turn; make this one in your next turn", maxCalls)
				continue
			}
			switch call.Function.Name {
			case "finish":
				c := call
				finish = &c
				results[i] = "" // filled below, once the other calls of this turn are done
			case "update_plan":
				results[i] = tr.setPlan(call.Function.Arguments)
			default:
				t, ok := tools[call.Function.Name]
				if !slices.ContainsFunc(conv.Tools, func(sp ToolSpec) bool { return sp.Name == call.Function.Name }) {
					results[i] = "error: there is no tool " + call.Function.Name
					continue
				}
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
			if t, ok := tools[call.Function.Name]; ok && t.Reads != nil && !strings.HasPrefix(results[i], "error:") {
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
			conv.add(Message{Role: "tool", ToolCallID: call.ID, Content: clip(results[i])})
		}
		if left := cfg.MaxTurns - turn - 1; !done && (left == max(3, cfg.MaxTurns/4) || left == 2) {
			conv.add(Message{Role: "user", Content: fmt.Sprintf("You have %d turns left. "+
				"Stop exploring and finish soon with what you have found: call finish with your report.", left)})
		}
		save()
		if done {
			return tr
		}
	}
}

// finish handles a call to finish: accepted, it sets the outcome and returns done.
func (tr *Transcript) finish(ctx context.Context, cfg Config, args string, rejected *int) (string, bool) {
	var a struct{ Status, Report string }
	if json.Unmarshal([]byte(args), &a) != nil || (a.Status != Met && a.Status != Blocked) || strings.TrimSpace(a.Report) == "" {
		return "error: finish needs a status (met or blocked) and a report", false
	}
	if a.Status == Met {
		if unread := tr.unread(a.Report); len(unread) > 0 && *rejected < maxRejected {
			*rejected++
			return "Not finished: your report cites " + strings.Join(unread, ", ") + ", which you have not read in this run. " +
				"Read them to confirm what you say about them, or take those claims out, then finish again.", false
		}
		if cfg.Verify != nil && *rejected < maxRejected {
			ok, why, err := cfg.Verify(ctx, cfg.Goal, a.Report)
			if err == nil && !ok {
				*rejected++
				return "Not finished: a check of your report found the goal not yet met" + why +
					". The goal: " + cfg.Goal + "\nKeep working, then finish again.", false
			}
		}
	}
	tr.Outcome, tr.Report = a.Status, a.Report
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
	for _, code := range []string{" 429 ", " 500 ", " 502 ", " 503 ", " 504 ", "could not reach"} {
		if strings.Contains(s, code) {
			return true
		}
	}
	return errors.Is(err, context.DeadlineExceeded)
}

// citedRE finds files a report names: a path or file name with a source-like extension, perhaps
// followed by a line number.
var citedRE = regexp.MustCompile(`\b([\w./-]*[\w-]+\.(?:go|mod|py|rs|ts|tsx|js|jsx|c|h|cc|cpp|hpp|java|kt|swift|rb|php|cs|sh|ya?ml|toml|json|md|sql|proto))\b`)

// unread lists the files a report cites that the agent's tools never read.
func (tr *Transcript) unread(report string) []string {
	var out []string
	for _, m := range citedRE.FindAllStringSubmatch(report, -1) {
		c := strings.TrimPrefix(strings.TrimPrefix(m[1], "./"), "/")
		if slices.Contains(out, c) || slices.ContainsFunc(tr.Read, func(r string) bool { return samePath(r, c) }) {
			continue
		}
		out = append(out, c)
	}
	return out
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
