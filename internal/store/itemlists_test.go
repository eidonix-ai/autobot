package store

import (
	"testing"
	"time"
)

func saveAs(t *testing.T, st *Store, ws int64, it Item) {
	t.Helper()
	if err := st.SaveItem(ws, it, nil); err != nil {
		t.Fatal(err)
	}
}

// The lists behind the orientation packet: one person's items (any case), one label's, the open
// ones, only the recent; short records by number; the files of what a person got merged. None of
// them leaks another workspace.
func TestItemLists(t *testing.T) {
	st, a, b := twoWorkspaces(t)
	day := 24 * time.Hour
	saveAs(t, st, a.ID, Item{Number: 1, Kind: "pr", Title: "Old merged", State: "closed", Merged: true, Author: "Alice", Created: t0.Add(-300 * day), Updated: t0})
	saveAs(t, st, a.ID, Item{Number: 2, Kind: "pr", Title: "Recent merged", State: "closed", Merged: true, Author: "alice", Created: t0.Add(-10 * day), Updated: t0.Add(-5 * day)})
	saveAs(t, st, a.ID, Item{Number: 3, Kind: "issue", Title: "Open issue", State: "open", Author: "ALICE", Labels: []string{"Bug"}, Created: t0.Add(-2 * day), Updated: t0})
	saveAs(t, st, a.ID, Item{Number: 4, Kind: "pr", Title: "Closed unmerged", State: "closed", Author: "bob", Labels: []string{"bug"}, Created: t0.Add(-1 * day), Updated: t0})
	saveAs(t, st, b.ID, Item{Number: 5, Kind: "issue", Title: "Other workspace", State: "open", Author: "alice", Labels: []string{"bug"}, Created: t0, Updated: t0})
	st.SaveFiles(a.ID, 2, []ItemFile{{Path: "core/a.go"}, {Path: "docs/x.md"}})
	st.SaveFiles(a.ID, 1, []ItemFile{{Path: "old/o.go"}})
	st.SaveFiles(b.ID, 5, []ItemFile{{Path: "leak.go"}})
	ix := st.ItemIndex(a.ID)

	nums := func(l []ItemBrief) (out []int) {
		for _, b := range l {
			out = append(out, b.Number)
		}
		return
	}
	same := func(got []int, want ...int) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}
	l, err := ix.List(ItemList{Author: "alice"})
	if err != nil || !same(nums(l), 3, 2, 1) {
		t.Fatalf("by author, any case, newest first: %v %v", nums(l), err)
	}
	if l[1].State != "merged" || l[0].State != "open" || l[0].Kind != "issue" {
		t.Fatalf("states: %+v", l)
	}
	if l, _ := ix.List(ItemList{Author: "alice", CreatedSince: t0.Add(-180 * day)}); !same(nums(l), 3, 2) {
		t.Fatalf("only the recent: %v", nums(l))
	}
	if l, _ := ix.List(ItemList{Author: "alice", OpenOnly: true}); !same(nums(l), 3) {
		t.Fatalf("only the open: %v", nums(l))
	}
	if l, _ := ix.List(ItemList{Label: "BUG"}); !same(nums(l), 4, 3) {
		t.Fatalf("by label, this workspace only: %v", nums(l))
	}
	if l, _ := ix.List(ItemList{Limit: 2}); len(l) != 2 {
		t.Fatalf("the limit: %v", nums(l))
	}
	briefs, _ := ix.Briefs([]int{2, 4, 5, 99})
	if len(briefs) != 2 || briefs[2].Title != "Recent merged" || briefs[4].State != "closed" {
		t.Fatalf("briefs: %+v", briefs)
	}
	saveAs(t, st, a.ID, Item{Number: 6, Kind: "pr", Title: "Closed, not merged", State: "closed", Author: "alice", Created: t0.Add(-3 * day), Updated: t0}) // its files do not count
	st.SaveFiles(a.ID, 6, []ItemFile{{Path: "unmerged/u.go"}})
	files, _ := ix.MergedFiles("Alice", t0.Add(-180*day))
	if len(files) != 1 || len(files[2]) != 2 || files[2][0] != "core/a.go" {
		t.Fatalf("merged files: %+v", files)
	}
}

// Phrase finds the words together in order, Any finds one of them.
func TestSearchPhraseAndAny(t *testing.T) {
	st, a, _ := twoWorkspaces(t)
	mustItem(t, st, a.ID, 1, "issue", "Slow", "the max-batch flag is ignored")
	mustItem(t, st, a.ID, 2, "issue", "Other", "batch jobs and a max of three")
	mustItem(t, st, a.ID, 3, "issue", "Third", "nothing about it, only a flag")
	ix := st.ItemIndex(a.ID)
	count := func(q ItemSearch) int {
		h, err := ix.Search(q)
		if err != nil {
			t.Fatal(err)
		}
		return len(h)
	}
	if n := count(ItemSearch{Query: "max-batch", Phrase: true}); n != 1 {
		t.Fatalf("phrase found %d", n)
	}
	if n := count(ItemSearch{Query: "max batch"}); n != 2 {
		t.Fatalf("all words found %d", n)
	}
	if n := count(ItemSearch{Query: "batch flag", Any: true}); n != 3 {
		t.Fatalf("any word found %d", n)
	}
}
