package store

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // Eastern time in case headlines, on hosts without a zoneinfo database
)

// Case is the file of findings on one issue or pull request.
type Case struct {
	UUID        string
	WorkspaceID int64
	Number      int
	Kind        string // "pr" or "issue"
	Title       string
	Contents    string // Markdown, always ending in a newline
	Version     int
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Lines is how many lines the case file has.
func (c Case) Lines() int { return strings.Count(c.Contents, "\n") }

// Label is how the case is named: "PR #459" or "Issue #12".
func (c Case) Label() string {
	if c.Kind == "pr" {
		return fmt.Sprintf("PR #%d", c.Number)
	}
	return fmt.Sprintf("Issue #%d", c.Number)
}

// Contribution is one addition to a case file, or a person's edit of it.
type Contribution struct {
	ID          int64
	Author      string
	Kind        string // "system", "agent" or "human"
	Draft       string // while open: the text that finalizing appends
	Note        string // a human edit: which lines it changed
	StartLine   int    // the lines it was appended as; 0 for none
	EndLine     int
	StartedAt   time.Time
	FinalizedAt time.Time // zero while open
}

// Open reports whether the contribution can still be revised.
func (c Contribution) Open() bool { return c.FinalizedAt.IsZero() }

// ErrFinalized means the contribution can no longer change.
var ErrFinalized = errors.New("the contribution is finalized and can no longer change")

// newUUID returns a random (version 4) UUID from the system's secure random source.
func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

const caseCols = `uuid, workspace_id, number, kind, title, contents, version, created_at, updated_at`

func scanCase(row scanner) (*Case, error) {
	var c Case
	var created, updated int64
	if err := row.Scan(&c.UUID, &c.WorkspaceID, &c.Number, &c.Kind, &c.Title, &c.Contents, &c.Version, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	c.CreatedAt, c.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
	return &c, nil
}

// OpenCase returns the workspace's case for an issue or pull request, creating it if there is
// none. A new case file starts with one line saying when it was opened and what for, written by
// Autobot as a finalized contribution. firstLine is that line, without its newline.
func (s *Store) OpenCase(ws *Workspace, number int, kind, title, firstLine string) (*Case, bool, error) {
	if kind != "pr" && kind != "issue" {
		return nil, false, fmt.Errorf("case kind %q is not pr or issue", kind)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	if c, err := scanCase(tx.QueryRow(`SELECT `+caseCols+` FROM cases WHERE workspace_id = ? AND number = ?`, ws.ID, number)); err == nil {
		return c, false, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, false, err
	}
	id, t := newUUID(), now()
	if _, err := tx.Exec(`INSERT INTO cases (uuid, workspace_id, number, kind, title, contents, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, id, ws.ID, number, kind, title, firstLine+"\n", t, t); err != nil {
		return nil, false, err
	}
	if _, err := tx.Exec(`INSERT INTO case_contributions (case_uuid, author, kind, start_line, end_line, started_at, finalized_at)
		VALUES (?, 'Autobot', 'system', 1, 1, ?, ?)`, id, t, t); err != nil {
		return nil, false, err
	}
	label := "issue"
	if kind == "pr" {
		label = "pull request"
	}
	if err := addEvent(tx, ws.ID, nil, fmt.Sprintf("opened a case for %s #%d", label, number)); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	c, err := s.Case(id)
	return c, true, err
}

// Case looks a case up by its uuid.
func (s *Store) Case(uuid string) (*Case, error) {
	return scanCase(s.db.QueryRow(`SELECT `+caseCols+` FROM cases WHERE uuid = ?`, uuid))
}

// CaseByNumber looks a workspace's case up by its issue or pull request number.
func (s *Store) CaseByNumber(workspaceID int64, number int) (*Case, error) {
	return scanCase(s.db.QueryRow(`SELECT `+caseCols+` FROM cases WHERE workspace_id = ? AND number = ?`, workspaceID, number))
}

// Cases lists a workspace's cases, most recently changed first, without their contents.
func (s *Store) Cases(workspaceID int64, limit int) ([]Case, error) {
	rows, err := s.db.Query(`SELECT `+strings.Replace(caseCols, "contents", "''", 1)+` FROM cases
		WHERE workspace_id = ? ORDER BY updated_at DESC, number DESC LIMIT ?`, workspaceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Case
	for rows.Next() {
		c, err := scanCase(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func scanContribution(row scanner) (*Contribution, error) {
	var c Contribution
	var start, end, finalized sql.NullInt64
	var started int64
	if err := row.Scan(&c.ID, &c.Author, &c.Kind, &c.Draft, &c.Note, &start, &end, &started, &finalized); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	c.StartLine, c.EndLine, c.StartedAt = int(start.Int64), int(end.Int64), time.Unix(started, 0)
	if finalized.Valid {
		c.FinalizedAt = time.Unix(finalized.Int64, 0)
	}
	return &c, nil
}

const contributionCols = `id, author, kind, draft, note, start_line, end_line, started_at, finalized_at`

// Contributions lists a case's contributions, oldest first.
func (s *Store) Contributions(uuid string) ([]Contribution, error) {
	rows, err := s.db.Query(`SELECT `+contributionCols+` FROM case_contributions WHERE case_uuid = ? ORDER BY id`, uuid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Contribution
	for rows.Next() {
		c, err := scanContribution(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// StartContribution opens a contribution to a case for author, with an empty draft.
func (s *Store) StartContribution(uuid, author string) (int64, error) {
	if strings.TrimSpace(author) == "" {
		return 0, errors.New("a contribution needs an author")
	}
	res, err := s.db.Exec(`INSERT INTO case_contributions (case_uuid, author, kind, started_at) VALUES (?, ?, 'agent', ?)`, uuid, author, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SetDraft replaces an open contribution's draft. Only its own text changes; the case file does
// not, until the contribution is finalized.
func (s *Store) SetDraft(id int64, text string) error {
	res, err := s.db.Exec(`UPDATE case_contributions SET draft = ? WHERE id = ? AND finalized_at IS NULL`, text, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrFinalized
	}
	return nil
}

// FinalizeContribution appends an open contribution's draft to the end of its case file and records
// the lines it became. From then on it cannot change. An empty draft is finalized with no lines.
func (s *Store) FinalizeContribution(id int64) (*Contribution, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var uuid, draft string
	var finalized sql.NullInt64
	if err := tx.QueryRow(`SELECT case_uuid, draft, finalized_at FROM case_contributions WHERE id = ?`, id).Scan(&uuid, &draft, &finalized); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if finalized.Valid {
		return nil, ErrFinalized
	}
	t := now()
	text := strings.TrimRight(strings.ReplaceAll(draft, "\r\n", "\n"), "\n")
	var start, end any
	if strings.TrimSpace(text) != "" {
		var contents string
		if err := tx.QueryRow(`SELECT contents FROM cases WHERE uuid = ?`, uuid).Scan(&contents); err != nil {
			return nil, err
		}
		before := strings.Count(contents, "\n")
		added := strings.Count(text, "\n") + 1
		start, end = before+1, before+added
		if _, err := tx.Exec(`UPDATE cases SET contents = contents || ?, version = version + 1, updated_at = ? WHERE uuid = ?`,
			text+"\n", t, uuid); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(`UPDATE case_contributions SET start_line = ?, end_line = ?, finalized_at = ? WHERE id = ?`,
		start, end, t, id); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return scanContribution(s.db.QueryRow(`SELECT `+contributionCols+` FROM case_contributions WHERE id = ?`, id))
}

// ConflictingEdit means someone changed the case file after the editor opened it.
type ConflictingEdit struct{ Current *Case }

func (e *ConflictingEdit) Error() string { return "the case file changed while it was being edited" }

// EditCase replaces a case file with a person's edit, made from version baseVersion, and records
// the edit as a contribution whose note says which lines changed. If the file has changed since
// baseVersion, nothing is saved and a *ConflictingEdit carries the current case.
func (s *Store) EditCase(uuid string, baseVersion int, contents string, editor *User) error {
	contents = strings.ReplaceAll(contents, "\r\n", "\n")
	if !strings.HasSuffix(contents, "\n") {
		contents += "\n"
	}
	if len(contents) > 4<<20 {
		return invalid("A case file is limited to 4 MB.")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	cur, err := scanCase(tx.QueryRow(`SELECT `+caseCols+` FROM cases WHERE uuid = ?`, uuid))
	if err != nil {
		return err
	}
	if cur.Version != baseVersion {
		return &ConflictingEdit{Current: cur}
	}
	if cur.Contents == contents {
		return nil
	}
	t := now()
	if _, err := tx.Exec(`UPDATE cases SET contents = ?, version = version + 1, updated_at = ? WHERE uuid = ?`, contents, t, uuid); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO case_contributions (case_uuid, author, kind, note, started_at, finalized_at) VALUES (?, ?, 'human', ?, ?, ?)`,
		uuid, editor.Name(), LineChanges(cur.Contents, contents), t, t); err != nil {
		return err
	}
	return tx.Commit()
}

// LineChanges describes how after differs from before, line by line, such as "line 37 changed;
// lines 72–100 removed; 4 lines added after line 50". It is a light record, not a patch.
func LineChanges(before, after string) string {
	a, b := strings.Split(strings.TrimSuffix(before, "\n"), "\n"), strings.Split(strings.TrimSuffix(after, "\n"), "\n")
	if len(a)*len(b) > 25_000_000 {
		return fmt.Sprintf("%d lines before, %d after (too large to compare line by line)", len(a), len(b))
	}
	// lcs[i][j] is the longest common subsequence of a[i:] and b[j:].
	lcs := make([][]int32, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int32, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var notes []string
	span := func(from, to int, what string) string {
		if from == to {
			return fmt.Sprintf("line %d %s", from, what)
		}
		return fmt.Sprintf("lines %d–%d %s", from, to, what)
	}
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		if i < len(a) && j < len(b) && a[i] == b[j] {
			i, j = i+1, j+1
			continue
		}
		// A run of differences: lines dropped from before and lines new in after.
		di, dj := i, j
		for (i < len(a) || j < len(b)) && !(i < len(a) && j < len(b) && a[i] == b[j]) {
			if j >= len(b) || (i < len(a) && lcs[i+1][j] >= lcs[i][j+1]) {
				i++
			} else {
				j++
			}
		}
		removed, added := i-di, j-dj
		switch {
		case removed > 0 && added > 0 && removed == added:
			notes = append(notes, span(di+1, i, "changed"))
		case removed > 0 && added > 0:
			notes = append(notes, span(di+1, i, fmt.Sprintf("replaced by %d", added)))
		case removed > 0:
			notes = append(notes, span(di+1, i, "removed"))
		case added == 1:
			notes = append(notes, fmt.Sprintf("1 line added after line %d", di))
		default:
			notes = append(notes, fmt.Sprintf("%d lines added after line %d", added, di))
		}
	}
	if len(notes) == 0 {
		return "no change"
	}
	return strings.Join(notes, "; ")
}

// CaseHeadline is a case file's first line: when it was opened, in Eastern time, and for what.
func CaseHeadline(at time.Time, repo, kind string, number int, title string) string {
	eastern, err := time.LoadLocation("America/New_York")
	if err != nil {
		eastern = time.UTC
	}
	label, path := "Issue", "issues"
	if kind == "pr" {
		label, path = "PR", "pull"
	}
	return fmt.Sprintf("Opened %s ET for [%s %s #%d](https://github.com/%s/%s/%d): %s",
		at.In(eastern).Format("2006-01-02 15:04"), repo, label, number, repo, path, number, escapeMarkdown(strings.TrimSpace(title)))
}

// escapeMarkdown makes text show as itself in Markdown: a title written on GitHub cannot add
// links, images or formatting to a case file.
func escapeMarkdown(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune("\\`*_{}[]()<>#+-.!|~", r) {
			b.WriteByte('\\')
		}
		if r == '\n' || r == '\r' {
			r = ' '
		}
		b.WriteRune(r)
	}
	return b.String()
}

// CaseSummary is one row of a workspace's list of cases.
type CaseSummary struct {
	Number    int
	Kind      string
	Title     string
	Lines     int
	UpdatedAt time.Time
}

// Label is how the case is named: "PR #459" or "Issue #12".
func (c CaseSummary) Label() string { return Case{Number: c.Number, Kind: c.Kind}.Label() }

// FindCases lists a workspace's cases, most recently changed first, matching q when it is not
// empty: a number (with or without #) matches that issue or PR, anything else matches the title.
func (s *Store) FindCases(workspaceID int64, q string, limit int) ([]CaseSummary, error) {
	q = strings.TrimSpace(q)
	query := `SELECT number, kind, title, length(contents) - length(replace(contents, char(10), '')), updated_at
		FROM cases WHERE workspace_id = ?`
	args := []any{workspaceID}
	if n, err := strconv.Atoi(strings.TrimPrefix(q, "#")); err == nil {
		query += ` AND number = ?`
		args = append(args, n)
	} else if q != "" {
		query += ` AND title LIKE ? ESCAPE '\'`
		args = append(args, "%"+strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(q)+"%")
	}
	query += ` ORDER BY updated_at DESC, number DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CaseSummary
	for rows.Next() {
		var c CaseSummary
		var at int64
		if err := rows.Scan(&c.Number, &c.Kind, &c.Title, &c.Lines, &at); err != nil {
			return nil, err
		}
		c.UpdatedAt = time.Unix(at, 0)
		out = append(out, c)
	}
	return out, rows.Err()
}

// CaseCount is how many cases a workspace has.
func (s *Store) CaseCount(workspaceID int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM cases WHERE workspace_id = ?`, workspaceID).Scan(&n)
	return n, err
}
