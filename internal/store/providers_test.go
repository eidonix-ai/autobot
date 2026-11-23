package store

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestProviderKeyBinding(t *testing.T) {
	st := open(t)
	if err := st.UseKey(bytes.Repeat([]byte{7}, 32)); err != nil {
		t.Fatal(err)
	}
	admin := mustUser(t, st, "admin", true)
	edit := func(name, key string) ProviderEdit {
		return ProviderEdit{Name: name, Kind: KindOpenAI, Endpoint: "https://x.example/v1", Key: key}
	}
	a, err := st.CreateProvider(edit("A", " key-a "), admin)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := st.CreateProvider(edit("B", "key-b"), admin)
	none, _ := st.CreateProvider(edit("None", ""), admin)
	if k, err := st.ProviderKey(a.ID); err != nil || k != "key-a" {
		t.Fatalf("key: %q, %v", k, err)
	}
	if k, err := st.ProviderKey(none.ID); err != nil || k != "" || none.HasKey {
		t.Fatalf("no key: %q, %v, %v", k, err, none.HasKey)
	}

	// A sealed key copied to another provider's row, or taken from a workspace's GitHub token, does not open.
	ws, _ := st.CreateWorkspace("o/r", admin, "bot", "gh-token")
	for _, from := range []struct {
		sql string
		id  int64
	}{
		{`SELECT api_key FROM providers WHERE id = ?`, a.ID},
		{`SELECT token FROM workspace_github WHERE workspace_id = ?`, ws.ID},
	} {
		if _, err := st.db.Exec(`UPDATE providers SET api_key = (`+from.sql+`) WHERE id = ?`, from.id, b.ID); err != nil {
			t.Fatal(err)
		}
		if k, err := st.ProviderKey(b.ID); !errors.Is(err, ErrKeyUnreadable) || k != "" {
			t.Fatalf("a moved sealed value opened: %q, %v", k, err)
		}
	}
}

