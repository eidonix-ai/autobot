package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/eidonix-ai/autobot/internal/discord"
	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/eidonix-ai/autobot/internal/workflow"
)

// toServer sends every request to a test server, whatever its host: Discord, here.
type toServer struct{ url *url.URL }

func (t toServer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host = t.url.Scheme, t.url.Host
	return http.DefaultTransport.RoundTrip(r)
}

func TestDiscord(t *testing.T) {
	var mu sync.Mutex
	var posts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			io.WriteString(w, `{"name":"Autobot","channel_id":"42"}`)
			return
		}
		var p struct{ Content string }
		json.NewDecoder(r.Body).Decode(&p)
		mu.Lock()
		posts = append(posts, p.Content)
		mu.Unlock()
		io.WriteString(w, `{"id":"1"}`)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	discord.UseTransport(toServer{u})
	defer discord.UseTransport(nil)

	e := newEnv(t)
	derek := e.user("derek", true)
	e.st.CreateWorkspace("o/r", derek, "", "")
	e.st.CreateWorkspace("o/r2", derek, "", "")
	b := e.signIn("derek")
	tok := b.csrf()
	const hook = "https://discord.com/api/webhooks/123456789012345678/abcdefghijklmnopqrstuvwxyz_SECRET-0123456789"
	if code, loc, _ := b.post("/w/o/r/settings/discord", url.Values{"csrf": {tok}, "name": {"reviews"}, "webhook": {hook}}); code != http.StatusSeeOther {
		t.Fatalf("add: %d %s", code, loc)
	}
	if code, body := b.get("/w/o/r/settings"); code != 200 || !strings.Contains(body, "#reviews") || strings.Contains(body, "SECRET") {
		t.Fatalf("settings page %d, or it shows the webhook", code)
	}
	ch, _ := e.st.DiscordChannels(mustWS(t, e, "o/r").ID)
	id := itoa(ch[0].ID)
	// A channel is reached only through its own workspace.
	if code, _, _ := b.post("/w/o/r2/settings/discord/"+id+"/test", url.Values{"csrf": {tok}}); code != http.StatusNotFound {
		t.Fatalf("another workspace tested the channel: %d", code)
	}
	if code, _, body := b.post("/w/o/r/settings/discord/"+id+"/test", url.Values{"csrf": {tok}}); code != 200 || !strings.Contains(body, "Test message sent") {
		t.Fatalf("test message %d", code)
	}

	// A block posts only to its own workspace's channels, and its test posts the rendered message.
	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	g := workflow.Graph{Nodes: []workflow.Node{{ID: "dc", Type: workflow.Discord, Settings: raw(workflow.DefaultDiscord(workflow.PR))}}, Links: []workflow.Link{}}
	for _, ws := range []string{"o/r", "o/r2"} {
		if _, err := e.st.SaveWorkflow(mustWS(t, e, ws).ID, workflow.PR, 0, string(raw(g)), derek); err != nil {
			t.Fatal(err)
		}
	}
	save := func(ws string) int {
		code, _, _ := b.post("/w/"+ws+"/workflows/pr/nodes/dc", url.Values{"csrf": {tok}, "version": {"1"}, "name": {"Post"},
			"channel": {id}, "message": {"**{pr_title}** is reviewed: {previous_output}"}})
		return code
	}
	if save("o/r2") != http.StatusBadRequest {
		t.Fatal("a block was pointed at another workspace's channel")
	}
	if save("o/r") != http.StatusOK {
		t.Fatal("saving the block")
	}
	code, _, body := b.post("/w/o/r/workflows/pr/nodes/dc/test", url.Values{"csrf": {tok}, "tag_pr_title": {"Retry uploads"}, "tag_previous_output": {"fine"}})
	mu.Lock()
	defer mu.Unlock()
	if code != 200 || !strings.Contains(body, "Posted to #reviews") || posts[len(posts)-1] != "**Retry uploads** is reviewed: fine" {
		t.Fatalf("%d %q\n%s", code, posts, body)
	}
}

func mustWS(t *testing.T, e *env, name string) *store.Workspace {
	t.Helper()
	ws, err := e.st.Workspace(name)
	if err != nil {
		t.Fatal(err)
	}
	return ws
}
