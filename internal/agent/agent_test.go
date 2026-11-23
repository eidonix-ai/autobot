package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// script is a model that answers with the replies it was given, in order, and records what it saw.
type script struct {
	replies []Message
	seen    [][]Message
	tools   [][]ToolSpec
}

func (s *script) Complete(ctx context.Context, msgs []Message, tools []ToolSpec) (Message, Usage, error) {
	s.seen = append(s.seen, append([]Message{}, msgs...))
	s.tools = append(s.tools, tools)
	if len(s.seen) > len(s.replies) {
		return Message{}, Usage{}, errors.New("the script ran out")
	}
	return s.replies[len(s.seen)-1], Usage{In: 10, Out: 2}, nil
}

func call(id, name, args string) Message {
	tc := ToolCall{ID: id, Type: "function"}
	tc.Function.Name, tc.Function.Arguments = name, args
	return Message{Role: "assistant", ToolCalls: []ToolCall{tc}}
}

func calls(cs ...Message) Message {
	m := Message{Role: "assistant"}
	for _, c := range cs {
		m.ToolCalls = append(m.ToolCalls, c.ToolCalls...)
	}
	return m
}

func last(msgs []Message) string { return msgs[len(msgs)-1].Content }

// A reply without a tool call does not end the run: the agent is told to keep going.
func TestNudge(t *testing.T) {
	m := &script{replies: []Message{
		{Role: "assistant", Content: "Looks fine to me."},
		call("f", "finish", `{"status":"met","report":"Reviewed: no problems."}`),
	}}
	tr := Run(context.Background(), Config{Model: m, System: "s", Task: "t", MaxTurns: 10}, nil)
	if tr.Outcome != Met || tr.Report != "Reviewed: no problems." || last(m.seen[1]) != nudge || tr.In != 20 {
		t.Fatalf("%+v", tr)
	}
	// Answering again and again without finishing ends the run as unfinished.
	m = &script{replies: []Message{{Content: "a"}, {Content: "b"}, {Content: "c"}, {Content: "d"}}}
	if tr := Run(context.Background(), Config{Model: m, MaxTurns: 10}, nil); tr.Outcome != Unfinished {
		t.Fatalf("%+v", tr)
	}
}

// The plan is the agent's own notes: any status word is taken, and finish takes the report whatever
// the plan says (Derek, 2026-11-23: a run lost a finished report to a plan marked "completed").
func TestPlanNeverHoldsFinish(t *testing.T) {
	m := &script{replies: []Message{
		call("p", "update_plan", `{"steps":[{"step":"read the diff","status":"completed"},{"step":"run the tests","status":"pending"}]}`),
		call("f", "finish", `{"status":"met","report":"the report"}`),
	}}
	tr := Run(context.Background(), Config{Model: m, MaxTurns: 10}, nil)
	if tr.Outcome != Met || tr.Report != "the report" || len(tr.Plan) != 2 || tr.Plan[0].Status != "completed" {
		t.Fatalf("%+v", tr)
	}
}

// A check that finds the goal unmet sends the agent back to work.
func TestVerify(t *testing.T) {
	m := &script{replies: []Message{
		call("r", "read", `{"path":"internal/a.go"}`),
		call("f1", "finish", `{"status":"met","report":"probably fine"}`),
		call("f2", "finish", `{"status":"met","report":"Read all 4 files; found a nil dereference at a.go:12."}`),
	}}
	checks := 0
	verify := func(ctx context.Context, goal, report string) (bool, string, error) {
		checks++
		return strings.Contains(report, "a.go:12"), " (no file or line given)", nil
	}
	tr := Run(context.Background(), Config{Model: m, Goal: "list each problem with its file and line", Verify: verify, MaxTurns: 10,
		Tools: []Tool{readTool}}, nil)
	if tr.Outcome != Met || checks != 2 || !strings.Contains(last(m.seen[2]), "file and line") {
		t.Fatalf("%+v", tr)
	}
}

// The tool calls of one turn run at the same time: each of these waits for the other.
func TestParallelTools(t *testing.T) {
	a, b := make(chan bool), make(chan bool)
	meet := func(mine, theirs chan bool) Tool {
		return Tool{Spec: ToolSpec{Name: "t" + string(rune('a'+len(mine)))}, Run: func(ctx context.Context, _ json.RawMessage) (string, error) {
			close(mine)
			select {
			case <-theirs:
				return "met", nil
			case <-time.After(2 * time.Second):
				return "", errors.New("ran alone")
			}
		}}
	}
	ta, tb := meet(a, b), meet(b, a)
	ta.Spec.Name, tb.Spec.Name = "ta", "tb"
	m := &script{replies: []Message{
		calls(call("1", "ta", `{}`), call("2", "tb", `{}`)),
		call("f", "finish", `{"status":"met","report":"ok"}`),
	}}
	tr := Run(context.Background(), Config{Model: m, Tools: []Tool{ta, tb}, MaxTurns: 5}, nil)
	got := m.seen[1]
	if tr.Outcome != Met || got[len(got)-2].Content != "met" || got[len(got)-1].Content != "met" {
		t.Fatalf("results %q %q", got[len(got)-2].Content, got[len(got)-1].Content)
	}
}

