package store

import (
	"database/sql"
	"errors"
	"time"
)

// StoredWorkflow is a workspace's workflow of one kind, as stored. Graph is JSON, checked by
// package workflow before it is saved.
type StoredWorkflow struct {
	Graph     string
	Version   int  // 0 when the workflow has never been saved
	Live      bool // the poller starts runs from it by itself
	UpdatedBy string
	UpdatedAt time.Time
}

// ErrStaleWorkflow means the workflow was saved by someone else since the editor loaded it.
var ErrStaleWorkflow = errors.New("the workflow changed since it was loaded")

// Workflow returns a workspace's workflow of a kind; an empty one, version 0, if never saved.
func (s *Store) Workflow(workspaceID int64, kind string) (*StoredWorkflow, error) {
	var w StoredWorkflow
	var at int64
	err := s.db.QueryRow(`SELECT w.graph, w.version, w.live, `+authorName+`, w.updated_at FROM workflows w
		LEFT JOIN users u ON u.id = w.updated_by WHERE w.workspace_id = ? AND w.kind = ?`, workspaceID, kind).
		Scan(&w.Graph, &w.Version, &w.Live, &w.UpdatedBy, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return &StoredWorkflow{Graph: `{"nodes":[],"links":[]}`}, nil
	}
	if err != nil {
		return nil, err
	}
	w.UpdatedAt = time.Unix(at, 0)
	return &w, nil
}

// SaveWorkflow stores a workflow made from version base and returns the new version. If someone
// else saved it after base, nothing is stored and ErrStaleWorkflow is returned.
func (s *Store) SaveWorkflow(workspaceID int64, kind string, base int, graph string, actor *User) (int, error) {
	return s.SaveWorkflowRenaming(workspaceID, kind, base, graph, actor, nil)
}

// KeyRename says that the output kept under the name Old (and its parts, Old.report and so on) is
// kept under New from now on.
type KeyRename struct {
	Old, New string
	Keep     bool // a value already kept under New stays, and the one under Old is dropped
}

// SaveWorkflowRenaming is SaveWorkflow, and in the same transaction the values kept for the cases of
// this kind of item (pull requests, or issues) under the old names of renamed tags move to the new
// ones, so the next run finds them. A nil actor is Autobot itself.
func (s *Store) SaveWorkflowRenaming(workspaceID int64, kind string, base int, graph string, actor *User, renames []KeyRename) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var cur int
	err = tx.QueryRow(`SELECT version FROM workflows WHERE workspace_id = ? AND kind = ?`, workspaceID, kind).Scan(&cur)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if cur != base {
		return 0, ErrStaleWorkflow
	}
	next := cur + 1
	var by sql.NullInt64
	if actor != nil {
		by = sql.NullInt64{Int64: actor.ID, Valid: true}
	}
	if _, err := tx.Exec(`INSERT INTO workflows (workspace_id, kind, graph, version, updated_by, updated_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (workspace_id, kind) DO UPDATE SET graph = excluded.graph, version = excluded.version,
		updated_by = excluded.updated_by, updated_at = excluded.updated_at`, workspaceID, kind, graph, next, by, now()); err != nil {
		return 0, err
	}
	for _, r := range renames {
		if err := renameCaseKeys(tx, workspaceID, kind, r.Old, r.New, r.Keep); err != nil {
			return 0, err
		}
	}
	return next, tx.Commit()
}
