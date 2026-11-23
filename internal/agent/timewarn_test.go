package agent

import (
	"context"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// clock is a clock the tests move.
type clock struct{ at atomic.Int64 } // nanoseconds since base

var base = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

func (c *clock) now() time.Time          { return base.Add(time.Duration(c.at.Load())) }
func (c *clock) advance(d time.Duration) { c.at.Add(int64(d)) }
func (c *clock) set(d time.Duration)     { c.at.Store(int64(d)) }

func warnings(msgs []Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role == "user" && strings.Contains(m.Content, "minute") && strings.Contains(m.Content, "left: finish with your report soon.") {
			n++
		}
	}
	return n
}

// With 10% of 20 minutes, the agent is told at the first turn at or after 18 minutes, once: "About 2
// minutes left". What was sent before is not touched, and with 0 there is no warning at all.
func TestTimeWarningFiresOnceAtTheRightMoment(t *testing.T) {
	for _, percent := range []int{10, 0} {
		clk := &clock{}
		var replies []Message
		for i := 0; i < 9; i++ {
			replies = append(replies, call(string(rune('a'+i)), "read", `{"path":"x.go"}`))
		}
		replies = append(replies, finishCall("done"))
		m := &script{replies: replies}
		cfg := Config{Model: modelFunc(func(ctx context.Context, msgs []Message, tools []ToolSpec) (Message, Usage, error) {
			clk.advance(3 * time.Minute) // each request takes three minutes
			return m.Complete(ctx, msgs, tools)
		}), Tools: []Tool{readTool}, MaxTurns: 30, Task: "go", Now: clk.now}
		cfg.Deadline = base.Add(20 * time.Minute)
		cfg.WarnAt = WarnTime(cfg.Deadline, 20*time.Minute, percent)
		Run(context.Background(), cfg, nil)
		final := m.seen[len(m.seen)-1]
		if percent == 0 {
			if !cfg.WarnAt.IsZero() || warnings(final) != 0 {
				t.Fatalf("a warning with 0 percent: %v", cfg.WarnAt)
			}
			continue
		}
		// Each request takes three minutes, so 18 are gone when the seventh is about to be sent (index 6):
		// that one is the first to carry the warning, which says 2 minutes are left.
		first := -1
		for i, msgs := range m.seen {
			if warnings(msgs) > 0 && first < 0 {
				first = i
			}
			if warnings(msgs) > 1 {
				t.Fatalf("request %d has the warning twice", i)
			}
		}
		if first != 6 || warnings(final) != 1 || last(m.seen[6]) != "About 2 minutes left: finish with your report soon." {
			t.Fatalf("the warning came at request %d: %q", first, last(m.seen[6]))
		}
		if !reflect.DeepEqual(m.seen[6][:len(m.seen[5])], m.seen[5]) {
			t.Fatal("the warning changed what came before")
		}
	}
}

// modelFunc is a model made of a function.
type modelFunc func(ctx context.Context, msgs []Message, tools []ToolSpec) (Message, Usage, error)

func (f modelFunc) Complete(ctx context.Context, msgs []Message, tools []ToolSpec) (Message, Usage, error) {
	return f(ctx, msgs, tools)
}