// The turn limit ends a run that never finishes, and a long result keeps its start and end.
func TestBudgetAndClip(t *testing.T) {
	long := Tool{Spec: ToolSpec{Name: "long"}, Run: func(context.Context, json.RawMessage) (string, error) {
		return "START" + strings.Repeat("x", 100_000) + "END", nil
	}}
	m := &script{replies: []Message{call("1", "long", `{}`), call("2", "long", `{}`), call("3", "long", `{}`)}}
	tr := Run(context.Background(), Config{Model: m, Tools: []Tool{long}, MaxTurns: 2}, nil)
	res := m.seen[1][len(m.seen[1])-2].Content // the tool's result, before the last-turn message
	if tr.Outcome != Budget || len(res) > maxResult+100 || !strings.HasPrefix(res, "START") || !strings.HasSuffix(res, "END") {
		t.Fatalf("%s, result of %d characters", tr.Outcome, len(res))
	}
}

var readTool = Tool{Spec: ToolSpec{Name: "read"},
	Run: func(context.Context, json.RawMessage) (string, error) { return "package a", nil },
	Reads: func(raw json.RawMessage) []string {
		var a struct{ Path string }
		json.Unmarshal(raw, &a)
		return []string{a.Path}
	}}

// Finish takes the report, and marks where it cites a file the agent never opened (GLM-5.3-Flash
// once cited four files it never opened).
func TestUnreadCitationsAreMarked(t *testing.T) {
	m := &script{replies: []Message{
		call("r", "read", `{"path":"internal/target.go"}`),
		call("f", "finish", `{"status":"met","report":"target.go:302 is fixed; every reader in query.go:253-260 and `+"`frame.go`"+` checks for nil, as query.go says. See https://go.dev/doc."}`),
	}}
	tr := Run(context.Background(), Config{Model: m, Tools: []Tool{readTool}, MaxTurns: 10}, nil)
	want := "target.go:302 is fixed; every reader in query.go:253-260 (not opened) and `frame.go` (not opened) checks for nil, as query.go (not opened) says. See https://go.dev/doc."
	if tr.Outcome != Met || tr.Turns != 2 || tr.Report != want {
		t.Fatalf("%s %d:\n%s\nwant\n%s", tr.Outcome, tr.Turns, tr.Report, want)
	}
}

// A report the check sends back is kept: a run that then runs out of turns still has it.
func TestARefusedReportIsKept(t *testing.T) {
	m := &script{replies: []Message{
		call("f", "finish", `{"status":"met","report":"the first report"}`),
		call("r", "read", `{"path":"a.go"}`),
	}}
	never := func(context.Context, string, string) (bool, string, error) { return false, "", nil }
	tr := Run(context.Background(), Config{Model: m, Tools: []Tool{readTool}, Verify: never, MaxTurns: 2}, nil)
	if tr.Outcome != Budget || tr.Report != "the first report" {
		t.Fatalf("%s: %q", tr.Outcome, tr.Report)
	}
}

// Running low on turns, the agent is told to wrap up. Results are not cut while the request is small:
// what was sent is never rewritten (see cache_test.go for when it is).
func TestBudgetWarning(t *testing.T) {
	var replies []Message
	for i := 0; i < 12; i++ {
		replies = append(replies, call(fmt.Sprint(i), "long", `{}`))
	}
	long := Tool{Spec: ToolSpec{Name: "long"}, Run: func(context.Context, json.RawMessage) (string, error) {
		return "first line\n" + strings.Repeat("y", 5000), nil
	}}
	replies[11] = call("f", "finish", `{"status":"met","report":"what I found"}`)
	m := &script{replies: replies}
	tr := Run(context.Background(), Config{Model: m, Tools: []Tool{long}, MaxTurns: 12}, nil)
	var warnings []int
	for i, msgs := range m.seen {
		if strings.Contains(last(msgs), "turns left") {
			warnings = append(warnings, i+1)
		}
	}
	// One warning, with 5 turns left (before turn 8); on the last turn finish is the only tool.
	if len(warnings) != 1 || warnings[0] != 8 || !strings.Contains(m.seen[7][len(m.seen[7])-1].Content, "5 turns left") {
		t.Fatalf("warned before turns %v", warnings)
	}
	if len(m.tools[10]) < 3 || len(m.tools[11]) != 1 || m.tools[11][0].Name != "finish" || !strings.Contains(last(m.seen[11]), "last turn") {
		t.Fatalf("turn 11 had %d tools, the last %v", len(m.tools[10]), m.tools[11])
	}
	if tr.Outcome != Met || tr.Report != "what I found" {
		t.Fatalf("%s: %q", tr.Outcome, tr.Report)
	}
	// One that answers its last turn in words has that as its report.
	replies[11] = Message{Role: "assistant", Content: "my findings, in words"}
	m = &script{replies: replies}
	if tr := Run(context.Background(), Config{Model: m, Tools: []Tool{long}, MaxTurns: 12}, nil); tr.Outcome != Budget || tr.Report != "my findings, in words" {
		t.Fatalf("%s: %q", tr.Outcome, tr.Report)
	}
}

// One turn runs at most maxCalls tool calls; the rest are refused, to be made next turn.
func TestCallsPerTurn(t *testing.T) {
	var runs atomic.Int32 // the calls of one turn run at the same time
	tool := Tool{Spec: ToolSpec{Name: "t"}, Run: func(context.Context, json.RawMessage) (string, error) { runs.Add(1); return "ok", nil }}
	var many []Message
	for i := 0; i < maxCalls+5; i++ {
		many = append(many, call(fmt.Sprint(i), "t", `{}`))
	}
	m := &script{replies: []Message{calls(many...), call("f", "finish", `{"status":"met","report":"done"}`)}}
	Run(context.Background(), Config{Model: m, Tools: []Tool{tool}, MaxTurns: 5}, nil)
	if runs.Load() != maxCalls || !strings.Contains(last(m.seen[1]), "next turn") {
		t.Fatalf("%d calls ran", runs.Load())
	}
}
