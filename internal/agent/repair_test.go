package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// A run cut off in the middle of a turn leaves tool calls with no result; the next run answers
// them before it adds its message, or the provider would refuse the conversation.
func TestInterruptedTurnIsRepaired(t *testing.T) {
	conv := NewConversation("m", 1, "s", []Tool{bigTool("long", 10)})
	conv.add(Message{Role: "user", Content: "go"})
	conv.add(calls(call("a", "long", `{}`), call("b", "long", `{}`)))
	conv.add(Message{Role: "tool", ToolCallID: "a", Content: "result of a"})
	cs := newChatServer(t, call("f", "finish", `{"status":"met","report":"done"}`))
	Run(context.Background(), Config{Model: cs.model(""), Tools: []Tool{bigTool("long", 10)}, MaxTurns: 3, Task: "again"}, conv)
	var req struct{ Messages []Message }
	json.Unmarshal([]byte(cs.bodies[0]), &req)
	var roles []string
	for _, m := range req.Messages {
		roles = append(roles, m.Role+":"+m.ToolCallID)
	}
	if got := strings.Join(roles, " "); got != "system: user: assistant: tool:a tool:b user:" {
		t.Fatalf("%s", got)
	}
}
