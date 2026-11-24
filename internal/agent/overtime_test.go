package agent

import (
	"context"
	"strings"
	"testing"
	"time"
)

// overtimeModel answers from replies, but its second request waits for the run's time limit, as a slow
// model does when the time runs out in the middle of a request.
func overtimeModel(m *script) Model {
	return modelFunc(func(ctx context.Context, msgs []Message, tools []ToolSpec) (Message, Usage, error) {
		if len(m.seen) == 1 {
			m.seen = append(m.seen, append([]Message{}, msgs...))
			<-ctx.Done()
			return Message{}, Usage{}, ctx.Err()
		}
		return m.Complete(ctx, msgs, tools)
	})
}

// An agent cut off by its time limit gets one more turn, finish only, and its report is kept.
func TestTimeLimitGivesOneFinishTurn(t *testing.T) {
	m := &script{replies: []Message{call("a", "read", `{"path":"x.go"}`), finishCall("what I found")}}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	tr := Run(ctx, Config{Model: overtimeModel(m), Tools: []Tool{readTool}, MaxTurns: 30, Task: "go"}, nil)
	if tr.Outcome != Met || tr.Report != "what I found" {
		t.Fatalf("outcome %q, report %q, error %q", tr.Outcome, tr.Report, tr.Error)
	}
	if got := last(m.seen[len(m.seen)-1]); !strings.HasPrefix(got, timeUp+lastTurn) {
		t.Fatalf("the extra turn said %q", got)
	}
}

// The extra turn is one turn: an agent that still does not finish ends out of time, and a reply in
// words is its report.
func TestTimeLimitExtraTurnIsOne(t *testing.T) {
	for _, tc := range []struct {
		reply  Message
		report string
	}{
		{call("b", "read", `{"path":"x.go"}`), ""},
		{Message{Role: "assistant", Content: "my findings in words"}, "my findings in words"},
	} {
		m := &script{replies: []Message{call("a", "read", `{"path":"x.go"}`), tc.reply, finishCall("too late")}}
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		tr := Run(ctx, Config{Model: overtimeModel(m), Tools: []Tool{readTool}, MaxTurns: 30, Task: "go"}, nil)
		cancel()
		if tr.Outcome != Budget || tr.Report != tc.report || tr.Error != "stopped at the time limit without finishing" {
			t.Fatalf("outcome %q, report %q, error %q", tr.Outcome, tr.Report, tr.Error)
		}
		if m.given != 2 {
			t.Fatalf("%d answers used: the extra turn is one", m.given)
		}
	}
}

// A person's stop is not a time limit: the run ends at once, with no extra turn.
func TestStopGivesNoExtraTurn(t *testing.T) {
	m := &script{replies: []Message{call("a", "read", `{"path":"x.go"}`), finishCall("should not run")}}
	ctx, cancel := context.WithCancelCause(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel(&StopError{By: "derek"}) }()
	tr := Run(ctx, Config{Model: overtimeModel(m), Tools: []Tool{readTool}, MaxTurns: 30, Task: "go"}, nil)
	if tr.Outcome != Stopped || m.given != 1 {
		t.Fatalf("outcome %q after %d answers", tr.Outcome, m.given)
	}
}
