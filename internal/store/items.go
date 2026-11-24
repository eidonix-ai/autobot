package store

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// The local copy of a workspace's pull requests and issues, with all their discussion, so that
// agents (and later people) can search and cross-reference without asking GitHub.
//
//   - items holds one row per pull request or issue, open, closed or merged. Its detail_updated is
//     the updated time of the pull request when its details (head, sizes, files, reviews) were last
//     read; a pull request is read again only when GitHub says it was updated after that.
//   - item_comments holds every conversation comment, review and inline review comment. GitHub numbers
//     the three kinds separately, so a comment is identified by its kind and id.
//   - item_files holds the files each pull request changes; item_links holds who mentions, fixes or
//     closes whom, read out of titles, bodies and comments.
//   - item_fts and comment_fts are full-text indexes (stemmed: "retries" finds "retry") kept in step
//     by triggers, so every write to items or item_comments, from wherever, updates them. The rows'
//     own integer ids are the index's row numbers, so they never move.
//   - index_state is where each workspace's backfill and sync have got to, and what went wrong.
//
// Everything is scoped by workspace_id.
//
// The copy is a lean working copy, not an archive: GitHub stays the source for anything old.
//   - An item is kept in full while it is open and for KeepInFull after it closes or merges: its
//     description, every comment, review and inline comment, its files, its links, and its text in
//     the full-text indexes.
//   - Then, until KeepSlim after it closed, it is a slim row (slim = 1): number, kind, title, state,
//     author, labels, dates and, for a pull request, its changed files, which still answer "what
//     touched this file". Its description, discussion and links are deleted and it leaves the indexes.
//   - After that all of it is deleted. Prune does this; the index runs it daily.
const schemaItems = `
CREATE TABLE items (
	id             INTEGER PRIMARY KEY,
	workspace_id   INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	number         INTEGER NOT NULL,
	kind           TEXT    NOT NULL,
	title          TEXT    NOT NULL DEFAULT '',
	body           TEXT    NOT NULL DEFAULT '',
	state          TEXT    NOT NULL,
	merged         INTEGER NOT NULL DEFAULT 0,
	draft          INTEGER NOT NULL DEFAULT 0,
	author         TEXT    NOT NULL DEFAULT '',
	association    TEXT    NOT NULL DEFAULT '',
	created_at     INTEGER NOT NULL DEFAULT 0,
	updated_at     INTEGER NOT NULL DEFAULT 0,
	closed_at      INTEGER NOT NULL DEFAULT 0,
	merged_at      INTEGER NOT NULL DEFAULT 0,
	base_ref       TEXT    NOT NULL DEFAULT '',
	head_sha       TEXT    NOT NULL DEFAULT '',
	head_ref       TEXT    NOT NULL DEFAULT '',
	additions      INTEGER NOT NULL DEFAULT 0,
	deletions      INTEGER NOT NULL DEFAULT 0,
	detail_updated INTEGER NOT NULL DEFAULT 0,
	slim           INTEGER NOT NULL DEFAULT 0,
	comments_expected        INTEGER NOT NULL DEFAULT 0,
	review_comments_expected INTEGER NOT NULL DEFAULT 0,
	UNIQUE (workspace_id, number)
);
CREATE INDEX items_by_state ON items (workspace_id, state);
CREATE TABLE item_labels (
	workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	number       INTEGER NOT NULL,
	label        TEXT    NOT NULL,
	PRIMARY KEY (workspace_id, number, label)
);
CREATE TABLE item_comments (
	id           INTEGER PRIMARY KEY,
	workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	kind         TEXT    NOT NULL,
	github_id    INTEGER NOT NULL,
	number       INTEGER NOT NULL,
	author       TEXT    NOT NULL DEFAULT '',
	association  TEXT    NOT NULL DEFAULT '',
	body         TEXT    NOT NULL DEFAULT '',
	review_state TEXT    NOT NULL DEFAULT '',
	path         TEXT    NOT NULL DEFAULT '',
	line         INTEGER NOT NULL DEFAULT 0,
	created_at   INTEGER NOT NULL DEFAULT 0,
	updated_at   INTEGER NOT NULL DEFAULT 0,
	UNIQUE (workspace_id, kind, github_id)
);
CREATE INDEX item_comments_by_item ON item_comments (workspace_id, number, created_at);
CREATE TABLE item_files (
	workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	number       INTEGER NOT NULL,
	path         TEXT    NOT NULL,
	status       TEXT    NOT NULL DEFAULT '',
	additions    INTEGER NOT NULL DEFAULT 0,
	deletions    INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (workspace_id, number, path)
);
CREATE INDEX item_files_by_path ON item_files (workspace_id, path);
CREATE TABLE item_links (
	workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	from_number  INTEGER NOT NULL,
	to_number    INTEGER NOT NULL,
	how          TEXT    NOT NULL,
	src          TEXT    NOT NULL,
	PRIMARY KEY (workspace_id, from_number, to_number, how, src)
);
CREATE INDEX item_links_to ON item_links (workspace_id, to_number);
CREATE TABLE index_state (
	workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	key          TEXT    NOT NULL,
	value        TEXT    NOT NULL,
	PRIMARY KEY (workspace_id, key)
);

CREATE VIRTUAL TABLE item_fts USING fts5(title, body, content='items', content_rowid='id', tokenize='porter unicode61');
-- A slim item is not in the index; these triggers keep it so whichever way a row changes.
CREATE TRIGGER items_fts_ai AFTER INSERT ON items BEGIN
	INSERT INTO item_fts (rowid, title, body) SELECT new.id, new.title, new.body WHERE new.slim = 0;
END;
CREATE TRIGGER items_fts_ad AFTER DELETE ON items BEGIN
	INSERT INTO item_fts (item_fts, rowid, title, body) SELECT 'delete', old.id, old.title, old.body WHERE old.slim = 0;
END;
CREATE TRIGGER items_fts_au AFTER UPDATE OF title, body, slim ON items BEGIN
	INSERT INTO item_fts (item_fts, rowid, title, body) SELECT 'delete', old.id, old.title, old.body WHERE old.slim = 0;
	INSERT INTO item_fts (rowid, title, body) SELECT new.id, new.title, new.body WHERE new.slim = 0;
END;

CREATE VIRTUAL TABLE comment_fts USING fts5(body, content='item_comments', content_rowid='id', tokenize='porter unicode61');
CREATE TRIGGER comments_fts_ai AFTER INSERT ON item_comments BEGIN
	INSERT INTO comment_fts (rowid, body) VALUES (new.id, new.body);
END;
CREATE TRIGGER comments_fts_ad AFTER DELETE ON item_comments BEGIN
	INSERT INTO comment_fts (comment_fts, rowid, body) VALUES ('delete', old.id, old.body);
END;
CREATE TRIGGER comments_fts_au AFTER UPDATE OF body ON item_comments BEGIN
	INSERT INTO comment_fts (comment_fts, rowid, body) VALUES ('delete', old.id, old.body);
	INSERT INTO comment_fts (rowid, body) VALUES (new.id, new.body);
END;
`

