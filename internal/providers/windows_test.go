package providers

import (
	"context"
	"testing"
)

// A model's context window is read from the list when the provider reports it: vLLM's max_model_len,
// or context_length or context_window; and not otherwise.
func TestDiscoverReadsContextWindows(t *testing.T) {
	srv, _ := serve(t, `{"data":[{"id":"vllm","max_model_len":131072},{"id":"router","context_length":200000},{"id":"other","context_window":65536},
		{"id":"none"},{"id":"zero","max_model_len":0}]}`)
	got, err := Discover(context.Background(), "openai", srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"vllm": 131072, "router": 200000, "other": 65536, "none": 0, "zero": 0}
	for _, m := range got {
		if m.Window != want[m.ID] {
			t.Errorf("%s: window %d, want %d", m.ID, m.Window, want[m.ID])
		}
	}
	if len(got) != len(want) {
		t.Errorf("%d models", len(got))
	}
}
