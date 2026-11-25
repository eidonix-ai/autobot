package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// WorkflowPage is a user-defined workflow page under a workspace's Large features (LFA) tab.
// Its canvas graph lives in the workflows table under kind = "lfa:" + ID.
type WorkflowPage struct {
	ID          string
	WorkspaceID int64
	Title       string
	Description string
	CreatedBy   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// WorkflowPages lists a workspace's LFA pages, oldest first.
func (s *Store) WorkflowPages(workspaceID int64) ([]WorkflowPage, error) {
	rows, err := s.db.Query(`SELECT p.id, p.workspace_id, p.title, p.description, `+authorName+`, p.created_at, p.updated_at
		FROM workflow_pages p LEFT JOIN users u ON u.id = p.created_by
		WHERE p.workspace_id = ? ORDER BY p.created_at ASC, p.id ASC`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkflowPage
	for rows.Next() {
		var p WorkflowPage
		var ca, ua int64
		if err := rows.Scan(&p.ID, &p.WorkspaceID, &p.Title, &p.Description, &p.CreatedBy, &ca, &ua); err != nil {
			return nil, err
		}
		p.CreatedAt, p.UpdatedAt = time.Unix(ca, 0), time.Unix(ua, 0)
		out = append(out, p)
	}
	return out, rows.Err()
}

// WorkflowPage returns one LFA page of a workspace, or ErrNotFound.
func (s *Store) WorkflowPage(workspaceID int64, id string) (*WorkflowPage, error) {
	var p WorkflowPage
	var ca, ua int64
	err := s.db.QueryRow(`SELECT p.id, p.workspace_id, p.title, p.description, `+authorName+`, p.created_at, p.updated_at
		FROM workflow_pages p LEFT JOIN users u ON u.id = p.created_by
		WHERE p.workspace_id = ? AND p.id = ?`, workspaceID, id).
		Scan(&p.ID, &p.WorkspaceID, &p.Title, &p.Description, &p.CreatedBy, &ca, &ua)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.CreatedAt, p.UpdatedAt = time.Unix(ca, 0), time.Unix(ua, 0)
	return &p, nil
}

func pageSlug(title string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(title)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
			b.WriteByte('-')
		}
		if b.Len() >= 28 {
			break
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		s = "page"
	}
	var rnd [3]byte
	rand.Read(rnd[:])
	return s + "-" + hex.EncodeToString(rnd[:])
}

// CreateWorkflowPage adds a new user-defined LFA workflow page and initializes its canvas with an
// initial graph (or empty graph if initialGraph is "").
func (s *Store) CreateWorkflowPage(ws *Workspace, title, description, initialGraph string, actor *User) (*WorkflowPage, error) {
	title = strings.TrimSpace(title)
	description = strings.TrimSpace(description)
	switch {
	case title == "":
		return nil, invalid("give the page a title")
	case len([]rune(title)) > 80:
		return nil, invalid("the page title is limited to 80 characters")
	case len([]rune(description)) > 400:
		return nil, invalid("the page description is limited to 400 characters")
	}
	if initialGraph == "" {
		initialGraph = `{"nodes":[],"links":[]}`
	}
	id := pageSlug(title)
	t := now()
	var by sql.NullInt64
	if actor != nil {
		by = sql.NullInt64{Int64: actor.ID, Valid: true}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO workflow_pages (id, workspace_id, title, description, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, id, ws.ID, title, description, by, t, t); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`INSERT INTO workflows (workspace_id, kind, graph, version, updated_by, updated_at)
		VALUES (?, ?, ?, 1, ?, ?)`, ws.ID, "lfa:"+id, initialGraph, by, t); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.WorkflowPage(ws.ID, id)
}

// UpdateWorkflowPage renames or edits the description of an LFA page.
func (s *Store) UpdateWorkflowPage(workspaceID int64, id, title, description string) error {
	title = strings.TrimSpace(title)
	description = strings.TrimSpace(description)
	switch {
	case title == "":
		return invalid("give the page a title")
	case len([]rune(title)) > 80:
		return invalid("the page title is limited to 80 characters")
	case len([]rune(description)) > 400:
		return invalid("the page description is limited to 400 characters")
	}
	res, err := s.db.Exec(`UPDATE workflow_pages SET title = ?, description = ?, updated_at = ? WHERE workspace_id = ? AND id = ?`,
		title, description, now(), workspaceID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteWorkflowPage removes an LFA page and its stored workflow canvas.
func (s *Store) DeleteWorkflowPage(workspaceID int64, id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`DELETE FROM workflow_pages WHERE workspace_id = ? AND id = ?`, workspaceID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(`DELETE FROM workflows WHERE workspace_id = ? AND kind = ?`, workspaceID, "lfa:"+id); err != nil {
		return err
	}
	return tx.Commit()
}
