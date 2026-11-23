package store

import (
	"database/sql"
	"errors"
	"time"
)

// A kept item has a gap when it holds fewer comments than GitHub says it has: ones older than what
// the sync asks for were never read, or it was only just kept.
const gapSQL = `i.slim = 0 AND (
	(SELECT COUNT(*) FROM item_comments c WHERE c.workspace_id = i.workspace_id AND c.number = i.number AND c.kind = 'comment') < i.comments_expected OR
	(SELECT COUNT(*) FROM item_comments c WHERE c.workspace_id = i.workspace_id AND c.number = i.number AND c.kind = 'review_comment') < i.review_comments_expected)`

// CommentGap says whether an item kept in full holds fewer conversation comments, or fewer inline
// review comments, than GitHub says it has.
func (s *Store) CommentGap(ws int64, number int) (issue, review bool, err error) {
	err = s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM item_comments c WHERE c.workspace_id = i.workspace_id AND c.number = i.number AND c.kind = 'comment') < i.comments_expected,
		(SELECT COUNT(*) FROM item_comments c WHERE c.workspace_id = i.workspace_id AND c.number = i.number AND c.kind = 'review_comment') < i.review_comments_expected
		FROM items i WHERE i.workspace_id = ? AND i.number = ? AND i.slim = 0`, ws, number).Scan(&issue, &review)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	return
}

// NextCommentGap is the lowest-numbered item above after that has a comment gap.
func (s *Store) NextCommentGap(ws int64, after int) (int, bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT i.number FROM items i WHERE i.workspace_id = ? AND i.number > ? AND `+gapSQL+` ORDER BY i.number LIMIT 1`, ws, after).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return n, err == nil, err
}

// Pruned is what one pruning did.
type Pruned struct {
	Slimmed, Deleted int
}

// pruneBatchSize is how many items one transaction handles, so that pruning never holds the database long.
const pruneBatchSize = 20

// closedAt is when an item closed or merged, or failing that when it last changed.
const closedAt = `COALESCE(NULLIF(closed_at, 0), updated_at)`

// Prune ages a workspace's items: one closed or merged longer ago than KeepInFull becomes a slim row
// (its description, comments, reviews, inline comments, links and index entries are deleted), and one
// closed longer ago than KeepSlim is deleted with everything of it. Open items are never touched.
func (s *Store) Prune(ws int64, now time.Time) (Pruned, error) {
	var p Pruned
	remove := func(tx querier, number int) error {
		for _, table := range []string{"items", "item_labels", "item_comments", "item_files"} {
			if _, err := tx.Exec(`DELETE FROM `+table+` WHERE workspace_id = ? AND number = ?`, ws, number); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`DELETE FROM item_links WHERE workspace_id = ? AND from_number = ?`, ws, number)
		return err
	}
	slim := func(tx querier, number int) error {
		if _, err := tx.Exec(`UPDATE items SET slim = 1, body = '', comments_expected = 0, review_comments_expected = 0 WHERE workspace_id = ? AND number = ?`, ws, number); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM item_comments WHERE workspace_id = ? AND number = ?`, ws, number); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM item_links WHERE workspace_id = ? AND from_number = ?`, ws, number)
		return err
	}
	for {
		n, err := s.pruneBatch(ws, `state = 'closed' AND `+closedAt+` < ?`, now.Add(-KeepSlim).Unix(), remove)
		p.Deleted += n
		if err != nil {
			return p, err
		}
		if n == 0 {
			break
		}
	}
	for {
		n, err := s.pruneBatch(ws, `state = 'closed' AND slim = 0 AND `+closedAt+` < ?`, now.Add(-KeepInFull).Unix(), slim)
		p.Slimmed += n
		if err != nil {
			return p, err
		}
		if n == 0 {
			break
		}
	}
	return p, nil
}

// pruneBatch runs do on up to pruneBatchSize of the items matching where (which takes one argument),
// in one transaction, and returns how many it handled.
func (s *Store) pruneBatch(ws int64, where string, arg int64, do func(tx querier, number int) error) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT number FROM items WHERE workspace_id = ? AND `+where+` ORDER BY number LIMIT ?`, ws, arg, pruneBatchSize)
	if err != nil {
		return 0, err
	}
	var numbers []int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return 0, err
		}
		numbers = append(numbers, n)
	}
	rows.Close()
	for _, n := range numbers {
		if err := do(tx, n); err != nil {
			return 0, err
		}
	}
	return len(numbers), tx.Commit()
}
