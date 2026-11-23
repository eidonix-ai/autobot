package agent

import (
	"encoding/json"

	"github.com/eidonix-ai/autobot/internal/store"
)

// Turn is what one model call cost, as the conversation's file records it.
type Turn struct{ In, Out, Cached int }

// Shown is a conversation's file read for display.
type Shown struct {
	System   string
	Messages []Message    // as they were first written: later prunes are not applied; a compaction's new messages follow
	Usage    map[int]Turn // what the call that produced an assistant message cost, by message index
}

// Show reads a conversation's file for display.
func Show(events []store.ConvEvent) *Shown {
	s := &Shown{Usage: map[int]Turn{}}
	for _, e := range events {
		switch e.T {
		case "header":
			s.System = e.System
		case "msg":
			var m Message
			json.Unmarshal(e.M, &m)
			s.Messages = append(s.Messages, m)
		case "compact": // what the conversation was compacted to is shown after what it had
			for _, raw := range e.Msgs {
				var m Message
				json.Unmarshal(raw, &m)
				s.Messages = append(s.Messages, m)
			}
		case "turn":
			s.Usage[len(s.Messages)-1] = Turn{e.In, e.Out, e.Cached}
		}
	}
	return s
}