// How long a closed or merged item is kept in full, and how long after that as a slim row.
const (
	KeepInFull = 30 * 24 * time.Hour
	KeepSlim   = 180 * 24 * time.Hour
)

// Item is a pull request or issue as kept locally.
type Item struct {
	Number      int
	Kind        string // "pr" or "issue"
	Title       string
	Body        string
	State       string // "open" or "closed"
	Merged      bool
	Draft       bool
	Author      string
	Association string
	Labels      []string
	Created     time.Time
	Updated     time.Time
	Closed      time.Time // zero while open
	MergedAt    time.Time // zero unless merged
	Comments    int       // how many conversation comments GitHub says it has

	// Pull requests only.
	Base, HeadRef, HeadSHA string
	Additions, Deletions   int
}

// ItemLink says that the item it belongs to refers to item To: How is "mentions", "fixes" or "closes".
type ItemLink struct {
	To  int
	How string
}

// ItemComment is one conversation comment ("comment"), review ("review") or inline review comment
// ("review_comment") on an item.
type ItemComment struct {
	Kind        string
	ID          int64 // GitHub's id, unique within its kind
	Number      int   // the item it is on
	Author      string
	Association string
	Body        string
	ReviewState string // reviews: APPROVED, CHANGES_REQUESTED, COMMENTED...
	Path        string // inline comments: the file and line
	Line        int
	Created     time.Time
	Updated     time.Time
	Links       []ItemLink // what its text refers to; written, not read back
}

