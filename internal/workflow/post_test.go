package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

// A Post needs a message and at least one place to post; its card says where.
func TestPostSettings(t *testing.T) {
	ok := PostSettings{Message: "Hi", GitHub: true, Trial: true, Discord: true}
	if err := ok.Check(); err != nil {
		t.Fatal(err)
	}
	for s, want := range map[PostSettings]string{
		{GitHub: true}:  "write the message",
		{Message: "Hi"}: "tick where to post",
		{Message: "Hi", Trial: true, Summary: true}: "tick where to post", // a trial and a summary are no destination
	} {
		if err := s.Check(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: %v, want %q", s, err, want)
		}
	}
	d := DefaultPost()
	if !d.GitHub || !d.Trial || d.Discord || d.Message != "" {
		t.Errorf("a new Post: %+v", d)
	}
	form := PostFromForm(func(k string) string {
		return map[string]string{"message": " Hello ", "github": "on", "discord": "on", "summary": "on"}[k]
	})
	if form != (PostSettings{Message: "Hello", GitHub: true, Discord: true, Summary: true}) {
		t.Errorf("from the form: %+v", form)
	}
	for s, want := range map[PostSettings]string{
		{GitHub: true, Trial: true, Discord: true}: "GitHub (trial) · Discord",
		{GitHub: true}:                 "GitHub",
		{Discord: true, Summary: true}: "Discord (summary)",
		{}:                             "Tick where to post",
	} {
		if got := s.Where(); got != want {
			t.Errorf("%+v: %q, want %q", s, got, want)
		}
		if got := Summary(Node{Type: Post, Settings: mustJSON(s)}, PR, "", nil); got != want {
			t.Errorf("the card of %+v: %q, want %q", s, got, want)
		}
	}
}

func mustJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

// A graph saved with the old GitHub Comment and Post to Discord blocks loads with Post nodes that
// have the destination each had, and the name each showed; the palette offers Post only; Post is an endpoint.
func TestOldPostingBlocksBecomePosts(t *testing.T) {
	saved := `{"nodes":[{"id":"e","type":"pr_ready","x":0,"y":0},
		{"id":"c","type":"github_comment","x":1,"y":1,"settings":{"message":"Hi {pr:title}","trial":false,"keep_one":true}},
		{"id":"n","type":"github_comment","name":"Our comment","x":1,"y":1},
		{"id":"d","type":"discord_post","x":2,"y":2,"settings":{"message":"There","summary":true}},
		{"id":"u","type":"discord_post","x":2,"y":2}],
		"links":[{"from":"e","out":"out","to":"c","in":"in"}]}`
	var g Graph
	if err := json.Unmarshal([]byte(saved), &g); err != nil {
		t.Fatal(err)
	}
	if err := g.Check(PR); err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		name string
		s    PostSettings
	}{
		"c": {"GitHub Comment", PostSettings{Message: "Hi {pr:title}", GitHub: true}},
		"n": {"Our comment", PostSettings{Message: "{input.report}", GitHub: true, Trial: true}}, // never saved: what it had
		"d": {"Post to Discord", PostSettings{Message: "There", Discord: true, Summary: true}},
		"u": {"Post to Discord", PostSettings{Message: "{input.report}", Discord: true}},
	}
	for _, n := range g.Nodes {
		w, ok := want[n.ID]
		if !ok {
			continue
		}
		s, err := SettingsOf(n, DefaultPost())
		if n.Type != Post || s != w.s || DisplayName(n) != w.name || err != nil {
			t.Errorf("%s: %s %q %+v, want %q %+v", n.ID, n.Type, DisplayName(n), s, w.name, w.s)
		}
		if HasOutput(n.Type) {
			t.Errorf("%s keeps an output", n.ID)
		}
	}
	// Saved again, it is a Post node, and loads the same.
	b, _ := json.Marshal(g)
	if strings.Contains(string(b), "github_comment") || strings.Contains(string(b), "discord_post") {
		t.Errorf("the old types are written again: %s", b)
	}
	// The palette offers Post and neither old block.
	var labels []string
	for _, ty := range TypesFor(PR) {
		labels = append(labels, ty.Label)
	}
	if got := strings.Join(labels, ","); !strings.Contains(got, "Post") || strings.Contains(got, "GitHub Comment") || strings.Contains(got, "Post to Discord") {
		t.Errorf("the palette: %s", got)
	}
	// A run from before that kept one in its snapshot can still name it.
	if ty, ok := TypeOf("github_comment"); !ok || ty.Label != "GitHub Comment" {
		t.Errorf("the old type: %+v %v", ty, ok)
	}
}