func TestProviderModelsAndChecks(t *testing.T) {
	st := open(t)
	admin := mustUser(t, st, "admin", true)
	good := ProviderEdit{Name: "OpenRouter", Kind: KindOpenAI, Endpoint: "http://10.0.0.5:8000/v1/"}
	p, err := st.CreateProvider(good, admin)
	if err != nil || p.Endpoint != "http://10.0.0.5:8000/v1" {
		t.Fatalf("create: %+v, %v", p, err)
	}
	if _, err := st.CreateProvider(ProviderEdit{Name: "openrouter", Kind: KindAnthropic, Endpoint: "https://a.example"}, admin); !isInput(err) {
		t.Fatalf("duplicate name (any case): %v", err)
	}
	for _, bad := range []ProviderEdit{
		{Name: "", Kind: KindOpenAI, Endpoint: "https://a.example"},
		{Name: "x", Kind: "other", Endpoint: "https://a.example"},
		{Name: "x", Kind: KindOpenAI, Endpoint: "a.example/v1"},
		{Name: "x", Kind: KindOpenAI, Endpoint: "ftp://a.example"},
		{Name: "x", Kind: KindOpenAI, Endpoint: "https://"},
		{Name: "x", Kind: KindOpenAI, Endpoint: "https://user:pw@a.example"},
		{Name: "x", Kind: KindOpenAI, Endpoint: "https://a.example", Key: "two words"},
	} {
		if _, err := st.CreateProvider(bad, admin); !isInput(err) {
			t.Errorf("%+v was accepted: %v", bad, err)
		}
	}

	if err := st.SetProviderModels(p.ID, []string{"b", " a ", "b"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Provider(p.ID); strings.Join(got.Models, ",") != "a,b" {
		t.Fatalf("models: %v", got.Models)
	}
	for _, bad := range []string{"two words", "", strings.Repeat("x", 201), "tab\there", "bell\a"} {
		if err := st.SetProviderModels(p.ID, []string{"keep", bad}); !isInput(err) {
			t.Errorf("model %q was accepted: %v", bad, err)
		}
	}
	if got, _ := st.Provider(p.ID); len(got.Models) != 2 {
		t.Fatalf("a refused save changed the models: %v", got.Models)
	}

	if err := st.DeleteProvider(p.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	st.db.QueryRow(`SELECT count(*) FROM provider_models`).Scan(&n)
	if n != 0 {
		t.Fatal("permitted models survived their provider")
	}
	if err := st.DeleteProvider(p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

// A model counts as a decision model when its provider's kind answers decisions or its endpoint was
// found to serve a decision route; an Anthropic provider never does.
func TestModelUses(t *testing.T) {
	st := open(t)
	admin := mustUser(t, st, "admin", true)
	ws, _ := st.CreateWorkspace("o/r", admin, "", "")
	add := func(name, kind string) *Provider {
		p, err := st.CreateProvider(ProviderEdit{Name: name, Kind: kind, Endpoint: "https://" + name + ".example/v1"}, admin)
		if err != nil {
			t.Fatal(err)
		}
		st.SetProviderModels(p.ID, []string{name + "-m"})
		st.LendProvider(p, []int64{ws.ID}, admin)
		return p
	}
	both := add("both", KindOpenAI)
	add("text", KindOpenAI)
	add("native", KindDecisions)
	claude := add("claude", KindAnthropic)
	st.SetProviderRoutes(both.ID, false, true)
	st.SetProviderRoutes(claude.ID, true, true) // ignored: Anthropic has no decision routes
	want := map[string]string{"both-m": "text, decisions", "text-m": "text", "native-m": "decisions", "claude-m": "text"}
	all, err := st.AvailableModels(ws.ID)
	if err != nil || len(all) != len(want) {
		t.Fatalf("%v %+v", err, all)
	}
	for _, a := range all {
		if a.Use() != want[a.Model] {
			t.Errorf("%s: %q, want %q", a.Model, a.Use(), want[a.Model])
		}
	}
	if p, _ := st.Provider(both.ID); p.DecisionAPI() != KindSystemOne || p.RoutesChecked.IsZero() {
		t.Errorf("provider %+v", p)
	}
}

// A search provider reaches a workspace by the same rule as a model (lent by an owner who is
// assigned to it and enabled), and never counts as a model.
func TestSearchProviders(t *testing.T) {
	st := open(t)
	admin := mustUser(t, st, "admin", true)
	alice := mustUser(t, st, "alice", false)
	st.UseKey(bytes.Repeat([]byte{7}, 32))
	ws, _ := st.CreateWorkspace("o/r", admin, "", "")
	st.SetMembers(ws.ID, []int64{alice.ID}, admin)
	p, err := st.CreateProvider(ProviderEdit{Name: "Tavily", Kind: KindTavily, Endpoint: "https://api.tavily.com", Key: "k"}, alice)
	if err != nil {
		t.Fatal(err)
	}
	st.SetProviderModels(p.ID, []string{"stray"})
	st.LendProvider(p, []int64{ws.ID}, alice)
	count := func() int {
		t.Helper()
		got, err := st.SearchProviders(ws.ID)
		if err != nil {
			t.Fatal(err)
		}
		return len(got)
	}
	if count() != 1 {
		t.Fatal("a lent search provider is missing")
	}
	if m, _ := st.AvailableModels(ws.ID); len(m) != 0 {
		t.Fatalf("a search provider counted as models: %+v", m)
	}
	st.SetMembers(ws.ID, nil, admin)
	if count() != 0 {
		t.Fatal("a search provider stayed after its owner left the workspace")
	}
	st.SetMembers(ws.ID, []int64{alice.ID}, admin)
	if err := st.UpdateUser(alice.ID, UserUpdate{DisplayName: "Alice", Disabled: true, Workspaces: []int64{ws.ID}}, admin); err != nil {
		t.Fatal(err)
	}
	if count() != 0 {
		t.Fatal("a disabled owner's search provider stayed usable")
	}
}
