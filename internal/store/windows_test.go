package store

import (
	"reflect"
	"testing"
)

// A permitted model's context window is kept with it: saved, read back, kept when the models are
// saved again, gone with the model, and never set for a model that is not permitted.
func TestModelWindows(t *testing.T) {
	st := open(t)
	admin := mustUser(t, st, "admin", true)
	p, err := st.CreateProvider(ProviderEdit{Name: "P", Kind: KindOpenAI, Endpoint: "https://x.example/v1"}, admin)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetProviderModels(p.ID, []string{"flash", "glm"}); err != nil {
		t.Fatal(err)
	}
	if w := st.ModelWindow(p.ID, "flash"); w != 0 {
		t.Fatalf("a window nobody set: %d", w)
	}
	if err := st.SetModelWindows(p.ID, map[string]int{"flash": 1_000_000, "glm": 200_000, "not-permitted": 5000}); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.ModelWindows(p.ID); !reflect.DeepEqual(got, map[string]int{"flash": 1_000_000, "glm": 200_000}) {
		t.Fatalf("%v", got)
	}
	// Saving the models again keeps the windows of those that stay.
	if err := st.SetProviderModels(p.ID, []string{"flash", "other"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.ModelWindows(p.ID); !reflect.DeepEqual(got, map[string]int{"flash": 1_000_000}) {
		t.Fatalf("after saving the models: %v", got)
	}
	if st.ModelWindow(p.ID, "flash") != 1_000_000 || st.ModelWindow(p.ID, "glm") != 0 {
		t.Fatal("ModelWindow")
	}
	// 0 clears one.
	st.SetModelWindows(p.ID, map[string]int{"flash": 0})
	if got, _ := st.ModelWindows(p.ID); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	if err := st.SetModelWindows(p.ID, map[string]int{"flash": -5}); err == nil {
		t.Fatal("a negative window was accepted")
	}
}
