package store

import (
	"testing"
	"time"
)

// Page lists open items first, most recently updated first, then those closed within 30 days; a short
// record shows with what it has; a dry run's case is not the item's case.
func TestItemPage(t *testing.T) {
	st, a, b := twoWorkspaces(t)
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	hours := func(h int) time.Time { return now.Add(-time.Duration(h) * time.Hour) }
	add := func(ws int64, n int, state string, updated time.Time, labels ...string) {
		t.Helper()
		it := Item{Number: n, Kind: "pr", Title: "Widget " + state, State: state, Author: "alice", Labels: labels, Created: hours(24 * 90), Updated: updated}
		if state == "closed" {
			it.Closed = updated
		}
		if err := st.SaveItem(ws, it, nil); err != nil {
			t.Fatal(err)
		}
	}
	add(a.ID, 1, "open", hours(30))
	add(a.ID, 2, "open", hours(2), "bug")
	add(a.ID, 3, "closed", hours(24*5))
	add(a.ID, 4, "closed", hours(24*31)) // beyond the window
	add(a.ID, 5, "closed", hours(1))     // closed a moment ago: after the open ones all the same
	add(b.ID, 9, "open", hours(1))       // another workspace's
	if err := st.SaveItem(a.ID, Item{Number: 6, Kind: "issue", Title: "An issue", State: "open", Updated: hours(1)}, nil); err != nil {
		t.Fatal(err)
	}
	same := func(got []ItemRow, want ...int) {
		t.Helper()
		var g []int
		for _, r := range got {
			g = append(g, r.Number)
		}
		if len(g) != len(want) {
			t.Fatalf("got %v, want %v", g, want)
		}
		for i := range g {
			if g[i] != want[i] {
				t.Fatalf("got %v, want %v", g, want)
			}
		}
	}
	x := st.ItemIndex(a.ID)
	rows, _, err := x.Page(ItemPage{Kind: "pr", State: "all", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	same(rows, 2, 1, 5, 3)
	if rows[0].Labels[0] != "bug" || rows[2].State != "closed" {
		t.Errorf("rows: %+v", rows)
	}
	rows, _, _ = x.Page(ItemPage{Kind: "pr", State: "open", Now: now})
	same(rows, 2, 1)
	rows, _, _ = x.Page(ItemPage{Kind: "pr", State: "closed", Now: now})
	same(rows, 5, 3)

	// A short record shows with what it has: no body, still its title, state, author and labels.
	if _, err := st.db.Exec(`UPDATE items SET slim = 1, body = '' WHERE workspace_id = ? AND number = 3`, a.ID); err != nil {
		t.Fatal(err)
	}
	rows, _, _ = x.Page(ItemPage{Kind: "pr", State: "closed", Now: now})
	if len(rows) != 2 || rows[1].Number != 3 || !rows[1].Slim || rows[1].Title == "" || rows[1].Author != "alice" {
		t.Errorf("a short record: %+v", rows)
	}

	// The limit says whether there was more.
	rows, more, _ := x.Page(ItemPage{Kind: "pr", State: "all", Limit: 2, Now: now})
	if len(rows) != 2 || !more {
		t.Errorf("limit: %d rows, more %v", len(rows), more)
	}

	// Cases: the item's real case counts, a dry run's does not.
	if _, _, err := st.OpenCase(a, 1, "pr", "Widget", "Opened"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.OpenDryCase(a, 77, 2, "pr", "Widget", "Opened"); err != nil {
		t.Fatal(err)
	}
	rows, _, _ = x.Page(ItemPage{Kind: "pr", State: "all", HasCase: true, Now: now})
	same(rows, 1)
	rows, _, _ = x.Page(ItemPage{Kind: "pr", State: "all", Now: now})
	for _, r := range rows {
		if r.HasCase != (r.Number == 1) {
			t.Errorf("#%d has case %v", r.Number, r.HasCase)
		}
	}
}
