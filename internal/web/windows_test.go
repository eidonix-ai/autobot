package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// The provider page has one box for context windows, "model tokens" a line, prefilled with what is
// known (saved, or just discovered); saving it sets them for the permitted models; a bad line is
// refused with nothing changed; a model left out of the box has none.
func TestContextWindowsBox(t *testing.T) {
	e := newEnv(t)
	e.user("derek", true)
	admin := e.signIn("derek")
	tok := admin.csrf()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"vendor/small","max_model_len":131072},{"id":"vendor/large"}]}`))
	}))
	t.Cleanup(srv.Close)
	_, loc, _ := admin.post("/providers", url.Values{"csrf": {tok}, "preset": {"custom"}, "name": {"Mine"}, "endpoint": {srv.URL}})
	path := loc[:strings.Index(loc, "?")]
	p, _ := e.st.Provider(1)

	// What the provider says is in the box after discovery; nothing is saved by looking.
	_, _, body := admin.post(path+"/discover", url.Values{"csrf": {tok}})
	if !strings.Contains(body, `name="windows"`) || !strings.Contains(body, "Context windows (tokens)") || !strings.Contains(body, ">vendor/small 131072\n</textarea>") {
		t.Fatalf("the box after discovery:\n%s", body)
	}
	if w := e.st.ModelWindow(p.ID, "vendor/small"); w != 0 {
		t.Fatalf("looking saved a window: %d", w)
	}
	save := func(windows string) (int, string) {
		code, _, body := admin.post(path+"/models", url.Values{"csrf": {tok}, "model": {"vendor/small", "vendor/large"}, "windows": {windows}})
		return code, body
	}
	if code, body := save("vendor/small 131,072\nvendor/large 200k\nnot/permitted 5000\n\n"); code != http.StatusSeeOther {
		t.Fatalf("save: %d\n%s", code, body)
	}
	got, _ := e.st.ModelWindows(p.ID)
	if len(got) != 2 || got["vendor/small"] != 131072 || got["vendor/large"] != 200000 {
		t.Fatalf("saved %v", got)
	}
	if _, body := admin.get(path); !strings.Contains(body, ">vendor/large 200000\nvendor/small 131072\n</textarea>") {
		t.Fatalf("the box after saving:\n%s", body)
	}
	// A line that is not "model tokens" is refused, its text comes back, nothing changes.
	for _, bad := range []string{"vendor/small lots", "vendor/small", "vendor/small 0", "vendor/small 12 13", "vendor/small 999999999"} {
		code, body := save(bad)
		if code != http.StatusBadRequest || !strings.Contains(body, "is not a context window") || !strings.Contains(body, bad) {
			t.Fatalf("%q: %d", bad, code)
		}
		if got, _ := e.st.ModelWindows(p.ID); len(got) != 2 {
			t.Fatalf("a refused save changed the windows: %v", got)
		}
	}
	// The box is the whole set: a model it does not name has no window.
	if code, _ := save("vendor/small 64k"); code != http.StatusSeeOther {
		t.Fatal(code)
	}
	if got, _ := e.st.ModelWindows(p.ID); len(got) != 1 || got["vendor/small"] != 64000 {
		t.Fatalf("%v", got)
	}
}

// An agent run is given its model's window: past 85% of it the agent is told to compact, with a
// window too small for even the system prompt that is at once, and with none set it is 128,000.
func TestAgentRunUsesTheModelsWindow(t *testing.T) {
	c := newContEnv(t, nil)
	c.saveGraph(append(append([]workflow.Node{}, events...), c.agentNode("A", "Reviewer", nil)), [2]string{"ev1", "A"})
	c.run("ev1")
	if first := c.llm.requests()[0]; strings.Contains(first, "Your context is at") {
		t.Fatalf("with no window set:\n%.300s", first)
	}
	if err := c.st.SetModelWindows(c.prov.ID, map[string]int{"m": 1000}); err != nil {
		t.Fatal(err)
	}
	c.run("ev1")
	second := c.llm.requests()[1]
	if !strings.Contains(second, "Your context is at") || !strings.Contains(second, "then call compact.") {
		t.Fatalf("with a window of 1,000 tokens:\n%.300s", second)
	}
	if got := requestTools(t, second); strings.Join(got, ",") != "update_plan,compact,scratchpad_write" {
		t.Fatalf("offered %v", got)
	}
}

// requestTools are the tools a request offers, in order.
func requestTools(t *testing.T, body string) []string {
	t.Helper()
	var req struct {
		Tools []struct {
			Function struct{ Name string }
		}
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, tl := range req.Tools {
		out = append(out, tl.Function.Name)
	}
	return out
}

// At start, a permitted model without a window gets the one its provider's model list gives; a
// window someone set is kept.
func TestWindowsFilledAtStart(t *testing.T) {
	e := newEnv(t)
	derek := e.user("derek", true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"big","context_window":1048576},{"id":"set","context_window":1048576},{"id":"silent"}]}`))
	}))
	t.Cleanup(srv.Close)
	p, err := e.st.CreateProvider(store.ProviderEdit{Name: "P", Kind: store.KindOpenAI, Endpoint: srv.URL, Key: "k"}, derek)
	if err != nil {
		t.Fatal(err)
	}
	e.st.SetProviderModels(p.ID, []string{"big", "set", "silent"})
	e.st.SetModelWindows(p.ID, map[string]int{"set": 64000})
	e.ui.CheckRoutes(context.Background())
	if got, _ := e.st.ModelWindows(p.ID); len(got) != 2 || got["big"] != 1048576 || got["set"] != 64000 {
		t.Fatalf("windows %v", got)
	}
}