// ItemFile is a file a pull request changes.
type ItemFile struct {
	Path      string
	Status    string
	Additions int
	Deletions int
}

// PullInfo is what reading one pull request adds to its item.
type PullInfo struct {
	State                  string
	Merged                 bool
	MergedAt               time.Time
	Draft                  bool
	Base, HeadRef, HeadSHA string
	Additions, Deletions   int
	Updated                time.Time // the pull request's own updated time when read
	ReviewComments         int       // how many inline review comments GitHub says it has
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func fromUnix(u int64) time.Time {
	if u == 0 {
		return time.Time{}
	}
	return time.Unix(u, 0).UTC()
}

// SaveItem writes an item, kept in full, from its list fields as GitHub's list of issues gives
// them (labels and links replace what was there). A slim row that is saved again becomes full. It
// leaves a pull request's details alone: SavePull writes those.
func (s *Store) SaveItem(ws int64, it Item, links []ItemLink) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT INTO items (workspace_id, number, kind, title, body, state, merged, draft, author, association,
			created_at, updated_at, closed_at, merged_at, comments_expected)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (workspace_id, number) DO UPDATE SET kind = excluded.kind, title = excluded.title, body = excluded.body,
			state = excluded.state, merged = excluded.merged, draft = excluded.draft, author = excluded.author,
			association = excluded.association, created_at = excluded.created_at, updated_at = excluded.updated_at,
			closed_at = excluded.closed_at, merged_at = excluded.merged_at, comments_expected = excluded.comments_expected, slim = 0`,
		ws, it.Number, it.Kind, it.Title, it.Body, it.State, it.Merged, it.Draft, it.Author, it.Association,
		unix(it.Created), unix(it.Updated), unix(it.Closed), unix(it.MergedAt), it.Comments)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM item_labels WHERE workspace_id = ? AND number = ?`, ws, it.Number); err != nil {
		return err
	}
	for _, l := range it.Labels {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO item_labels (workspace_id, number, label) VALUES (?, ?, ?)`, ws, it.Number, l); err != nil {
			return err
		}
	}
	if err := replaceLinks(tx, ws, it.Number, "item", links); err != nil {
		return err
	}
	return tx.Commit()
}

// replaceLinks makes links the set an item's source ("item" or a comment) refers to.
func replaceLinks(tx querier, ws int64, from int, src string, links []ItemLink) error {
	if _, err := tx.Exec(`DELETE FROM item_links WHERE workspace_id = ? AND from_number = ? AND src = ?`, ws, from, src); err != nil {
		return err
	}
	for _, l := range links {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO item_links (workspace_id, from_number, to_number, how, src) VALUES (?, ?, ?, ?, ?)`,
			ws, from, l.To, l.How, src); err != nil {
			return err
		}
	}
	return nil
}

// SavePull writes what reading a pull request found, and notes the updated time it was read at.
// The pull request's item must have been saved.
func (s *Store) SavePull(ws int64, number int, p PullInfo) error {
	res, err := s.db.Exec(`UPDATE items SET state = ?, merged = ?, merged_at = ?, draft = ?, base_ref = ?, head_ref = ?, head_sha = ?,
			additions = ?, deletions = ?, detail_updated = ?, review_comments_expected = ?
		WHERE workspace_id = ? AND number = ?`,
		p.State, p.Merged, unix(p.MergedAt), p.Draft, p.Base, p.HeadRef, p.HeadSHA, p.Additions, p.Deletions, max(unix(p.Updated), 1), p.ReviewComments, ws, number)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SaveFiles replaces the files a pull request changes.
func (s *Store) SaveFiles(ws int64, number int, files []ItemFile) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM item_files WHERE workspace_id = ? AND number = ?`, ws, number); err != nil {
		return err
	}
	for _, f := range files {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO item_files (workspace_id, number, path, status, additions, deletions) VALUES (?, ?, ?, ?, ?, ?)`,
			ws, number, f.Path, f.Status, f.Additions, f.Deletions); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SaveComments adds comments or updates ones already kept (matched by kind and id). A comment on an
