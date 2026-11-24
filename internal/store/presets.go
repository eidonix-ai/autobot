package store

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Presets (Derek, 2026-11-23): the texts an Agent block can follow instead of its own. A preset is
// shared (no owner: admins make and change it, everyone uses it) or a user's own (its owner changes
// it and uses it in any workspace they are in). A block names its preset by key, which never changes;
// the database is the authority. The built-in presets are put in once, when the table is new
// (SeedPresets).
const schemaPresets = `
CREATE TABLE presets (
	id                INTEGER PRIMARY KEY,
	key               TEXT    NOT NULL UNIQUE,
	name              TEXT    NOT NULL,
	description       TEXT    NOT NULL DEFAULT '',
	owner_id          INTEGER REFERENCES users(id),
	pr_instruction    TEXT    NOT NULL,
	pr_goal           TEXT    NOT NULL,
	pr_summary        TEXT    NOT NULL DEFAULT '',
	issue_instruction TEXT    NOT NULL,
	issue_goal        TEXT    NOT NULL,
	issue_summary     TEXT    NOT NULL DEFAULT '',
	closing           TEXT    NOT NULL DEFAULT '',
	created_at        INTEGER NOT NULL,
	updated_at        INTEGER NOT NULL,
	updated_by        INTEGER REFERENCES users(id)
);
`

// schemaCitationMarks: whether the agent marks a file it cites but never opened (stored as its
// inverse, so every preset made before it keeps the marks), and the Additional instructions a block
// that chooses the preset starts with.
const schemaCitationMarks = `
ALTER TABLE presets ADD COLUMN no_citation_marks INTEGER NOT NULL DEFAULT 0;
ALTER TABLE presets ADD COLUMN additional TEXT NOT NULL DEFAULT '';
`

// schemaRunDefaults: the starting run settings a block takes when it chooses the preset, as JSON
// (workflow.RunDefaults); empty for none.
const schemaRunDefaults = `
ALTER TABLE presets ADD COLUMN run_defaults TEXT NOT NULL DEFAULT '';
`

// schemaNoTools: whether a preset runs with only the finish tool (no repo, scratchpad or plan tools),
// for text-only tasks such as editing a draft.
const schemaNoTools = `
ALTER TABLE presets ADD COLUMN no_tools INTEGER NOT NULL DEFAULT 0;
`

