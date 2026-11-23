package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eidonix-ai/autobot/internal/store"
)

// toolsOf is the tools array of a request body, as sent.
func toolsOf(t *testing.T, body string) string {
	t.Helper()
	_, rest, ok := strings.Cut(body, `"tools":`)
	if !ok {
		t.Fatalf("no tools in %.200s", body)
	}
	arr, _, ok := strings.Cut(rest, `,"messages":`)
	if !ok || len(arr) < 100 {
		t.Fatalf("tools array %.100s", arr)
	}
	return arr
}

// counted is a tool that counts how often it runs.
func counted(runs *atomic.Int32) Tool {
	return Tool{Spec: ToolSpec{Name: "long", Description: "reads", Parameters: schema(`{"type":"object","properties":{}}`)},
		Run: func(context.Context, json.RawMessage) (string, error) { runs.Add(1); return "ran", nil }}
}

// lastResult is the content of the last tool message of a conversation.
func lastResult(c *Conversation) string {
	for i := len(c.Messages) - 1; i >= 0; i-- {
		if c.Messages[i].Role == "tool" {
			return c.Messages[i].Content
		}
	}
	return ""
}

// A turn that allows only some tools still sends the whole tools array, byte for byte as the turn before
// (a different array makes the provider start over on the whole request), says which are allowed, and
// answers a call to any other with an error without running it.
func TestNarrowedTurnsKeepTheToolsAndRefuseOthers(t *testing.T) {
	t.Run("last turn", func(t *testing.T) {
		var runs atomic.Int32
		cs := newChatServer(t, call("a", "long", `{}`), call("b", "long", `{}`), call("c", "long", `{}`))
		conv := NewConversation("m", 1, "s", []Tool{counted(&runs)})
		tr := Run(context.Background(), Config{Model: cs.model(""), Tools: []Tool{counted(&runs)}, MaxTurns: 3, Task: "go", Pad: &memPad{}}, conv)
		if tr.Outcome != Budget || runs.Load() != 2 {
			t.Fatalf("%s, the tool ran %d times", tr.Outcome, runs.Load())
		}
		if toolsOf(t, cs.bodies[2]) != toolsOf(t, cs.bodies[1]) || toolsOf(t, cs.bodies[1]) != toolsOf(t, cs.bodies[0]) {
			t.Fatal("the tools array of the last turn is not the one before")
		}
		if got := lastResult(conv); got != "[result 3] error: only finish is available on this turn" {
			t.Fatalf("%q", got)
		}
	})
	t.Run("forced compact", func(t *testing.T) {
		var runs atomic.Int32
		cs := newChatServer(t, call("a", "long", `{}`), calls(call("b", "long", `{}`), call("w", "scratchpad_write", `{"title":"t","importance":"low","description":"d","body":"b"}`), call("f", "finish", `{"status":"met","report":"x"}`)), finishCall("ok"))
		cs.usage = `{"prompt_tokens":9000,"completion_tokens":5}`
		conv := NewConversation("m", 1, "s", []Tool{counted(&runs)})
		var events []store.ConvEvent
		pad := &memPad{}
		tr := Run(context.Background(), Config{Model: cs.model(""), Tools: []Tool{counted(&runs)}, MaxTurns: 9, Window: 10_000, Task: "go", Pad: pad,
			Save: func(*Transcript) { events = append(events, conv.Take()...) }}, conv)
		events = append(events, conv.Take()...)
		if tr.Outcome != Met || runs.Load() != 1 || len(pad.entries) != 1 {
			t.Fatalf("%s: the tool ran %d times, %d notes: %s", tr.Outcome, runs.Load(), len(pad.entries), tr.Error)
		}
		if toolsOf(t, cs.bodies[1]) != toolsOf(t, cs.bodies[0]) {
			t.Fatal("the tools array of the forced turn is not the one before")
		}
		var answers []string
		for _, m := range Show(events).Messages {
			if m.Role == "tool" {
				answers = append(answers, m.Content)
			}
		}
		want := []string{"[result 2] error: only scratchpad_write, update_plan and compact are available on this turn", "[result 3] Saved as entry 1.", "[result 4] error: only scratchpad_write, update_plan and compact are available on this turn\n[context: 9k of 10k tokens]"}
		if strings.Join(answers[1:4], "|") != strings.Join(want, "|") {
			t.Fatalf("answers %q", answers)
		}
	})
	t.Run("save turn", func(t *testing.T) {
		var runs atomic.Int32
		cs := newChatServer(t, finishCall("done"), calls(call("b", "long", `{}`), call("w", "scratchpad_write", `{"title":"t","importance":"low","description":"d","body":"b"}`), call("f", "finish", `{"status":"met","report":"x"}`)))
		conv := NewConversation("m", 1, "s", []Tool{counted(&runs)})
		cfg := Config{Model: cs.model(""), Tools: []Tool{counted(&runs)}, MaxTurns: 9, Task: "go", Pad: &memPad{}}
		tr := Run(context.Background(), cfg, conv)
		SaveTurn(context.Background(), cfg, conv, tr)
		if runs.Load() != 0 || len(cfg.Pad.(*memPad).entries) != 1 || len(cs.bodies) != 2 {
			t.Fatalf("the tool ran %d times, %d notes, %d requests", runs.Load(), len(cfg.Pad.(*memPad).entries), len(cs.bodies))
		}
		if toolsOf(t, cs.bodies[1]) != toolsOf(t, cs.bodies[0]) {
			t.Fatal("the tools array of the save turn is not the one before")
		}
		if got := lastResult(conv); got != "[result 4] error: only scratchpad_write is available on this turn" {
			t.Fatalf("%q", got)
		}
	})
}