// item that is not kept in full (not kept at all, or a slim row) is dropped.
func (s *Store) SaveComments(ws int64, cs []ItemComment) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	full := map[int]bool{}
	for _, c := range cs {
		ok, seen := full[c.Number]
		if !seen {
			var slim bool
			err := tx.QueryRow(`SELECT slim FROM items WHERE workspace_id = ? AND number = ?`, ws, c.Number).Scan(&slim)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			ok = err == nil && !slim
			full[c.Number] = ok
		}
		if !ok {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO item_comments (workspace_id, kind, github_id, number, author, association, body, review_state, path, line, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (workspace_id, kind, github_id) DO UPDATE SET number = excluded.number, author = excluded.author,
				association = excluded.association, body = excluded.body, review_state = excluded.review_state,
				path = excluded.path, line = excluded.line, created_at = excluded.created_at, updated_at = excluded.updated_at`,
			ws, c.Kind, c.ID, c.Number, c.Author, c.Association, c.Body, c.ReviewState, c.Path, c.Line, unix(c.Created), unix(c.Updated)); err != nil {
			return err
		}
		if err := replaceLinks(tx, ws, c.Number, fmt.Sprintf("%s:%d", c.Kind, c.ID), c.Links); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ItemMeta is what the sync needs to know about an item it already keeps.
type ItemMeta struct {
	Kind          string
	State         string
	Updated       time.Time
	DetailUpdated time.Time // zero until a pull request's details have been read
	HeadSHA       string
}

// ItemMeta returns what is kept about an item, or ErrNotFound.
func (s *Store) ItemMeta(ws int64, number int) (*ItemMeta, error) {
	var m ItemMeta
	var up, det int64
	err := s.db.QueryRow(`SELECT kind, state, updated_at, detail_updated, head_sha FROM items WHERE workspace_id = ? AND number = ?`, ws, number).
		Scan(&m.Kind, &m.State, &up, &det, &m.HeadSHA)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	m.Updated, m.DetailUpdated = fromUnix(up), fromUnix(det)
	return &m, nil
}

// NextUnreadPull is the lowest-numbered pull request whose details have never been read.
func (s *Store) NextUnreadPull(ws int64) (int, bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT number FROM items WHERE workspace_id = ? AND kind = 'pr' AND slim = 0 AND detail_updated = 0 ORDER BY number LIMIT 1`, ws).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return n, err == nil, err
}

// ItemSync is what reconciling compares with GitHub, for one item.
type ItemSync struct {
	State         string
	Updated       time.Time
	IssueComments int // conversation comments kept
}

// ItemSyncStates lists every kept item's state, updated time and number of conversation comments.
func (s *Store) ItemSyncStates(ws int64) (map[int]ItemSync, error) {
	rows, err := s.db.Query(`SELECT i.number, i.state, i.updated_at,
			(SELECT COUNT(*) FROM item_comments c WHERE c.workspace_id = i.workspace_id AND c.number = i.number AND c.kind = 'comment')
		FROM items i WHERE i.workspace_id = ? AND i.slim = 0`, ws)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]ItemSync{}
	for rows.Next() {
		var n int
		var is ItemSync
		var up int64
		if err := rows.Scan(&n, &is.State, &up, &is.IssueComments); err != nil {
			return nil, err
		}
		is.Updated = fromUnix(up)
		out[n] = is
	}
	return out, rows.Err()
}

// IndexStats counts the local copy.
type IndexStats struct {
	Items, PRs, Issues, Open, Slim, Comments int
}

// IndexStats counts a workspace's local copy of pull requests and issues.
func (s *Store) IndexStats(ws int64) (IndexStats, error) {
	var st IndexStats
	err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(kind = 'pr'), 0), COALESCE(SUM(kind = 'issue'), 0), COALESCE(SUM(state = 'open'), 0), COALESCE(SUM(slim), 0)
		FROM items WHERE workspace_id = ?`, ws).Scan(&st.Items, &st.PRs, &st.Issues, &st.Open, &st.Slim)
	if err != nil {
		return st, err
	}
	err = s.db.QueryRow(`SELECT COUNT(*) FROM item_comments WHERE workspace_id = ?`, ws).Scan(&st.Comments)
	return st, err
}

// IndexGet reads a workspace's note about its sync ("" when none); IndexSet writes one.
func (s *Store) IndexGet(ws int64, key string) string {
	var v string
	s.db.QueryRow(`SELECT value FROM index_state WHERE workspace_id = ? AND key = ?`, ws, key).Scan(&v)
	return v
}

func (s *Store) IndexSet(ws int64, key, value string) error {
	_, err := s.db.Exec(`INSERT INTO index_state (workspace_id, key, value) VALUES (?, ?, ?)
		ON CONFLICT (workspace_id, key) DO UPDATE SET value = excluded.value`, ws, key, value)
	return err
}

// AllWorkspaces lists every workspace, whoever may see it.
func (s *Store) AllWorkspaces() ([]Workspace, error) {
	rows, err := s.db.Query(`SELECT id, name, created_at FROM workspaces ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Workspace
	for rows.Next() {
		var w Workspace
		var at int64
		if err := rows.Scan(&w.ID, &w.Name, &at); err != nil {
			return nil, err
		}
		w.CreatedAt = time.Unix(at, 0)
		out = append(out, w)
	}
	return out, rows.Err()
}

