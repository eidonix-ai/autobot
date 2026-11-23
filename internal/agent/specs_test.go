package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/eidonix-ai/autobot/internal/search"
)

// Every tool's definition is valid JSON, and the request that carries them can be written: one bad
// schema fails every agent run before its first turn.
func TestEveryToolDefinitionIsValid(t *testing.T) {
	searcher := func(context.Context, string, int) ([]search.Result, error) { return nil, nil }
	sets := map[string][]Tool{
		"pr":      PRTools(&PR{}),
		"web":     WebTools(searcher),
		"sandbox": SandboxTools(&fakeSession{}),
		"code":    CodeTools(&fakeCode{}, nil),
		"code pr": CodeTools(&fakeCode{}, &LocalPR{Number: 7, Head: headSHA, Base: "main"}),
		"items":   ItemTools(nil),
	}
	var specs []ToolSpec
	for name, tools := range sets {
		for _, tool := range tools {
			if !json.Valid(tool.Spec.Parameters) {
				t.Errorf("%s: %s has invalid parameters: %s", name, tool.Spec.Name, tool.Spec.Parameters)
			}
			specs = append(specs, tool.Spec)
		}
	}
	specs = append(specs, finishSpec, planSpec)
	// What the chat client sends, built the same way.
	type fn struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	}
	var ts []map[string]any
	for _, s := range specs {
		ts = append(ts, map[string]any{"type": "function", "function": fn{s.Name, s.Description, s.Parameters}})
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(map[string]any{"tools": ts}); err != nil {
		t.Fatalf("the request cannot be written: %v", err)
	}
}
