package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

// A published page is the value of a tag at the moment a template linked to it with [[tag]]: kept for
// the case it belongs to, served at an address that is a random uuid, to anyone who may read the
// case. The same content of the same tag of the same case is one page, so a template that links to an
// unchanged value again gets the same address; a changed value is a new page, and the old link still
// shows what it showed. Pages go with their case, and on the retention schedule (PrunePages).
const schemaPublished = `
CREATE TABLE published (
	uuid         TEXT    PRIMARY KEY,
	workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	case_uuid    TEXT    NOT NULL REFERENCES cases(uuid) ON DELETE CASCADE,
	item         INTEGER NOT NULL,
	tag          TEXT    NOT NULL,
	content      TEXT    NOT NULL,
	hash         TEXT    NOT NULL,
	created_at   INTEGER NOT NULL,
	UNIQUE (case_uuid, tag, hash)
);
CREATE INDEX published_by_item ON published (workspace_id, item);
`

// Page is a published page.
type Page struct {
	UUID        string
	WorkspaceID int64
	CaseUUID    string
	Item        int
	Tag         string
	Content     string
	CreatedAt   time.Time
}

// Publish keeps content as a page of the case under the tag's name, and returns the page's uuid: the
// existing page when the case already has this tag with exactly this content.
func (s *Store) Publish(c *Case, tag, content string) (string, error) {
	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])
	id := newUUID()
	if _, err := s.db.Exec(`INSERT INTO published (uuid, workspace_id, case_uuid, item, tag, content, hash, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (case_uuid, tag, hash) DO NOTHING`,
		id, c.WorkspaceID, c.UUID, c.Number, tag, content, hash, now()); err != nil {
		return "", err
	}
	var uuid string
	err := s.db.QueryRow(`SELECT uuid FROM published WHERE case_uuid = ? AND tag = ? AND hash = ?`, c.UUID, tag, hash).Scan(&uuid)
	return uuid, err
}

// schemaStandalonePages stores published pages not attached to a GitHub case (such as pages
// published from a Discord workflow via [[step.part|Label]] or the publish_page tool).
const schemaStandalonePages = `
CREATE TABLE standalone_pages (
	uuid         TEXT    PRIMARY KEY,
	workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	title        TEXT    NOT NULL,
	content      TEXT    NOT NULL,
	created_at   INTEGER NOT NULL
);
CREATE INDEX standalone_pages_by_created ON standalone_pages (created_at);
`

// PublishStandalone stores content as a standalone published page for workspaceID and returns its uuid.
func (s *Store) PublishStandalone(workspaceID int64, title, content string) (string, error) {
	id := newUUID()
	if _, err := s.db.Exec(`INSERT INTO standalone_pages (uuid, workspace_id, title, content, created_at)
		VALUES (?, ?, ?, ?, ?)`, id, workspaceID, title, content, now()); err != nil {
		return "", err
	}
	return id, nil
}

// PublishedPage looks a page up by its uuid (case-attached or standalone), or ErrNotFound.
func (s *Store) PublishedPage(uuid string) (*Page, error) {
	p := &Page{}
	var at int64
	err := s.db.QueryRow(`SELECT uuid, workspace_id, case_uuid, item, tag, content, created_at FROM published WHERE uuid = ?`, uuid).
		Scan(&p.UUID, &p.WorkspaceID, &p.CaseUUID, &p.Item, &p.Tag, &p.Content, &at)
	if errors.Is(err, sql.ErrNoRows) {
		err = s.db.QueryRow(`SELECT uuid, workspace_id, title, content, created_at FROM standalone_pages WHERE uuid = ?`, uuid).
			Scan(&p.UUID, &p.WorkspaceID, &p.Tag, &p.Content, &at)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
	}
	if err != nil {
		return nil, err
	}
	p.CreatedAt = time.Unix(at, 0)
	return p, nil
}

// PagesOf lists a case's published pages, oldest first.
func (s *Store) PagesOf(caseUUID string) ([]Page, error) {
	rows, err := s.db.Query(`SELECT uuid, workspace_id, case_uuid, item, tag, content, created_at FROM published WHERE case_uuid = ? ORDER BY created_at, rowid`, caseUUID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Page
	for rows.Next() {
		var p Page
		var at int64
		if err := rows.Scan(&p.UUID, &p.WorkspaceID, &p.CaseUUID, &p.Item, &p.Tag, &p.Content, &at); err != nil {
			return nil, err
		}
		p.CreatedAt = time.Unix(at, 0)
		out = append(out, p)
	}
	return out, rows.Err()
}

// PrunePages deletes the pages whose keeping has run out, as of at: case files are never deleted, so
// a page goes 30 days (KeepConversation) after its item closed, as a conversation does; where the
// local copy of items does not know the item, 30 days after the page was made. A page of an open
// item stays, and so does one of an item the local copy may not have read yet (complete says whether a
// workspace's copy is complete). It returns how many it deleted.
func (s *Store) PrunePages(at time.Time, complete func(ws int64) bool) (int, error) {
	cutoff := at.Add(-KeepConversation).Unix()
	rows, err := s.db.Query(`SELECT p.uuid, p.workspace_id, p.created_at, COALESCE(i.state, ''), COALESCE(MAX(i.closed_at, i.merged_at), 0)
		FROM published p LEFT JOIN items i ON i.workspace_id = p.workspace_id AND i.number = p.item AND p.item != 0`)
	if err != nil {
		return 0, err
	}
	var gone []string
	for rows.Next() {
		var id, state string
		var ws, made, closed int64
		if err := rows.Scan(&id, &ws, &made, &state, &closed); err != nil {
			rows.Close()
			return 0, err
		}
		end := made
		switch {
		case state == "open":
			continue
		case state == "" && !complete(ws):
			continue
		case state != "":
			end = max(made, closed)
		}
		if end < cutoff {
			gone = append(gone, id)
		}
	}
	rows.Close()
	for _, id := range gone {
		if _, err := s.db.Exec(`DELETE FROM published WHERE uuid = ?`, id); err != nil {
			return 0, err
		}
	}
	res, err := s.db.Exec(`DELETE FROM standalone_pages WHERE created_at < ?`, cutoff)
	if err != nil {
		return len(gone), err
	}
	extra, _ := res.RowsAffected()
	return len(gone) + int(extra), nil
}
