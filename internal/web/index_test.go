package web

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/eidonix-ai/autobot/internal/github"
	"github.com/eidonix-ai/autobot/internal/index"
)

// indexFake is a repository with two issues.
type indexFake struct{ fakeGitHub }

func (indexFake) ListItems(_ context.Context, _, _ string, q github.ItemQuery) ([]github.ItemRecord, error) {
	if q.Page > 1 {
		return nil, nil
	}
	at := time.Now().Add(-48 * time.Hour)
	return []github.ItemRecord{
		{Number: 1, Kind: "issue", Title: "Uploads hang", State: "open", Author: "alice", Created: at, Updated: at},
		{Number: 2, Kind: "issue", Title: "Docs typo", State: "closed", Author: "bob", Created: at, Updated: at},
	}, nil
}

// An agent gets the item tools only for a workspace whose copy is complete, and only that
// workspace's; the settings page tells how the copy stands.
func TestItemToolsAndSettingsLine(t *testing.T) {
	e := newEnvWith(t, indexFake{})
	admin := e.user("derek", true)
	a, err := e.st.CreateWorkspace("o/r", admin, "flare-autobot", "tok-good")
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.st.CreateWorkspace("o/r2", admin, "flare-autobot", "tok-good")
	if err != nil {
		t.Fatal(err)
	}
	br := e.signIn("derek")
	if code, body := br.get("/w/o/r/settings"); code != 200 || !strings.Contains(body, "Copying from GitHub: starting") {
		t.Fatalf("settings before the copy: %d %s", code, body)
	}
	if got := e.ui.itemTools(a); len(got) != 0 {
		t.Fatal("item tools given before the copy was complete")
	}

	sy := index.New(e.st, indexFake{})
	sy.Pace = 0
	for more := true; more; {
		var err error
		if more, err = sy.Step(context.Background(), *a, "tok-good"); err != nil {
			t.Fatal(err)
		}
	}
	tools := e.ui.itemTools(a)
	if len(tools) != 4 {
		t.Fatalf("a finished copy should give 4 tools, got %d", len(tools))
	}
	for _, tl := range tools {
		if tl.Spec.Name == "search_items" {
			if out, err := tl.Run(context.Background(), json.RawMessage(`{"query":"hang"}`)); err != nil || !strings.Contains(out, "#1 issue (open) Uploads hang") {
				t.Fatalf("the tools read the wrong copy: %q %v", out, err)
			}
		}
	}
	if got := e.ui.itemTools(b); len(got) != 0 {
		t.Fatal("another workspace's agent got tools for a copy it does not have")
	}
	if code, body := br.get("/w/o/r/settings"); code != 200 || !strings.Contains(body, "2 items (0 pull requests, 2 issues, 1 open; 0 older ones kept as short records) and 0 comments") ||
		!strings.Contains(body, "Last synced") {
		t.Fatalf("settings after the copy: %d %s", code, body)
	}
}