// Every session still working is warned: the main agent, which gets it when its call to spawn_agent
// comes back, and a sub-agent at its next turn; a sub-agent started after the moment on its first turn.
func TestTimeWarningReachesTheMainAgentAndSubAgents(t *testing.T) {
	e := newSubEnv(t)
	clk := &clock{}
	var subRequests [][]Message
	var late []Message
	subs := e.subs(func(ctx context.Context, msgs []Message, tools []ToolSpec) (Message, error) {
		if strings.Contains(msgs[1].Content, `"late"`) {
			late = append([]Message{}, msgs...)
			return finishCall("late report"), nil
		}
		subRequests = append(subRequests, append([]Message{}, msgs...))
		if len(subRequests) == 1 { // the time passes while it works
			clk.set(19 * time.Minute)
			return call("r", "read", `{"path":"x.go"}`), nil
		}
		return finishCall("report"), nil
	}, 2, 0)
	subs.Deadline, subs.Now = base.Add(20*time.Minute), clk.now
	subs.WarnAt = WarnTime(subs.Deadline, 20*time.Minute, 10)
	main := &script{replies: []Message{spawnCall("a", "one", "t"), spawnCall("b", "late", "t"), finishCall("done")}}
	cfg := Config{Model: main, Tools: []Tool{subs.Tool()}, MaxTurns: 9, Task: "go", Now: clk.now, Deadline: subs.Deadline, WarnAt: subs.WarnAt}
	tr := Run(context.Background(), cfg, nil)
	if tr.Outcome != Met {
		t.Fatalf("%s: %s", tr.Outcome, tr.Error)
	}
	if warnings(main.seen[0]) != 0 || warnings(subRequests[0]) != 0 {
		t.Fatal("warned before the time")
	}
	if len(subRequests) != 2 || warnings(subRequests[1]) != 1 || last(subRequests[1]) != "About 1 minute left: finish with your report soon." {
		t.Fatalf("the running sub-agent: %d requests", len(subRequests))
	}
	// The main agent was blocked in spawn_agent while the time passed: it is told when the call returns.
	if warnings(main.seen[1]) != 1 || warnings(main.seen[2]) != 1 {
		t.Fatalf("the main agent: %d, %d", warnings(main.seen[1]), warnings(main.seen[2]))
	}
	// One started after the moment is told on its first turn.
	if warnings(late) != 1 {
		t.Fatalf("the late sub-agent: %v", late)
	}
}

// The writing session has its own time and the same share of it as its warning.
func TestWriterGetsItsOwnTimeWarning(t *testing.T) {
	clk := &clock{}
	var writerSeen [][]Message
	model := modelFunc(func(ctx context.Context, msgs []Message, tools []ToolSpec) (Message, Usage, error) {
		clk.advance(2 * time.Minute)
		switch {
		case strings.Contains(msgs[1].Content, "Your research is done"):
			writerSeen = append(writerSeen, append([]Message{}, msgs...))
			if len(writerSeen) < 7 {
				return call(string(rune('a'+len(writerSeen))), "scratchpad_index", `{}`), Usage{}, nil
			}
			return finishCall("written"), Usage{}, nil
		case strings.HasPrefix(msgs[len(msgs)-1].Content, "Before your report is written"):
			return Message{Role: "assistant"}, Usage{}, nil
		}
		return finishCall("research"), Usage{}, nil
	})
	wtools := []Tool{readTool}
	w := Writer{Config: Config{Model: model, MaxTurns: 20, Tools: wtools}, Conv: NewWriterConversation("m", 1, "s", wtools), Intro: "Do it.", Time: 10 * time.Minute, WarnPercent: 10}
	tr := RunClosing(context.Background(), Config{Model: model, Task: "go", MaxTurns: 9, Pad: &memPad{}, Now: clk.now}, NewConversation("m", 1, "s", nil), w)
	if tr.Report != "written" {
		t.Fatalf("%q %s", tr.Report, tr.Error)
	}
	// The writer starts at 4 minutes (research and save turn took two each); it is warned when 9 of its 10
	// minutes have gone, at the first turn after that: its sixth request (clock 4+2*5 = 14, 10 gone).
	var firstWarned []int
	for i, msgs := range writerSeen {
		if warnings(msgs) > 0 {
			firstWarned = append(firstWarned, i)
		}
	}
	if len(firstWarned) == 0 || firstWarned[0] != 5 || warnings(writerSeen[len(writerSeen)-1]) != 1 {
		t.Fatalf("warned at %v of %d requests", firstWarned, len(writerSeen))
	}
}
