package store

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Workspace groups a GitHub repository with the test hardware, case files and history that go
// with it. Its name is the repository's owner/repo.
type Workspace struct {
	ID        int64
	Name      string
	CreatedAt time.Time
}

// Path is the workspace's page in the web UI; it mirrors the GitHub path.
func (w Workspace) Path() string { return "/w/" + w.Name }

// GitHubURL is the repository on GitHub.
func (w Workspace) GitHubURL() string { return "https://github.com/" + w.Name }

var (
	ownerRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
	repoRE  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
)

// NormalizeWorkspaceName accepts owner/repo or a pasted github.com link and returns owner/repo,
// or an InputError when it is not a valid GitHub repository name.
func NormalizeWorkspaceName(name string) (string, error) {
	n := strings.TrimSpace(name)
	for _, p := range []string{"https://", "http://", "www.", "github.com/"} {
		n = strings.TrimPrefix(n, p)
	}
	n = strings.TrimSuffix(strings.TrimSuffix(n, "/"), ".git")
	owner, repo, ok := strings.Cut(n, "/")
	if !ok || !ownerRE.MatchString(owner) || !repoRE.MatchString(repo) || repo == "." || repo == ".." {
		return "", invalid("Name a workspace after its GitHub repository, as owner/repo (for example NVIDIA/hyperflare-inference).")
	}
	return owner + "/" + repo, nil
}

