package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/eidonix-ai/autobot/internal/store"
)

// A provider reuses work it did for an earlier request only when a request starts with exactly the
// bytes of that one. So a conversation is kept append-only: what was sent is never rewritten, except
// in rare batches (shorten), which cost one cache miss each rather than one per turn. The system
// prompt and tool definitions are fixed when the conversation starts and stored with it, and every
// request of every run, resumed or not, sends them unchanged.

// Conversation is what a model has been told and has answered, across the runs of one conversation.
// Run adds to it; its events (messages, shortenings, usage) are taken out with Take and written to
// the conversation's file.
type Conversation struct {
	Model    string
	Provider int64
	Session  string // the key sent with every request, so a gateway can route the conversation to one server
	System   string
	Tools    []ToolSpec // the definitions sent, in order: finish and update_plan first
	Messages []Message  // after the system prompt; only ever added to
	Turns    int        // model calls so far

	// What the latest run that ended saw of the item, so that the next knows what is new.
	Head string
	Seen time.Time
	Read []string // the files its tools had read

	pending []store.ConvEvent
}

// Specs are the tool definitions a conversation starts with: finish and update_plan, then the tools.
func Specs(tools []Tool) []ToolSpec {
	specs := []ToolSpec{finishSpec, planSpec}
	for _, t := range tools {
		specs = append(specs, t.Spec)
	}
	return specs
}

// NewConversation starts a conversation with a model, its system prompt and the tools it is given.
func NewConversation(model string, provider int64, system string, tools []Tool) *Conversation {
	b := make([]byte, 12)
	rand.Read(b)
	c := &Conversation{Model: model, Provider: provider, Session: "autobot-" + hex.EncodeToString(b), System: system, Tools: Specs(tools)}
	specs, _ := json.Marshal(c.Tools)
	c.pending = append(c.pending, store.ConvEvent{T: "header", Model: model, Provider: provider, Session: c.Session, System: system, Tools: specs})
	return c
}

// add appends a message.
func (c *Conversation) add(m Message) {
	c.Messages = append(c.Messages, m)
	b, _ := json.Marshal(m)
	c.pending = append(c.pending, store.ConvEvent{T: "msg", M: b})
}

// Take returns the events not yet written, and forgets them.
func (c *Conversation) Take() []store.ConvEvent {
	out := c.pending
	c.pending = nil
	return out
}

// request is the conversation as sent: the system prompt, then the messages.
func (c *Conversation) request() []Message {
	return append([]Message{{Role: "system", Content: c.System}}, c.Messages...)
}

// LoadConversation rebuilds a conversation from its file's events.
func LoadConversation(events []store.ConvEvent) (*Conversation, error) {
	if len(events) == 0 || events[0].T != "header" {
		return nil, errors.New("the conversation's file has no header")
	}
	h := events[0]
	c := &Conversation{Model: h.Model, Provider: h.Provider, Session: h.Session, System: h.System}
	if len(h.Tools) > 0 {
		if err := json.Unmarshal(h.Tools, &c.Tools); err != nil {
			return nil, fmt.Errorf("the conversation's tools: %v", err)
		}
	}
	for _, e := range events[1:] {
		switch e.T {
		case "msg":
			var m Message
			if err := json.Unmarshal(e.M, &m); err != nil {
				return nil, fmt.Errorf("a message of the conversation: %v", err)
			}
			c.Messages = append(c.Messages, m)
		case "trim":
			shorten(c.Messages, e.Upto)
		case "turn":
			c.Turns++
		case "run_end":
			c.Head, c.Seen, c.Read = e.Head, time.Time{}, e.Read
			if e.Seen != 0 {
				c.Seen = time.Unix(e.Seen, 0)
			}
		}
	}
	return c, nil
}

// Turn is what one model call cost, as the conversation's file records it.
type Turn struct{ In, Out, Cached int }

// Shown is a conversation's file read for display: the messages, and what the turn that produced
// each assistant message cost.
type Shown struct {
	*Conversation
	Usage map[int]Turn // by the index of the assistant message
}

// Show reads a conversation's file for display, with each message as it was last sent.
func Show(events []store.ConvEvent) (*Shown, error) {
	c, err := LoadConversation(events)
	if err != nil {
		return nil, err
	}
	s := &Shown{Conversation: c, Usage: map[int]Turn{}}
	n := 0
	for _, e := range events[1:] {
		switch e.T {
		case "msg":
			n++
		case "turn":
			s.Usage[n-1] = Turn{e.In, e.Out, e.Cached}
		}
	}
	return s, nil
}

// maxStub is how long a tool result may be before shortening cuts it.
const maxStub = 400

// stub is what a tool result is shortened to: its first line and how much was left out.
func stub(content string) string {
	first, _, _ := strings.Cut(content, "\n")
	if len(first) > 200 {
		first = first[:200]
	}
	return strings.ToValidUTF8(first, "") + fmt.Sprintf("\n[%d characters left out to save room; call the tool again if you need them]", len(content))
}

// shorten cuts the tool results of msgs[:upto] to their stubs, and reports whether any changed.
func shorten(msgs []Message, upto int) bool {
	changed := false
	for i := range msgs[:min(upto, len(msgs))] {
		if m := msgs[i]; m.Role == "tool" && len(m.Content) > maxStub {
			msgs[i].Content = stub(m.Content)
			changed = true
		}
	}
	return changed
}

// charsPerToken is the rough size of a token, for deciding when a request has grown too large.
const charsPerToken = 3.5

// tokens estimates the size of the next request: the system prompt, the tool definitions and the
// messages, at 3.5 characters a token.
func (c *Conversation) tokens() int {
	n := len(c.System)
	for _, t := range c.Tools {
		n += len(t.Name) + len(t.Description) + len(t.Parameters)
	}
	for _, m := range c.Messages {
		n += len(m.Content)
		for _, tc := range m.ToolCalls {
			n += len(tc.Function.Name) + len(tc.Function.Arguments)
		}
	}
	return int(float64(n) / charsPerToken)
}

// shortenOld is the batch: it cuts every tool result older than the last keepTurns turns to its
// stub, for good, and records that in the conversation. It reports whether anything changed; if
// nothing did, nothing is recorded and the next request starts with the same bytes as the last.
func (c *Conversation) shortenOld() bool {
	turns, cut := 0, len(c.Messages)
	for i := len(c.Messages) - 1; i >= 0; i-- {
		if c.Messages[i].Role == "assistant" {
			if turns++; turns == keepTurns {
				cut = i
				break
			}
		}
	}
	if cut == len(c.Messages) || !shorten(c.Messages, cut) {
		return false
	}
	c.pending = append(c.pending, store.ConvEvent{T: "trim", Upto: cut})
	return true
}

// recordTurn notes what a model call cost.
func (c *Conversation) recordTurn(run int64, u Usage) {
	c.Turns++
	c.pending = append(c.pending, store.ConvEvent{T: "turn", Run: run, In: u.In, Out: u.Out, Cached: u.Cached})
}

// endRun notes that a run ended: which commit of the item it saw and when, and the files it read.
func (c *Conversation) endRun(run int64, head string, seen time.Time, read []string) {
	c.Head, c.Seen, c.Read = head, seen, read
	e := store.ConvEvent{T: "run_end", Run: run, Head: head, At: time.Now().Unix(), Read: read}
	if !seen.IsZero() {
		e.Seen = seen.Unix()
	}
	c.pending = append(c.pending, e)
}
