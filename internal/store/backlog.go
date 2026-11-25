package store

import "time"

// BacklogCandidate is an open PR or issue with no case file and no prior non-dry workflow run.
type BacklogCandidate struct {
	Number      int
	Title       string
	Draft       bool
	Author      string
	Association string
	BaseRef     string
	Additions   int
	Deletions   int
	Body        string
	UpdatedAt   time.Time
}

// BacklogCandidates returns open, non-slim items of kind that have neither a real case file nor any
// prior non-dry workflow run of kind, ordered newest or oldest first.
func (s *Store) BacklogCandidates(wsID int64, kind, order string, limit int) ([]BacklogCandidate, error) {
	if limit <= 0 {
		limit = 50
	}
	dir := "DESC"
	if order == "oldest" {
		dir = "ASC"
	}
	rows, err := s.db.Query(`SELECT i.number, i.title, i.draft, i.author, i.association, i.base_ref, i.additions, i.deletions, i.body, i.updated_at
		FROM items i
		WHERE i.workspace_id = ? AND i.kind = ? AND i.state = 'open' AND i.slim = 0
		  AND NOT EXISTS (SELECT 1 FROM cases c WHERE c.workspace_id = i.workspace_id AND c.number = i.number AND c.dry_run_of = 0)
		  AND NOT EXISTS (SELECT 1 FROM workflow_runs r WHERE r.workspace_id = i.workspace_id AND r.kind = i.kind AND r.item = i.number AND r.dry_run = 0)
		ORDER BY i.number `+dir+` LIMIT ?`, wsID, kind, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BacklogCandidate
	for rows.Next() {
		var c BacklogCandidate
		var updated int64
		if err := rows.Scan(&c.Number, &c.Title, &c.Draft, &c.Author, &c.Association, &c.BaseRef, &c.Additions, &c.Deletions, &c.Body, &updated); err != nil {
			return nil, err
		}
		if updated != 0 {
			c.UpdatedAt = time.Unix(updated, 0)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// BacklogCount returns how many open items of kind have no real case file and no prior non-dry run.
// draftFilter: -1 any, 0 ready only, 1 drafts only.
func (s *Store) BacklogCount(wsID int64, kind string, draftFilter int) (int, error) {
	extra := ""
	switch draftFilter {
	case 0:
		extra = " AND i.draft = 0"
	case 1:
		extra = " AND i.draft = 1"
	}
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM items i
		WHERE i.workspace_id = ? AND i.kind = ? AND i.state = 'open' AND i.slim = 0`+extra+`
		  AND NOT EXISTS (SELECT 1 FROM cases c WHERE c.workspace_id = i.workspace_id AND c.number = i.number AND c.dry_run_of = 0)
		  AND NOT EXISTS (SELECT 1 FROM workflow_runs r WHERE r.workspace_id = i.workspace_id AND r.kind = i.kind AND r.item = i.number AND r.dry_run = 0)`,
		wsID, kind).Scan(&n)
	return n, err
}