// Preset is a preset as stored.
type Preset struct {
	ID          int64
	Key         string // what a block stores: a lower-case slug, never changed
	Name        string
	Description string // one line
	OwnerID     int64  // 0 for a shared preset
	Owner       string // the owner's name; "" when shared
	PRInstruction, PRGoal, PRSummary,
	IssueInstruction, IssueGoal, IssueSummary string // a summary format is "" for the default summary
	Closing string // "" or "report"
	// NoCitationMarks turns off the "(not opened)" mark after a file the agent cites but never read
	// (for presets that rewrite text rather than research code).
	NoCitationMarks bool
	// NoTools runs with only the finish tool (no code, PR, item, scratchpad or plan tools).
	NoTools bool
	// Additional is what a block's Additional instructions box is filled with when the block chooses
	// the preset and the box is empty; one text for pull requests and issues.
	Additional string
	// RunDefaults is workflow.RunDefaults as JSON: the run settings a block starts with when it chooses
	// the preset; "" for none.
	RunDefaults string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Shared reports whether the preset belongs to everyone.
func (p Preset) Shared() bool { return p.OwnerID == 0 }

const presetCols = `p.id, p.key, p.name, p.description, COALESCE(p.owner_id, 0), COALESCE(` + authorName + `, ''), p.pr_instruction, p.pr_goal, p.pr_summary,
	p.issue_instruction, p.issue_goal, p.issue_summary, p.closing, p.no_citation_marks, p.no_tools, p.additional, p.run_defaults, p.created_at, p.updated_at`

const presetFrom = ` FROM presets p LEFT JOIN users u ON u.id = p.owner_id`

func scanPreset(r interface{ Scan(...any) error }) (*Preset, error) {
	var p Preset
	var created, updated int64
	err := r.Scan(&p.ID, &p.Key, &p.Name, &p.Description, &p.OwnerID, &p.Owner, &p.PRInstruction, &p.PRGoal, &p.PRSummary, &p.IssueInstruction, &p.IssueGoal, &p.IssueSummary,
		&p.Closing, &p.NoCitationMarks, &p.NoTools, &p.Additional, &p.RunDefaults, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	p.CreatedAt, p.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
	return &p, err
}

// Presets lists every preset: the shared ones first, then each owner's, by name.
func (s *Store) Presets() ([]*Preset, error) {
	rows, err := s.db.Query(`SELECT ` + presetCols + presetFrom + ` ORDER BY p.owner_id IS NOT NULL, ` + authorName + `, p.name COLLATE NOCASE, p.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Preset
	for rows.Next() {
		p, err := scanPreset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Preset returns one preset by id, or ErrNotFound.
func (s *Store) Preset(id int64) (*Preset, error) {
	return scanPreset(s.db.QueryRow(`SELECT `+presetCols+presetFrom+` WHERE p.id = ?`, id))
}

// PresetByKey returns the preset a block names, or ErrNotFound.
func (s *Store) PresetByKey(key string) (*Preset, error) {
	return scanPreset(s.db.QueryRow(`SELECT `+presetCols+presetFrom+` WHERE p.key = ?`, key))
}

var notSlug = regexp.MustCompile(`[^a-z0-9]+`)

// slug is a name as a key: lower-case letters and digits, with _ between words.
func slug(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if r < unicode.MaxASCII {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	k := strings.Trim(notSlug.ReplaceAllString(b.String(), "_"), "_")
	if len(k) > 40 {
		k = strings.Trim(k[:40], "_")
	}
	if k == "" {
		k = "preset"
	}
	return k
}

// freeKey is a key for a name that no preset has, and that is not "custom": the name's slug, with
// _2, _3… when it is taken.
func freeKey(q querier, name string) (string, error) {
	base := slug(name)
	for n := 1; ; n++ {
		key := base
		if n > 1 {
			key = base + "_" + strconv.Itoa(n)
		}
		var one int
		err := q.QueryRow(`SELECT 1 FROM presets WHERE key = ?`, key).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) && key != "custom" {
			return key, nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
}

// Limits of a preset's fields.
const (
	MaxPresetName = 60
	MaxPresetDesc = 200
	MaxPresetText = 50_000
)

// CheckPreset checks what a person typed: a name, a one-line description, the four texts, the closing
// step. (That the texts use only known tags is the web layer's to check, with package workflow.)
func CheckPreset(p *Preset) error {
	p.Name, p.Description = strings.TrimSpace(p.Name), strings.TrimSpace(p.Description)
	switch {
	case p.Name == "":
		return invalid("give the preset a name")
	case len([]rune(p.Name)) > MaxPresetName || strings.ContainsAny(p.Name, "\n\r\t"):
		return invalid("a preset's name is one line of up to %d characters", MaxPresetName)
	case len([]rune(p.Description)) > MaxPresetDesc || strings.ContainsAny(p.Description, "\n\r"):
		return invalid("a preset's description is one line of up to %d characters", MaxPresetDesc)
	case strings.TrimSpace(p.PRInstruction) == "" || strings.TrimSpace(p.IssueInstruction) == "":
		return invalid("write the instruction for pull requests and for issues")
	case strings.TrimSpace(p.PRGoal) == "" || strings.TrimSpace(p.IssueGoal) == "":
		return invalid("say when the work is done, for pull requests and for issues")
	case len(p.PRInstruction) > MaxPresetText || len(p.PRGoal) > MaxPresetText || len(p.IssueInstruction) > MaxPresetText || len(p.IssueGoal) > MaxPresetText ||
		len(p.PRSummary) > MaxPresetText || len(p.IssueSummary) > MaxPresetText || len(p.Additional) > MaxPresetText || len(p.RunDefaults) > MaxPresetText:
		return invalid("the texts are limited to %d characters each", MaxPresetText)
	case p.Closing != "" && p.Closing != "report":
		return invalid("choose a closing step")
	}
	return nil
}

// CreatePreset stores a new preset, shared when p.OwnerID is 0, and gives it its key.
func (s *Store) CreatePreset(p Preset, actor *User) (*Preset, error) {
	if err := CheckPreset(&p); err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	key, err := freeKey(tx, p.Name)
	if err != nil {
		return nil, err
	}
	res, err := tx.Exec(`INSERT INTO presets (key, name, description, owner_id, pr_instruction, pr_goal, pr_summary, issue_instruction, issue_goal, issue_summary, closing, no_citation_marks, no_tools, additional, run_defaults, created_at, updated_at, updated_by)
		VALUES (?, ?, ?, NULLIF(?, 0), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, key, p.Name, p.Description, p.OwnerID, p.PRInstruction, p.PRGoal, p.PRSummary,
		p.IssueInstruction, p.IssueGoal, p.IssueSummary, p.Closing, p.NoCitationMarks, p.NoTools, p.Additional, p.RunDefaults, now(), now(), actor.ID)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.Preset(id)
}

// UpdatePreset changes a preset's name, description, texts, closing step, citation marks, tool mode,
// starting additional instructions and starting run settings. Its key, owner and sharing never change.
func (s *Store) UpdatePreset(id int64, p Preset, actor *User) error {
	if err := CheckPreset(&p); err != nil {
		return err
	}
	res, err := s.db.Exec(`UPDATE presets SET name = ?, description = ?, pr_instruction = ?, pr_goal = ?, pr_summary = ?, issue_instruction = ?, issue_goal = ?, issue_summary = ?,
		closing = ?, no_citation_marks = ?, no_tools = ?, additional = ?, run_defaults = ?, updated_at = ?, updated_by = ? WHERE id = ?`, p.Name, p.Description, p.PRInstruction, p.PRGoal, p.PRSummary, p.IssueInstruction, p.IssueGoal,
		p.IssueSummary, p.Closing, p.NoCitationMarks, p.NoTools, p.Additional, p.RunDefaults, now(), actor.ID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// presetsSeeded is the setting that says the built-in presets were put in.
const presetsSeeded = "presets_seeded"

// SeedPresets puts the built-in presets in as shared presets, once: the first time it is called for
// a database. After that the database is the authority, and a preset an admin removed stays removed.
func (s *Store) SeedPresets(seeds []Preset) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var done string
	err = tx.QueryRow(`SELECT value FROM settings WHERE key = ?`, presetsSeeded).Scan(&done)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	for _, p := range seeds {
		if _, err := tx.Exec(`INSERT INTO presets (key, name, description, owner_id, pr_instruction, pr_goal, pr_summary, issue_instruction, issue_goal, issue_summary, closing, no_citation_marks, no_tools, additional, run_defaults, created_at, updated_at)
			VALUES (?, ?, ?, NULL, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, p.Key, p.Name, p.Description, p.PRInstruction, p.PRGoal, p.PRSummary, p.IssueInstruction, p.IssueGoal,
			p.IssueSummary, p.Closing, p.NoCitationMarks, p.NoTools, p.Additional, p.RunDefaults, now(), now()); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO settings (key, value) VALUES (?, '1')`, presetsSeeded); err != nil {
		return err
	}
	return tx.Commit()
}

// PresetRename is a seeded preset's old name and the one it is given.
type PresetRename struct{ Key, From, To string }

// PresetTextUpdate replaces a shared preset's PR instruction only if it still equals From.
type PresetTextUpdate struct{ Key, From, To string }

// PresetUpgrade updates a shared preset's fields in place only where the stored value still matches
// one of the expected previous SHA-256 hex digests in Match (keyed by column name: description,
// pr_instruction, pr_goal, pr_summary, issue_instruction, issue_goal, issue_summary, run_defaults).
// Any field an admin edited does not match and is preserved untouched.
type PresetUpgrade struct {
	Key   string
	Match map[string][]string
	To    Preset
}

// PresetSeedStep is a one-time idempotent migration of shared seeded presets, recorded in settings
// under Marker so each step runs at most once per database.
type PresetSeedStep struct {
	Marker      string
	Renames     []PresetRename    // rename only where name still matches From
	Adds        []Preset          // insert as shared only where key is free
	ReplacePR   *PresetTextUpdate // replace pr_instruction only where untouched
	SetDefaults []string          // keys to receive RunDefaults where shared and currently empty
	RunDefaults string
	SetNoTools  []string        // keys to mark no_tools = 1 where shared
	Upgrades    []PresetUpgrade // per-field update-if-untouched upgrades for shared presets
}

// RunPresetSteps applies each unapplied PresetSeedStep in order, each in its own transaction.
func (s *Store) RunPresetSteps(steps []PresetSeedStep) error {
	for _, step := range steps {
		if err := s.runPresetStep(step); err != nil {
			return err
		}
	}
	return nil
}

func sha256Hex(s string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(s))) }

func (s *Store) runPresetStep(step PresetSeedStep) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var done string
	err = tx.QueryRow(`SELECT value FROM settings WHERE key = ?`, step.Marker).Scan(&done)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	for _, r := range step.Renames {
		if _, err := tx.Exec(`UPDATE presets SET name = ?, updated_at = ? WHERE key = ? AND name = ?`, r.To, now(), r.Key, r.From); err != nil {
			return err
		}
	}
	for _, p := range step.Adds {
		if _, err := tx.Exec(`INSERT INTO presets (key, name, description, owner_id, pr_instruction, pr_goal, pr_summary, issue_instruction, issue_goal, issue_summary, closing, no_citation_marks, no_tools, additional, run_defaults, created_at, updated_at)
			VALUES (?, ?, ?, NULL, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (key) DO NOTHING`, p.Key, p.Name, p.Description, p.PRInstruction, p.PRGoal, p.PRSummary, p.IssueInstruction, p.IssueGoal,
			p.IssueSummary, p.Closing, p.NoCitationMarks, p.NoTools, p.Additional, p.RunDefaults, now(), now()); err != nil {
			return err
		}
	}
	if u := step.ReplacePR; u != nil {
		if _, err := tx.Exec(`UPDATE presets SET pr_instruction = ?, updated_at = ? WHERE key = ? AND owner_id IS NULL AND pr_instruction = ?`, u.To, now(), u.Key, u.From); err != nil {
			return err
		}
	}
	for _, k := range step.SetDefaults {
		if _, err := tx.Exec(`UPDATE presets SET run_defaults = ?, updated_at = ? WHERE key = ? AND owner_id IS NULL AND run_defaults = ''`, step.RunDefaults, now(), k); err != nil {
			return err
		}
	}
	for _, k := range step.SetNoTools {
		if _, err := tx.Exec(`UPDATE presets SET no_tools = 1, updated_at = ? WHERE key = ? AND owner_id IS NULL`, now(), k); err != nil {
			return err
		}
	}
	for _, u := range step.Upgrades {
		if err := upgradePreset(tx, u); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO settings (key, value) VALUES (?, '1')`, step.Marker); err != nil {
		return err
	}
	return tx.Commit()
}

func upgradePreset(tx querier, u PresetUpgrade) error {
	var cur Preset
	err := tx.QueryRow(`SELECT description, pr_instruction, pr_goal, pr_summary, issue_instruction, issue_goal, issue_summary, run_defaults
		FROM presets WHERE key = ? AND owner_id IS NULL`, u.Key).
		Scan(&cur.Description, &cur.PRInstruction, &cur.PRGoal, &cur.PRSummary, &cur.IssueInstruction, &cur.IssueGoal, &cur.IssueSummary, &cur.RunDefaults)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	fields := []struct {
		col string
		cur string
		to  string
	}{
		{"description", cur.Description, u.To.Description},
		{"pr_instruction", cur.PRInstruction, u.To.PRInstruction},
		{"pr_goal", cur.PRGoal, u.To.PRGoal},
		{"pr_summary", cur.PRSummary, u.To.PRSummary},
		{"issue_instruction", cur.IssueInstruction, u.To.IssueInstruction},
		{"issue_goal", cur.IssueGoal, u.To.IssueGoal},
		{"issue_summary", cur.IssueSummary, u.To.IssueSummary},
		{"run_defaults", cur.RunDefaults, u.To.RunDefaults},
	}
	var sets []string
	var args []any
	for _, f := range fields {
		hashes := u.Match[f.col]
		if len(hashes) == 0 || f.cur == f.to {
			continue
		}
		h := sha256Hex(f.cur)
		matched := false
		for _, want := range hashes {
			if h == want {
				matched = true
				break
			}
		}
		if matched {
			sets = append(sets, f.col+" = ?")
			args = append(args, f.to)
		}
	}
	if len(sets) == 0 {
		return nil
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, now(), u.Key)
	_, err = tx.Exec(`UPDATE presets SET `+strings.Join(sets, ", ")+` WHERE key = ? AND owner_id IS NULL`, args...)
	return err
}

// StoredGraph is a workflow of a workspace, or a run's snapshot of one, with the kind it is of.
type StoredGraph struct {
	Workspace int64
	Kind      string
	Graph     string
}

// Graphs lists every workspace's saved workflows.
func (s *Store) Graphs() ([]StoredGraph, error) {
	rows, err := s.db.Query(`SELECT workspace_id, kind, graph FROM workflows ORDER BY workspace_id, kind`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredGraph
	for rows.Next() {
		var g StoredGraph
		if err := rows.Scan(&g.Workspace, &g.Kind, &g.Graph); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// RemovePreset removes a preset. Before it goes, rewrite is given every saved workflow, and every
// run still going with its snapshot of one, and returns the graph with the blocks that used the
// preset changed (and whether it changed anything); each changed workflow gets a new version, so an
// open editor is told. All of it is one transaction: a failure leaves the preset and the blocks as
// they were.
func (s *Store) RemovePreset(id int64, rewrite func(StoredGraph) (string, bool, error)) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var key string
	if err := tx.QueryRow(`SELECT key FROM presets WHERE id = ?`, id).Scan(&key); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	for _, table := range []struct{ name, where, set string }{
		{"workflows", "1 = 1", ", version = version + 1"},
		{"workflow_runs", "status IN ('running', 'queued')", ""},
	} {
		rows, err := tx.Query(`SELECT rowid, workspace_id, kind, graph FROM ` + table.name + ` WHERE ` + table.where)
		if err != nil {
			return err
		}
		type change struct {
			rowid int64
			graph string
		}
		var changes []change
		for rows.Next() {
			var rowid int64
			var g StoredGraph
			if err := rows.Scan(&rowid, &g.Workspace, &g.Kind, &g.Graph); err != nil {
				rows.Close()
				return err
			}
			if next, changed, err := rewrite(g); err != nil {
				rows.Close()
				return err
			} else if changed {
				changes = append(changes, change{rowid, next})
			}
		}
		rows.Close()
		for _, c := range changes {
			if _, err := tx.Exec(`UPDATE `+table.name+` SET graph = ?`+table.set+` WHERE rowid = ?`, c.graph, c.rowid); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(`DELETE FROM presets WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}
