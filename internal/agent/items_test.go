package agent

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eidonix-ai/autobot/internal/store"
)

func itemTool(t *testing.T, tools []Tool, name string) func(args string) (string, error) {
	t.Helper()
	for _, tl := range tools {
		if tl.Spec.Name == name {
			return func(args string) (string, error) { return tl.Run(context.Background(), json.RawMessage(args)) }
		}
	}
	t.Fatalf("no tool %s", name)
	return nil
}

// liveIndex is a workspace's copy that can also fetch one item "from GitHub": a fake that counts its asks.
type liveIndex struct {
	*store.ItemIndex
	asked []int
	items map[int]*store.ItemDetail
}

func (x *liveIndex) Fetch(_ context.Context, number int) (*store.ItemDetail, error) {
	x.asked = append(x.asked, number)
	if d, ok := x.items[number]; ok {
		return d, nil
	}
	return nil, errors.New("GitHub has no such item")
}

// The item tools read the agent's own workspace's copy, in readable form, and never another's.
func TestItemTools(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	admin, _ := st.CreateUser("admin", "", "password-1234", true)
	a, _ := st.CreateWorkspace("o/a", admin, "", "")
	b, _ := st.CreateWorkspace("o/b", admin, "", "")
	at := time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)
	st.SaveItem(a.ID, store.Item{Number: 3, Kind: "pr", Title: "Retry uploads", Body: "Bounds the retry loop.", State: "open", Author: "bob", Labels: []string{"bug"}, Created: at, Updated: at},
		[]store.ItemLink{{To: 1, How: "fixes"}})
	st.SaveItem(a.ID, store.Item{Number: 1, Kind: "issue", Title: "Uploads hang", State: "open", Author: "alice", Created: at, Updated: at}, nil)
	st.SaveFiles(a.ID, 3, []store.ItemFile{{Path: "upload.go", Status: "modified", Additions: 3, Deletions: 1}})
	st.SaveComments(a.ID, []store.ItemComment{
		{Kind: "review_comment", ID: 2, Number: 3, Author: "dana", Body: "bound this", Path: "upload.go", Line: 12, Created: at.Add(time.Hour), Updated: at},
		{Kind: "comment", ID: 1, Number: 3, Author: "carol", Body: "first!", Created: at, Updated: at}})
	st.SaveItem(b.ID, store.Item{Number: 3, Kind: "pr", Title: "Retry uploads in B", State: "open", Author: "zed", Created: at, Updated: at}, nil)

	live := &liveIndex{ItemIndex: st.ItemIndex(a.ID), items: map[int]*store.ItemDetail{
		42: {Remote: true, Item: store.Item{Number: 42, Kind: "issue", Title: "Ancient report", Body: "from long ago", State: "closed", Author: "old", Created: at, Updated: at, Closed: at}},
	}}
	tools := ItemTools(live)
	out, err := itemTool(t, tools, "read_item")(`{"number":3}`)
	if err != nil {
		t.Fatal(err)
	}
	first, second := strings.Index(out, "first!"), strings.Index(out, "inline comment by dana on upload.go:12")
	if !strings.Contains(out, "#3 pull request (open) Retry uploads") || !strings.Contains(out, "upload.go (modified, +3 -1)") || first < 0 || second < first ||
		!strings.Contains(out, "#1 issue (open) Uploads hang [fixes]") || strings.Contains(out, "in B") {
		t.Fatalf("read_item:\n%s", out)
	}
	out, err = itemTool(t, tools, "search_items")(`{"query":"retries","state":"open"}`)
	if err != nil || !strings.Contains(out, "#3 pull request (open) Retry uploads, by bob") || strings.Contains(out, "in B") {
		t.Fatalf("search_items: %q %v", out, err)
	}
	out, err = itemTool(t, tools, "items_touching")(`{"path":"upload.go"}`)
	if err != nil || !strings.Contains(out, "#3 (open) Retry uploads") {
		t.Fatalf("items_touching: %q %v", out, err)
	}
	if out, err = itemTool(t, tools, "related_items")(`{"number":3}`); err != nil || !strings.Contains(out, "#1") {
		t.Fatalf("related_items: %q %v", out, err)
	}
	if len(live.asked) != 0 {
		t.Fatalf("GitHub was asked about items the copy has: %v", live.asked)
	}
	// An item the copy does not keep is fetched from GitHub; a short record is too.
	out, err = itemTool(t, tools, "read_item")(`{"number":42}`)
	if err != nil || !strings.Contains(out, "Ancient report") || !strings.Contains(out, "read live from GitHub") || len(live.asked) != 1 {
		t.Fatalf("read_item of an old number: %q %v (asked %v)", out, err, live.asked)
	}
	st.SaveItem(a.ID, store.Item{Number: 50, Kind: "issue", Title: "Closed long ago", State: "closed", Created: at, Updated: at, Closed: at}, nil)
	st.Prune(a.ID, at.Add(60*24*time.Hour))
	if out, err = itemTool(t, tools, "read_item")(`{"number":50}`); err != nil || !strings.Contains(out, "Closed long ago") || !strings.Contains(out, "short record") {
		t.Fatalf("a short record falls back on what it has when GitHub has no answer: %q %v", out, err)
	}
	if _, err = itemTool(t, tools, "read_item")(`{"number":99}`); err == nil || !strings.Contains(err.Error(), "#99") {
		t.Fatalf("a number neither the copy nor GitHub has should be said plainly: %v", err)
	}
	// Each tool says what the copy covers.
	for _, tl := range tools {
		if !strings.Contains(tl.Spec.Description, "30 days") && !strings.Contains(tl.Spec.Description, "180 days") {
			t.Errorf("%s does not say how far back it reaches: %s", tl.Spec.Name, tl.Spec.Description)
		}
	}
	if _, err = itemTool(t, tools, "search_items")(`{"query":"???"}`); err == nil || !strings.Contains(err.Error(), "word") {
		t.Fatalf("a query without words: %v", err)
	}
}
