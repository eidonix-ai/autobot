package store

import (
	"strings"
	"time"
)

// The Pull requests page's reading of the local copy: one kind of item at a time, open ones first, then
// those closed lately, each with whether it has a case file and its labels. Like every reader of the
// local copy it is scoped to the ItemIndex's workspace.

// ItemRecent is how long after closing an item still shows on the page.
const ItemRecent = 30 * 24 * time.Hour

// ItemPage selects what the page lists.
type ItemPage struct {
	Kind    string    // "pr" or "issue"
	Query   string    // words to find in titles, descriptions and comments (all of them); empty: no search
	State   string    // "open", "closed" (closed or merged, within ItemRecent) or "all"
	HasCase bool      // only items with a real case file
	Limit   int       // 1 to 500; 200 when left out
	Now     time.Time // the present when zero; for tests
}

// ItemRow is one item on the page.
type ItemRow struct {
	Number  int
	Kind    string
	Title   string
	State   string // open, closed or merged
	Draft   bool
	Author  string
	Labels  []string
	Updated time.Time
	Slim    bool // only the short record is kept: no description or discussion
	HasCase bool // a real case file exists (a dry run's never counts)
}

// Page lists the items that match, open ones first, then those closed within ItemRecent, each most
// recently updated first. more says there were more than the limit.
func (x *ItemIndex) Page(q ItemPage) (rows []ItemRow, more bool, err error) {
	if q.Limit < 1 || q.Limit > 500 {
		q.Limit = 200
	}
	now := q.Now
	if now.IsZero() {
		now = time.Now()
	}
	var w strings.Builder
	args := []any{x.ws, q.Kind}
	switch q.State {
	case "closed":
		w.WriteString(" AND i.state != 'open'")
	case "all":
	default:
		w.WriteString(" AND i.state = 'open'")
	}
	// Closed items are kept in the list for ItemRecent after they close.
	w.WriteString(` AND (i.state = 'open' OR (CASE WHEN i.closed_at > 0 THEN i.closed_at WHEN i.merged_at > 0 THEN i.merged_at ELSE i.updated_at END) >= ?)`)
	args = append(args, now.Add(-ItemRecent).Unix())
	if strings.TrimSpace(q.Query) != "" {
		match, err := matchQuery(q.Query, false, false)
		if err != nil {
			return nil, false, err
		}
		w.WriteString(` AND (i.id IN (SELECT rowid FROM item_fts WHERE item_fts MATCH ?)
			OR i.number IN (SELECT c.number FROM item_comments c WHERE c.workspace_id = i.workspace_id AND c.id IN (SELECT rowid FROM comment_fts WHERE comment_fts MATCH ?)))`)
		args = append(args, match, match)
	}
	if q.HasCase {
		w.WriteString(" AND " + hasCase)
	}
	res, err := x.s.db.Query(`SELECT i.number, i.kind, i.title, i.state, i.merged, i.draft, i.author, i.updated_at, i.slim, `+hasCase+`
		FROM items i WHERE i.workspace_id = ? AND i.kind = ?`+w.String()+
		` ORDER BY (i.state = 'open') DESC, i.number DESC LIMIT ?`, append(args, q.Limit+1)...)
	if err != nil {
		return nil, false, err
	}
	defer res.Close()
	for res.Next() {
		var r ItemRow
		var merged bool
		var updated int64
		if err := res.Scan(&r.Number, &r.Kind, &r.Title, &r.State, &merged, &r.Draft, &r.Author, &updated, &r.Slim, &r.HasCase); err != nil {
			return nil, false, err
		}
		r.State, r.Updated = stateOf(r.State, merged), fromUnix(updated)
		rows = append(rows, r)
	}
	if err := res.Err(); err != nil {
		return nil, false, err
	}
	if len(rows) > q.Limit {
		rows, more = rows[:q.Limit], true
	}
	if len(rows) == 0 {
		return rows, more, nil
	}
	// Labels, for just these items.
	byNumber := map[int]*ItemRow{}
	args = []any{x.ws}
	marks := make([]string, len(rows))
	for i := range rows {
		byNumber[rows[i].Number] = &rows[i]
		marks[i] = "?"
		args = append(args, rows[i].Number)
	}
	ls, err := x.s.db.Query(`SELECT number, label FROM item_labels WHERE workspace_id = ? AND number IN (`+strings.Join(marks, ",")+`) ORDER BY label`, args...)
	if err != nil {
		return nil, false, err
	}
	defer ls.Close()
	for ls.Next() {
		var n int
		var l string
		if err := ls.Scan(&n, &l); err != nil {
			return nil, false, err
		}
		if r := byNumber[n]; r != nil {
			r.Labels = append(r.Labels, l)
		}
	}
	return rows, more, ls.Err()
}

// hasCase is true for an item with a real case file: a dry run's case is kept apart (its number
// column is minus its run's id) and never counts.
const hasCase = `EXISTS (SELECT 1 FROM cases c WHERE c.workspace_id = i.workspace_id AND c.number = i.number AND c.dry_run_of = 0)`

// RunBrief is the part of a workflow run the Pull requests page shows.
type RunBrief struct {
	ID      int64
	Status  string
	DryRun  bool
	Started time.Time
}

// LastRuns returns the latest workflow run on each of the numbers (of one kind) that has any, and
// whether any run on them is still going.
func (s *Store) LastRuns(workspaceID int64, kind string, numbers []int) (map[int]RunBrief, bool, error) {
	out := map[int]RunBrief{}
	if len(numbers) == 0 {
		return out, false, nil
	}
	marks := make([]string, len(numbers))
	args := []any{workspaceID, kind}
	for i, n := range numbers {
		marks[i] = "?"
		args = append(args, n)
	}
	in := strings.Join(marks, ",")
	rows, err := s.db.Query(`SELECT w.item, w.id, w.status, w.dry_run, w.started_at FROM workflow_runs w
		WHERE w.id IN (SELECT MAX(id) FROM workflow_runs WHERE workspace_id = ? AND kind = ? AND item IN (`+in+`) GROUP BY item)`, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var n int
		var b RunBrief
		var started int64
		if err := rows.Scan(&n, &b.ID, &b.Status, &b.DryRun, &started); err != nil {
			return nil, false, err
		}
		b.Started = time.Unix(started, 0)
		out[n] = b
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	var going int
	err = s.db.QueryRow(`SELECT COUNT(*) FROM (SELECT 1 FROM workflow_runs WHERE workspace_id = ? AND kind = ? AND status IN ('running', 'queued') AND item IN (`+in+`) LIMIT 1)`, args...).Scan(&going)
	return out, going > 0, err
}
