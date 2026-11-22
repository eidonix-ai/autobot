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