// ---- Reading: what agents ask. Every query carries the workspace id, and a ItemIndex is made for one. ----

// ItemIndex reads one workspace's local copy. It cannot see another workspace's.
type ItemIndex struct {
	s  *Store
	ws int64
}

// ItemIndex returns the reader for one workspace.
func (s *Store) ItemIndex(ws int64) *ItemIndex { return &ItemIndex{s, ws} }

// ItemSearch is a search of titles, bodies and comments. Empty fields do not filter.
type ItemSearch struct {
	Query  string
	Kind   string // "pr" or "issue"
	State  string // "open", "closed" (closed without merging) or "merged"
	Author string
	Label  string
	Limit  int
	// Phrase wants the words together in that order; Any is satisfied by one of them (best match
	// first). With neither, every word is required, anywhere.
	Phrase, Any bool
}

// ItemHit is one item a search found, with the words around the match.
type ItemHit struct {
	Number  int
	Kind    string
	Title   string
	State   string // open, closed or merged
	Author  string
	Where   string // "title or description", or what kind of comment by whom
	Snippet string
	score   float64
}

var wordRE = regexp.MustCompile(`[\p{L}\p{N}_]+`)

// matchQuery turns what an agent typed into an FTS5 query that cannot be a syntax error: every
// word quoted, all required; or, with any, each one enough; or, with phrase, the words together in
// that order ("max-batch" finds "max batch" and "max-batch").
func matchQuery(q string, phrase, any bool) (string, error) {
	words := wordRE.FindAllString(q, 20)
	if len(words) == 0 {
		return "", &InputError{"give at least one word to search for"}
	}
	if phrase {
		return `"` + strings.Join(words, " ") + `"`, nil
	}
	for i, w := range words {
		words[i] = `"` + w + `"`
	}
	if any {
		return strings.Join(words, " OR "), nil
	}
	return strings.Join(words, " "), nil
}

func stateOf(state string, merged bool) string {
	if merged {
		return "merged"
	}
	return state
}

// filters adds the search's conditions on an items row aliased i.
func (q ItemSearch) filters(where *strings.Builder, args *[]any) {
	if q.Kind != "" {
		where.WriteString(" AND i.kind = ?")
		*args = append(*args, q.Kind)
	}
	switch q.State {
	case "open":
		where.WriteString(" AND i.state = 'open'")
	case "closed":
		where.WriteString(" AND i.state = 'closed' AND i.merged = 0")
	case "merged":
		where.WriteString(" AND i.merged = 1")
	}
	if q.Author != "" {
		where.WriteString(" AND i.author = ? COLLATE NOCASE")
		*args = append(*args, q.Author)
	}
	if q.Label != "" {
		where.WriteString(" AND EXISTS (SELECT 1 FROM item_labels l WHERE l.workspace_id = i.workspace_id AND l.number = i.number AND l.label = ? COLLATE NOCASE)")
		*args = append(*args, q.Label)
	}
}

