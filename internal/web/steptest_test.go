package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// llmServer is a provider: chat completions that read the diff and then finish, and /decisions.
func llmServer(t *testing.T) (*httptest.Server, func() []string) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, string(body))
		mu.Unlock()
		switch r.URL.Path {
		case "/v1/decisions":
			io.WriteString(w, `{"answers":[{"choice":"yes","probabilities":[{"value":"yes","probability":0.8},{"value":"no","probability":0.2}]}]}`)
		case "/v1/chat/completions":
			var req struct{ Messages []struct{ Role, Content string } }
			json.Unmarshal(body, &req)
			reply := `{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"pr_diff","arguments":"{}"}}]}`
			if req.Messages[len(req.Messages)-1].Role == "tool" {
				reply = `{"role":"assistant","content":"","tool_calls":[{"id":"c2","type":"function","function":{"name":"finish",` +
					`"arguments":"{\"status\":\"met\",\"report\":\"**upload.go**: the loop never ends.\"}"}}]}`
			}
			fmt.Fprintf(w, `{"choices":[{"message":%s}],"usage":{"prompt_tokens":100,"completion_tokens":20}}`, reply)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string{}, seen...) }
}

// A shift-click test runs one block with the values given for its tags, and writes nothing.
func TestBlockTests(t *testing.T) {
	e := newEnv(t)
	derek := e.user("derek", true)
	ws, _ := e.st.CreateWorkspace("o/r", derek, "", "")
	other, _ := e.st.CreateWorkspace("o/r2", derek, "", "")
	llm, seen := llmServer(t)
	p, _ := e.st.CreateProvider(store.ProviderEdit{Name: "LLM", Kind: store.KindOpenAI, Endpoint: llm.URL + "/v1", Key: "k"}, derek)
	e.st.SetProviderModels(p.ID, []string{"m"})
	e.st.SetProviderRoutes(p.ID, true, false)
	e.st.LendProvider(p, []int64{ws.ID}, derek)
	model := workflow.ModelRef{Provider: p.ID, Model: "m"}
	a := workflow.DefaultAgent(workflow.PR)
	a.Model, a.Internet = model, false
	d := workflow.DefaultDecision(workflow.PR)
	d.Model = model
	c := workflow.DefaultCaseEntry()
	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	g := workflow.Graph{Nodes: []workflow.Node{
		{ID: "ag", Type: workflow.Agent, Name: "Reviewer", Settings: raw(a)},
		{ID: "de", Type: workflow.Decision, Settings: raw(d)},
		{ID: "ce", Type: workflow.CaseEntry, Settings: raw(c)},
	}, Links: []workflow.Link{}}
	if _, err := e.st.SaveWorkflow(ws.ID, workflow.PR, 0, string(raw(g)), derek); err != nil {
		t.Fatal(err)
	}
	b := e.signIn("derek")
	tok := b.csrf()
	base := "/w/o/r/workflows/pr/nodes/"

	// The form asks for each tag the block uses, with the workspace's own values filled in.
	if code, body := b.get(base + "ag/test"); code != 200 || !strings.Contains(body, `name="tag_pr_link"`) || !strings.Contains(body, `name="tag_pr_body"`) {
		t.Fatalf("agent form %d:\n%s", code, body)
	}

	// An agent run reads the pull request through its tools and finishes with its report.
	code, loc, _ := b.post(base+"ag/test", url.Values{"csrf": {tok}, "tag_pr_link": {"https://github.com/o/r/pull/7"},
		"tag_pr_title": {"Retry uploads"}, "tag_pr_body": {"x"}})
	if code != http.StatusSeeOther || !strings.HasPrefix(loc, "/w/o/r/agent-runs/") {
		t.Fatalf("start: %d %q", code, loc)
	}
	var body string
	for i := 0; i < 100; i++ {
		_, body = b.get(loc)
		if !strings.Contains(body, `http-equiv="refresh"`) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(body, "Goal met") || !strings.Contains(body, "<strong>upload.go</strong>") || !strings.Contains(body, "200 tokens in") {
		t.Fatalf("run page:\n%s", body)
	}
	all := strings.Join(seen(), "\n")
	if !strings.Contains(all, `retry()`) || !strings.Contains(all, "Done when:") || !strings.Contains(all, "correctness first") {
		t.Fatal("the model did not get the diff, the goal or the persona")
	}
	// A run belongs to its workspace.
	if code, _ := b.get(strings.Replace(loc, "/w/o/r/", "/w/o/r2/", 1)); code != http.StatusNotFound {
		t.Fatalf("another workspace's run: %d", code)
	}
	_ = other

	// A decision answers in the form.
	if code, _, body := b.post(base+"de/test", url.Values{"csrf": {tok}, "tag_pr_title": {"t"}}); code != 200 || !strings.Contains(body, "<strong>yes</strong>") || !strings.Contains(body, "80.0 %") {
		t.Fatalf("decision %d:\n%s", code, body)
	}
	// A case entry shows what it would write, and writes nothing.
	if code, _, body := b.post(base+"ce/test", url.Values{"csrf": {tok}, "tag_previous_output": {"All good."}}); code != 200 || !strings.Contains(body, "All good.") {
		t.Fatalf("case entry %d:\n%s", code, body)
	}
	if n, _ := e.st.CaseCount(ws.ID); n != 0 {
		t.Fatal("a test wrote a case file")
	}
}
