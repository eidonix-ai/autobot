package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// script is a model that answers with the replies it was given, in order, and records what it saw.
type script struct {
	replies []Message
	seen    [][]Message
}

func (s *script) Complete(ctx context.Context, msgs []Message, tools []ToolSpec) (Message, Usage, error) {
	s.seen = append(s.seen, append([]Message{}, msgs...))
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

// "met" is refused while the agent's plan has open steps, and accepted once they are done.
func TestPlanHoldsFinish(t *testing.T) {
	m := &script{replies: []Message{
		call("p", "update_plan", `{"steps":[{"step":"read the diff","status":"done"},{"step":"run the tests","status":"pending"}]}`),
		call("f1", "finish", `{"status":"met","report":"done"}`),
		call("p2", "update_plan", `{"steps":[{"step":"read the diff","status":"done"},{"step":"run the tests","status":"dropped","note":"no machine"}]}`),
		call("f2", "finish", `{"status":"met","report":"done"}`),
	}}
	tr := Run(context.Background(), Config{Model: m, MaxTurns: 10}, nil)
	if tr.Outcome != Met || !strings.Contains(last(m.seen[2]), "run the tests") {
		t.Fatalf("%+v\n%s", tr, last(m.seen[2]))
	}
	// Blocked is always accepted: the agent says it cannot go on.
	m = &script{replies: []Message{
		call("p", "update_plan", `{"steps":[{"step":"x","status":"pending"}]}`),
		call("f", "finish", `{"status":"blocked","report":"cn7 has no free GPU"}`),
	}}
	if tr := Run(context.Background(), Config{Model: m, MaxTurns: 10}, nil); tr.Outcome != Blocked {
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
	res := last(m.seen[1])
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

// A report may cite only files the agent read: GLM-5.3-Flash once cited four files it never opened.
func TestCitedFilesMustBeRead(t *testing.T) {
	m := &script{replies: []Message{
		call("r", "read", `{"path":"internal/target.go"}`),
		call("f1", "finish", `{"status":"met","report":"target.go:302 is fixed; every reader in query.go:253 and frame.go checks for nil."}`),
		calls(call("r2", "read", `{"path":"query.go"}`), call("r3", "read", `{"path":"internal/frame.go"}`)),
		call("f2", "finish", `{"status":"met","report":"target.go:302 is fixed; every reader in query.go:253 and frame.go checks for nil. See https://go.dev/doc."}`),
	}}
	tr := Run(context.Background(), Config{Model: m, Tools: []Tool{readTool}, MaxTurns: 10}, nil)
	refusal := last(m.seen[2])
	if tr.Outcome != Met || !strings.Contains(refusal, "query.go, frame.go") || strings.Contains(refusal, "target.go") {
		t.Fatalf("%s: %q", tr.Outcome, refusal)
	}
}

// Running low on turns, the agent is told to wrap up; old tool results are cut when sent.
func TestBudgetWarningAndTrimming(t *testing.T) {
	var replies []Message
	for i := 0; i < 12; i++ {
		replies = append(replies, call(fmt.Sprint(i), "long", `{}`))
	}
	long := Tool{Spec: ToolSpec{Name: "long"}, Run: func(context.Context, json.RawMessage) (string, error) {
		return "first line\n" + strings.Repeat("y", 5000), nil
	}}
	m := &script{replies: replies}
	Run(context.Background(), Config{Model: m, Tools: []Tool{long}, MaxTurns: 12}, nil)
	warned := 0
	for _, msg := range m.seen[len(m.seen)-1] {
		if msg.Role == "user" && strings.Contains(msg.Content, "turns left") {
			warned++
		}
	}
	if warned != 2 {
		t.Fatalf("%d warnings", warned)
	}
	sent := m.seen[len(m.seen)-1]
	var full, cut int
	for _, msg := range sent {
		if msg.Role == "tool" {
			if strings.Contains(msg.Content, "left out to save room") {
				cut++
			} else if len(msg.Content) > 5000 {
				full++
			}
		}
	}
	if full != keepTurns || cut == 0 {
		t.Fatalf("%d sent in full, %d cut", full, cut)
	}
}

// One turn runs at most maxCalls tool calls; the rest are refused, to be made next turn.
func TestCallsPerTurn(t *testing.T) {
	runs := 0
	tool := Tool{Spec: ToolSpec{Name: "t"}, Run: func(context.Context, json.RawMessage) (string, error) { runs++; return "ok", nil }}
	var many []Message
	for i := 0; i < maxCalls+5; i++ {
		many = append(many, call(fmt.Sprint(i), "t", `{}`))
	}
	m := &script{replies: []Message{calls(many...), call("f", "finish", `{"status":"met","report":"done"}`)}}
	Run(context.Background(), Config{Model: m, Tools: []Tool{tool}, MaxTurns: 5}, nil)
	if runs != maxCalls || !strings.Contains(last(m.seen[1]), "next turn") {
		t.Fatalf("%d calls ran", runs)
	}
}