// Search finds items by the words in their titles, bodies and comments, best match first, one hit
// per item.
func (x *ItemIndex) Search(q ItemSearch) ([]ItemHit, error) {
	match, err := matchQuery(q.Query, q.Phrase, q.Any)
	if err != nil {
		return nil, err
	}
	if q.Limit < 1 || q.Limit > 50 {
		q.Limit = 10
	}
	best := map[int]ItemHit{}
	keep := func(h ItemHit) {
		if old, ok := best[h.Number]; !ok || h.score < old.score {
			best[h.Number] = h
		}
	}
	run := func(query string, args []any, where func(*sql.Rows) (ItemHit, error)) error {
		rows, err := x.s.db.Query(query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			h, err := where(rows)
			if err != nil {
				return err
			}
			keep(h)
		}
		return rows.Err()
	}

	var w strings.Builder
	args := []any{match, x.ws}
	q.filters(&w, &args)
	args = append(args, q.Limit*3)
	err = run(`SELECT i.number, i.kind, i.title, i.state, i.merged, i.author, snippet(item_fts, -1, '[', ']', '…', 20), bm25(item_fts)
		FROM item_fts JOIN items i ON i.id = item_fts.rowid
		WHERE item_fts MATCH ? AND i.workspace_id = ?`+w.String()+` ORDER BY bm25(item_fts) LIMIT ?`, args,
		func(r *sql.Rows) (ItemHit, error) {
			var h ItemHit
			var merged bool
			err := r.Scan(&h.Number, &h.Kind, &h.Title, &h.State, &merged, &h.Author, &h.Snippet, &h.score)
			h.State, h.Where = stateOf(h.State, merged), "title or description"
			return h, err
		})
	if err != nil {
		return nil, err
	}

	w.Reset()
	args = []any{match, x.ws}
	q.filters(&w, &args)
	args = append(args, q.Limit*3)
	err = run(`SELECT i.number, i.kind, i.title, i.state, i.merged, i.author, c.kind, c.author, snippet(comment_fts, -1, '[', ']', '…', 20), bm25(comment_fts)
		FROM comment_fts JOIN item_comments c ON c.id = comment_fts.rowid
		JOIN items i ON i.workspace_id = c.workspace_id AND i.number = c.number
		WHERE comment_fts MATCH ? AND c.workspace_id = ?`+w.String()+` ORDER BY bm25(comment_fts) LIMIT ?`, args,
		func(r *sql.Rows) (ItemHit, error) {
			var h ItemHit
			var merged bool
			var ckind, cauthor string
			err := r.Scan(&h.Number, &h.Kind, &h.Title, &h.State, &merged, &h.Author, &ckind, &cauthor, &h.Snippet, &h.score)
			h.State, h.Where = stateOf(h.State, merged), commentWhere(ckind)+" by "+cauthor
			return h, err
		})
	if err != nil {
		return nil, err
	}

	out := make([]ItemHit, 0, len(best))
	for _, h := range best {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score < out[j].score
		}
		return out[i].Number > out[j].Number
	})
	return out[:min(len(out), q.Limit)], nil
}

func commentWhere(kind string) string {
	switch kind {
	case "review":
		return "review"
	case "review_comment":
		return "inline review comment"
	}
	return "comment"
}

// ItemDetail is everything kept about one item.
type ItemDetail struct {
	Item
	Slim       bool // only the short record is kept locally: no description, discussion or links
	Remote     bool // read live from GitHub, not kept locally
	Files      []ItemFile
	Discussion []ItemComment // oldest first
	LinksOut   []ItemRef     // what it refers to
	LinksIn    []ItemRef     // what refers to it
}

// ItemRef is an item at the other end of a link; Title and State are empty when that item is not
// kept (a number from outside the repository's history, or one not yet synced).
type ItemRef struct {
	Number int
	Kind   string
	Title  string
	State  string
	How    string
}

