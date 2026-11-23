package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/eidonix-ai/autobot/internal/store"
)

// panicPad is a scratchpad that panics.
type panicPad struct{}

func (panicPad) Add(store.PadEntry) (int, error) { panic("the pad is broken (write)") }
func (panicPad) List() ([]store.PadEntry, error) { panic("the pad is broken (read)") }

// A panic in one of the harness's own tools (the scratchpad, compact) is an error answer the agent
// reads, like a panic in any other tool, and the run goes on.
func TestHarnessToolPanicIsAnErrorAnswer(t *testing.T) {
	m := &script{replies: []Message{
		calls(call("1", "scratchpad_write", `{"title":"t","importance":"low","description":"d","body":"b"}`), call("2", "scratchpad_index", `{}`),
			call("3", "scratchpad_read", `{"entries":[1]}`), call("4", "scratchpad_search", `{"pattern":"x"}`), call("5", "compact", `{"carry_forward":"on"}`)),
		finishCall("done"),
	}}
	conv := NewConversation("m", 1, "s", nil)
	tr := Run(context.Background(), Config{Model: m, MaxTurns: 9, Task: "go", Pad: panicPad{}}, conv)
	if tr.Outcome != Met || tr.Report != "done" {
		t.Fatalf("%s: %s", tr.Outcome, tr.Error)
	}
	var got []string
	for _, msg := range m.seen[1] {
		if msg.Role == "tool" {
			got = append(got, msg.Content)
		}
	}
	if len(got) != 5 {
		t.Fatalf("%d answers", len(got))
	}
	for i, want := range []string{"write", "read", "read", "read", ""} {
		if i < 4 && !strings.Contains(got[i], "error: the tool failed: the pad is broken ("+want+")") {
			t.Errorf("answer %d: %q", i+1, got[i])
		}
	}
	// The compact call itself was answered, and the conversation was not compacted (its pad could not
	// be read), so what the agent did is still there.
	if !strings.HasSuffix(got[4], "Compacting.") || len(m.seen[1]) < 8 {
		t.Fatalf("compact: %q, %d messages", got[4], len(m.seen[1]))
	}
	// The save turn, too.
	SaveTurn(context.Background(), Config{Model: &script{replies: []Message{call("s", "scratchpad_write", `{"title":"t","body":"b"}`)}}, Pad: panicPad{}}, conv, tr)
	if got := lastResult(conv); !strings.Contains(got, "error: the tool failed") {
		t.Fatalf("save turn: %q", got)
	}
}