func scanWorkspace(row scanner) (*Workspace, error) {
	var w Workspace
	var created int64
	if err := row.Scan(&w.ID, &w.Name, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	w.CreatedAt = time.Unix(created, 0)
	return &w, nil
}

// CreateWorkspace adds a workspace and starts its charter from the template. When login is not
// empty, the GitHub identity (login and token) is saved with it in the same transaction.
func (s *Store) CreateWorkspace(name string, actor *User, login, token string) (*Workspace, error) {
	name, err := NormalizeWorkspaceName(name)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	t := now()
	res, err := tx.Exec(`INSERT INTO workspaces (name, created_at) VALUES (?, ?) ON CONFLICT (name) DO NOTHING`, name, t)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, invalid("A workspace for %s already exists.", name)
	}
	id, _ := res.LastInsertId()
	if _, err := tx.Exec(`INSERT INTO charter_revisions (workspace_id, rev, body, author_id, created_at) VALUES (?, 1, ?, ?, ?)`,
		id, CharterTemplate(name), actor.ID, t); err != nil {
		return nil, err
	}
	if err := addEvent(tx, id, actor, "created the workspace"); err != nil {
		return nil, err
	}
	if login != "" {
		if err := s.setGitHub(tx, id, login, token, actor); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Workspace{ID: id, Name: name, CreatedAt: time.Unix(t, 0)}, nil
}

// Workspace looks a workspace up by owner/repo, ignoring case as GitHub does.
func (s *Store) Workspace(name string) (*Workspace, error) {
	return scanWorkspace(s.db.QueryRow(`SELECT id, name, created_at FROM workspaces WHERE name = ?`, name))
}

// Workspaces lists the workspaces a user can open: all of them for an admin, otherwise the ones
// they are assigned to.
func (s *Store) Workspaces(u *User) ([]Workspace, error) {
	q := `SELECT id, name, created_at FROM workspaces ORDER BY name`
	var args []any
	if !u.IsAdmin {
		q = `SELECT w.id, w.name, w.created_at FROM workspaces w
			JOIN workspace_members m ON m.workspace_id = w.id AND m.user_id = ? ORDER BY w.name`
		args = append(args, u.ID)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Workspace
	for rows.Next() {
		w, err := scanWorkspace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *w)
	}
	return out, rows.Err()
}

// CanOpen reports whether a user may open a workspace.
func (s *Store) CanOpen(u *User, workspaceID int64) (bool, error) {
	if u.IsAdmin {
		return true, nil
	}
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM workspace_members WHERE workspace_id = ? AND user_id = ?`, workspaceID, u.ID).Scan(&n)
	return n > 0, err
}

// Members lists the users assigned to a workspace.
func (s *Store) Members(workspaceID int64) ([]User, error) {
	rows, err := s.db.Query(`SELECT `+userCols+` FROM users u JOIN workspace_members m ON m.user_id = u.id
		WHERE m.workspace_id = ? ORDER BY u.username`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// CharterRevision is one saved version of a workspace's charter.
type CharterRevision struct {
	Rev       int
	Body      string // empty in history listings
	Author    string // display name of whoever saved it
	CreatedAt time.Time
}

// ConflictError means a charter save was based on a revision that is no longer current.
type ConflictError struct{ Current CharterRevision }

func (e *ConflictError) Error() string {
	return fmt.Sprintf("the charter has changed: revision %d is current", e.Current.Rev)
}

const authorName = `COALESCE(NULLIF(u.display_name, ''), u.username, '')`

func charter(q querier, workspaceID int64, rev int) (*CharterRevision, error) {
	query := `SELECT c.rev, c.body, ` + authorName + `, c.created_at FROM charter_revisions c
		LEFT JOIN users u ON u.id = c.author_id WHERE c.workspace_id = ?`
	args := []any{workspaceID}
	if rev > 0 {
		query += ` AND c.rev = ?`
		args = append(args, rev)
	} else {
		query += ` ORDER BY c.rev DESC LIMIT 1`
	}
	var c CharterRevision
	var created int64
	if err := q.QueryRow(query, args...).Scan(&c.Rev, &c.Body, &c.Author, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	c.CreatedAt = time.Unix(created, 0)
	return &c, nil
}

// Charter returns revision rev of a workspace's charter, or the current one when rev is 0.
func (s *Store) Charter(workspaceID int64, rev int) (*CharterRevision, error) {
	return charter(s.db, workspaceID, rev)
}

// CharterHistory lists a charter's revisions, newest first, without their bodies.
func (s *Store) CharterHistory(workspaceID int64) ([]CharterRevision, error) {
	rows, err := s.db.Query(`SELECT c.rev, `+authorName+`, c.created_at FROM charter_revisions c
		LEFT JOIN users u ON u.id = c.author_id WHERE c.workspace_id = ? ORDER BY c.rev DESC`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CharterRevision
	for rows.Next() {
		var c CharterRevision
		var created int64
		if err := rows.Scan(&c.Rev, &c.Author, &created); err != nil {
			return nil, err
		}
		c.CreatedAt = time.Unix(created, 0)
		out = append(out, c)
	}
	return out, rows.Err()
}

// MaxCharterBytes caps one charter revision.
const MaxCharterBytes = 512 << 10

// SaveCharter stores body as the next revision of the charter and returns its number. baseRev is
// the revision the editor started from; if someone has saved since, nothing is stored and a
// *ConflictError carries the current revision. Saving an unchanged body stores nothing.
func (s *Store) SaveCharter(workspaceID int64, baseRev int, body string, actor *User) (int, error) {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	if len(body) > MaxCharterBytes {
		return 0, invalid("The charter is limited to %d KB.", MaxCharterBytes>>10)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	cur, err := charter(tx, workspaceID, 0)
	if err != nil {
		return 0, err
	}
	if cur.Rev != baseRev {
		return 0, &ConflictError{Current: *cur}
	}
	if cur.Body == body {
		return cur.Rev, nil
	}
	rev := cur.Rev + 1
	if _, err := tx.Exec(`INSERT INTO charter_revisions (workspace_id, rev, body, author_id, created_at) VALUES (?, ?, ?, ?, ?)`,
		workspaceID, rev, body, actor.ID, now()); err != nil {
		return 0, err
	}
	if err := addEvent(tx, workspaceID, actor, fmt.Sprintf("edited the charter (revision %d)", rev)); err != nil {
		return 0, err
	}
	return rev, tx.Commit()
}

// Event is one line of a workspace's history.
type Event struct {
	Actor   string
	Summary string
	At      time.Time
}

// Events returns a workspace's most recent history, newest first.
func (s *Store) Events(workspaceID int64, limit int) ([]Event, error) {
	rows, err := s.db.Query(`SELECT `+authorName+`, e.summary, e.created_at FROM events e
		LEFT JOIN users u ON u.id = e.actor_id WHERE e.workspace_id = ? ORDER BY e.id DESC LIMIT ?`, workspaceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var at int64
		if err := rows.Scan(&e.Actor, &e.Summary, &at); err != nil {
			return nil, err
		}
		e.At = time.Unix(at, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

// CharterTemplate is the starting charter for a new workspace.
func CharterTemplate(name string) string {
	return "# " + name + " project charter\n" + `
## Vision

What this project is for, and what the world looks like when it succeeds.

## Philosophy

The principles reviewers apply when the rules run out.

## Goals & Non-Goals

**Goals**

- What this project must achieve.

**Non-goals**

- What it deliberately will not do.

## Applicable Standards

Coding, testing, licensing and security standards contributions must meet.

## Competitors

Projects we measure ourselves against, and what we take from each.

## Roadmap

What is planned next, in order.
`
}

// RenameWorkspace points a workspace at a different GitHub repository name (or fixes its
// spelling) and records the change in its history.
func (s *Store) RenameWorkspace(id int64, name string, actor *User) (*Workspace, error) {
	name, err := NormalizeWorkspaceName(name)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	cur, err := scanWorkspace(tx.QueryRow(`SELECT id, name, created_at FROM workspaces WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	if cur.Name == name {
		return cur, nil
	}
	var taken bool
	if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM workspaces WHERE name = ? AND id <> ?)`, name, id).Scan(&taken); err != nil {
		return nil, err
	}
	if taken {
		return nil, invalid("Another workspace is already named %s.", name)
	}
	if _, err := tx.Exec(`UPDATE workspaces SET name = ? WHERE id = ?`, name, id); err != nil {
		return nil, err
	}
	if err := addEvent(tx, id, actor, "renamed the workspace from "+cur.Name+" to "+name); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	cur.Name = name
	return cur, nil
}

// DeleteWorkspace removes a workspace with its charter, history and assignments.
func (s *Store) DeleteWorkspace(id int64) error {
	res, err := s.db.Exec(`DELETE FROM workspaces WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetMembers replaces the set of users assigned to a workspace and records each change in its
// history.
func (s *Store) SetMembers(workspaceID int64, userIDs []int64, actor *User) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	have := map[int64]bool{}
	rows, err := tx.Query(`SELECT user_id FROM workspace_members WHERE workspace_id = ?`, workspaceID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		have[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	want := map[int64]bool{}
	for _, id := range userIDs {
		want[id] = true
	}
	name := func(id int64) (string, error) {
		u, err := scanUser(tx.QueryRow(`SELECT `+userCols+` FROM users u WHERE u.id = ?`, id))
		if err != nil {
			return "", err
		}
		return u.Name(), nil
	}
	for id := range want {
		if have[id] {
			continue
		}
		n, err := name(id)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO workspace_members (workspace_id, user_id) VALUES (?, ?)`, workspaceID, id); err != nil {
			return err
		}
		if err := addEvent(tx, workspaceID, actor, "assigned "+n); err != nil {
			return err
		}
	}
	for id := range have {
		if want[id] {
			continue
		}
		n, err := name(id)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM workspace_members WHERE workspace_id = ? AND user_id = ?`, workspaceID, id); err != nil {
			return err
		}
		if err := addEvent(tx, workspaceID, actor, "unassigned "+n); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// WorkspaceByID looks a workspace up by id.
func (s *Store) WorkspaceByID(id int64) (*Workspace, error) {
	return scanWorkspace(s.db.QueryRow(`SELECT id, name, created_at FROM workspaces WHERE id = ?`, id))
}