// Read returns an item with its files, discussion and links, or ErrNotFound.
func (x *ItemIndex) Read(number int) (*ItemDetail, error) {
	d := &ItemDetail{}
	var created, updated, closed, merged int64
	var isMerged bool
	err := x.s.db.QueryRow(`SELECT kind, title, body, state, merged, draft, author, association, created_at, updated_at, closed_at, merged_at,
			base_ref, head_ref, head_sha, additions, deletions, slim FROM items WHERE workspace_id = ? AND number = ?`, x.ws, number).
		Scan(&d.Kind, &d.Title, &d.Body, &d.State, &isMerged, &d.Draft, &d.Author, &d.Association, &created, &updated, &closed, &merged,
			&d.Base, &d.HeadRef, &d.HeadSHA, &d.Additions, &d.Deletions, &d.Slim)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d.Number, d.Merged = number, isMerged
	d.Created, d.Updated, d.Closed, d.MergedAt = fromUnix(created), fromUnix(updated), fromUnix(closed), fromUnix(merged)

	rows, err := x.s.db.Query(`SELECT label FROM item_labels WHERE workspace_id = ? AND number = ? ORDER BY label`, x.ws, number)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			rows.Close()
			return nil, err
		}
		d.Labels = append(d.Labels, l)
	}
	rows.Close()

	if rows, err = x.s.db.Query(`SELECT path, status, additions, deletions FROM item_files WHERE workspace_id = ? AND number = ? ORDER BY path`, x.ws, number); err != nil {
		return nil, err
	}
	for rows.Next() {
		var f ItemFile
		if err := rows.Scan(&f.Path, &f.Status, &f.Additions, &f.Deletions); err != nil {
			rows.Close()
			return nil, err
		}
		d.Files = append(d.Files, f)
	}
	rows.Close()

	if rows, err = x.s.db.Query(`SELECT kind, github_id, author, association, body, review_state, path, line, created_at, updated_at
		FROM item_comments WHERE workspace_id = ? AND number = ? ORDER BY created_at, id`, x.ws, number); err != nil {
		return nil, err
	}
	for rows.Next() {
		c := ItemComment{Number: number}
		var cr, up int64
		if err := rows.Scan(&c.Kind, &c.ID, &c.Author, &c.Association, &c.Body, &c.ReviewState, &c.Path, &c.Line, &cr, &up); err != nil {
			rows.Close()
			return nil, err
		}
		c.Created, c.Updated = fromUnix(cr), fromUnix(up)
		d.Discussion = append(d.Discussion, c)
	}
	rows.Close()

	if d.LinksOut, err = x.refs(`SELECT l.to_number, l.how FROM item_links l WHERE l.workspace_id = ? AND l.from_number = ?`, number); err != nil {
		return nil, err
	}
	if d.LinksIn, err = x.refs(`SELECT l.from_number, l.how FROM item_links l WHERE l.workspace_id = ? AND l.to_number = ?`, number); err != nil {
		return nil, err
	}
	return d, nil
}

