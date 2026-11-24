package store

import (
	"strings"
	"time"
)

// Lists of items for the orientation packet: one person's, one label's, and short records by number.
// Like every reader of the local copy, they are scoped to the ItemIndex's workspace.

// ItemBrief is an item's short record.
type ItemBrief struct {
	Number  int
	Kind    string // "pr" or "issue"
	Title   string
	State   string // open, closed (not merged) or merged
	Author  string
	Created time.Time
	Updated time.Time
}

// ItemList selects items; empty fields do not filter.
type ItemList struct {
	Author       string // any case
	Label        string // any case
	CreatedSince time.Time
	OpenOnly     bool
	Limit        int // 1 to 500; 30 when left out
}

const briefColumns = `i.number, i.kind, i.title, i.state, i.merged, i.author, i.created_at, i.updated_at`

func scanBrief(r scanner) (ItemBrief, error) {
	var b ItemBrief
	var merged bool
	var created, updated int64
	if err := r.Scan(&b.Number, &b.Kind, &b.Title, &b.State, &merged, &b.Author, &created, &updated); err != nil {
		return b, err
	}
	b.State, b.Created, b.Updated = stateOf(b.State, merged), fromUnix(created), fromUnix(updated)
	return b, nil
}

// List returns the items that match, the newest created first.
func (x *ItemIndex) List(q ItemList) ([]ItemBrief, error) {
	if q.Limit < 1 || q.Limit > 500 {
		q.Limit = 30
	}
	var w strings.Builder
	args := []any{x.ws}
	if q.Author != "" {
		w.WriteString(" AND i.author = ? COLLATE NOCASE")
		args = append(args, q.Author)
	}
	if q.Label != "" {
		w.WriteString(" AND EXISTS (SELECT 1 FROM item_labels l WHERE l.workspace_id = i.workspace_id AND l.number = i.number AND l.label = ? COLLATE NOCASE)")
		args = append(args, q.Label)
	}
	if !q.CreatedSince.IsZero() {
		w.WriteString(" AND i.created_at >= ?")
		args = append(args, unix(q.CreatedSince))
	}
	if q.OpenOnly {
		w.WriteString(" AND i.state = 'open'")
	}
	rows, err := x.s.db.Query(`SELECT `+briefColumns+` FROM items i WHERE i.workspace_id = ?`+w.String()+
		` ORDER BY i.created_at DESC, i.number DESC LIMIT ?`, append(args, q.Limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ItemBrief
	for rows.Next() {
		b, err := scanBrief(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Briefs returns the short records of the numbers that are kept.
func (x *ItemIndex) Briefs(numbers []int) (map[int]ItemBrief, error) {
	out := map[int]ItemBrief{}
	for _, n := range numbers {
		b, err := scanBrief(x.s.db.QueryRow(`SELECT `+briefColumns+` FROM items i WHERE i.workspace_id = ? AND i.number = ?`, x.ws, n))
		if err == nil {
			out[n] = b
		}
	}
	return out, nil
}

// MergedFiles are the files of the pull requests an author got merged, created since the time,
// by pull request number.
func (x *ItemIndex) MergedFiles(author string, since time.Time) (map[int][]string, error) {
	rows, err := x.s.db.Query(`SELECT f.number, f.path FROM item_files f JOIN items i ON i.workspace_id = f.workspace_id AND i.number = f.number
		WHERE f.workspace_id = ? AND i.kind = 'pr' AND i.merged = 1 AND i.author = ? COLLATE NOCASE AND i.created_at >= ? ORDER BY f.number, f.path`,
		x.ws, author, unix(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int][]string{}
	for rows.Next() {
		var n int
		var p string
		if err := rows.Scan(&n, &p); err != nil {
			return nil, err
		}
		out[n] = append(out[n], p)
	}
	return out, rows.Err()
}