// refs runs a query of (other number, how) for one item and fills in the others' titles. A number
// linked in several ways is reported once, by the strongest: closes, then fixes, then mentions.
func (x *ItemIndex) refs(query string, number int) ([]ItemRef, error) {
	rows, err := x.s.db.Query(query, x.ws, number)
	if err != nil {
		return nil, err
	}
	rank := map[string]int{"mentions": 0, "fixes": 1, "closes": 2}
	how := map[int]string{}
	for rows.Next() {
		var n int
		var h string
		if err := rows.Scan(&n, &h); err != nil {
			rows.Close()
			return nil, err
		}
		if old, ok := how[n]; !ok || rank[h] > rank[old] {
			how[n] = h
		}
	}
	rows.Close()
	var out []ItemRef
	for n, h := range how {
		r := ItemRef{Number: n, How: h}
		var merged bool
		err := x.s.db.QueryRow(`SELECT kind, title, state, merged FROM items WHERE workspace_id = ? AND number = ?`, x.ws, n).Scan(&r.Kind, &r.Title, &r.State, &merged)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		r.State = stateOf(r.State, merged)
		if r.Title == "" && r.Kind == "" {
			r.State = ""
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

// SharedFiles is a pull request that touches some of the same files as another.
type SharedFiles struct {
	Number int
	Title  string
	State  string
	Author string
	Files  []string // the files in common, up to ten
	Count  int      // how many files in common
}

// ItemRelated is what to read next after an item.
type ItemRelated struct {
	Out    []ItemRef     // items it refers to
	In     []ItemRef     // items that refer to it
	Shared []SharedFiles // pull requests changing any of the same files, most overlap first
}

// Related returns the items an item refers to, those that refer to it, and the pull requests
// that touch any of the same files. It returns ErrNotFound for an item not kept.
func (x *ItemIndex) Related(number int) (*ItemRelated, error) {
	var one int
	if err := x.s.db.QueryRow(`SELECT 1 FROM items WHERE workspace_id = ? AND number = ?`, x.ws, number).Scan(&one); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	r := &ItemRelated{}
	var err error
	if r.Out, err = x.refs(`SELECT l.to_number, l.how FROM item_links l WHERE l.workspace_id = ? AND l.from_number = ?`, number); err != nil {
		return nil, err
	}
	if r.In, err = x.refs(`SELECT l.from_number, l.how FROM item_links l WHERE l.workspace_id = ? AND l.to_number = ?`, number); err != nil {
		return nil, err
	}
	rows, err := x.s.db.Query(`SELECT f2.number, i.title, i.state, i.merged, i.author, f2.path
		FROM item_files f1
		JOIN item_files f2 ON f2.workspace_id = f1.workspace_id AND f2.path = f1.path AND f2.number <> f1.number
		JOIN items i ON i.workspace_id = f2.workspace_id AND i.number = f2.number
		WHERE f1.workspace_id = ? AND f1.number = ? ORDER BY f2.number, f2.path`, x.ws, number)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byNum := map[int]*SharedFiles{}
	for rows.Next() {
		var n int
		var s SharedFiles
		var merged bool
		var path string
		if err := rows.Scan(&n, &s.Title, &s.State, &merged, &s.Author, &path); err != nil {
			return nil, err
		}
		sf := byNum[n]
		if sf == nil {
			s.Number, s.State = n, stateOf(s.State, merged)
			sf = &s
			byNum[n] = sf
		}
		sf.Count++
		if len(sf.Files) < 10 {
			sf.Files = append(sf.Files, path)
		}
	}
	for _, sf := range byNum {
		r.Shared = append(r.Shared, *sf)
	}
	sort.Slice(r.Shared, func(i, j int) bool {
		if r.Shared[i].Count != r.Shared[j].Count {
			return r.Shared[i].Count > r.Shared[j].Count
		}
		return r.Shared[i].Number > r.Shared[j].Number
	})
	if len(r.Shared) > 20 {
		r.Shared = r.Shared[:20]
	}
	return r, rows.Err()
}

// Touching lists the pull requests, open or merged, that changed a file or anything under a
// folder: newest first. A path ending in "/" or naming a folder prefix matches what is below it.
func (x *ItemIndex) Touching(path string, limit int) ([]SharedFiles, error) {
	path = strings.Trim(strings.TrimSpace(path), "/")
	if path == "" {
		return nil, &InputError{"give a file or folder path"}
	}
	if limit < 1 || limit > 100 {
		limit = 30
	}
	like := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(path) + "/%"
	rows, err := x.s.db.Query(`SELECT i.number, i.title, i.state, i.merged, i.author, f.path
		FROM item_files f JOIN items i ON i.workspace_id = f.workspace_id AND i.number = f.number
		WHERE f.workspace_id = ? AND (f.path = ? OR f.path LIKE ? ESCAPE '\') AND i.kind = 'pr' AND (i.state = 'open' OR i.merged = 1)
		ORDER BY (i.state = 'open') DESC, i.updated_at DESC, i.number DESC, f.path`, x.ws, path, like)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SharedFiles
	idx := map[int]int{}
	for rows.Next() {
		var s SharedFiles
		var merged bool
		var p string
		if err := rows.Scan(&s.Number, &s.Title, &s.State, &merged, &s.Author, &p); err != nil {
			return nil, err
		}
		i, ok := idx[s.Number]
		if !ok {
			if len(out) >= limit {
				continue
			}
			s.State = stateOf(s.State, merged)
			idx[s.Number] = len(out)
			out = append(out, s)
			i = len(out) - 1
		}
		out[i].Count++
		if len(out[i].Files) < 10 {
			out[i].Files = append(out[i].Files, p)
		}
	}
	return out, rows.Err()
}

// PruneComments deletes an item's comments of a kind whose GitHub ids are not in keep: they were
// deleted on GitHub.
func (s *Store) PruneComments(ws int64, number int, kind string, keep []int64) error {
	have := map[int64]bool{}
	for _, id := range keep {
		have[id] = true
	}
	rows, err := s.db.Query(`SELECT github_id FROM item_comments WHERE workspace_id = ? AND number = ? AND kind = ?`, ws, number, kind)
	if err != nil {
		return err
	}
	var gone []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		if !have[id] {
			gone = append(gone, id)
		}
	}
	rows.Close()
	for _, id := range gone {
		if _, err := s.db.Exec(`DELETE FROM item_comments WHERE workspace_id = ? AND kind = ? AND github_id = ?`, ws, kind, id); err != nil {
			return err
		}
		if _, err := s.db.Exec(`DELETE FROM item_links WHERE workspace_id = ? AND from_number = ? AND src = ?`, ws, number, fmt.Sprintf("%s:%d", kind, id)); err != nil {
			return err
		}
	}
	return nil
}
