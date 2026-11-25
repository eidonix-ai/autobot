package store

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// BacklogConfig controls idle backlog catch-up for one workspace and workflow kind (pr or issue).
// When enabled, Autobot starts reviews on open items that have no case file yet whenever the
// workspace has no queued/waiting runs and fewer than MaxSlots active runs.
type BacklogConfig struct {
	Enabled  bool   `json:"enabled"`
	NodeID   string `json:"node_id"`   // event block ID on that kind's canvas
	Order    string `json:"order"`     // "newest" (default) or "oldest"
	MaxSlots int    `json:"max_slots"` // 1..MaxMaxRuns: active runs cap for catch-up
}

// BacklogEntry is an enabled BacklogConfig with its workspace and kind.
type BacklogEntry struct {
	WorkspaceID int64
	Kind        string
	Config      BacklogConfig
}

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

func backlogKey(wsID int64, kind string) string {
	return fmt.Sprintf("backlog/%d/%s", wsID, kind)
}

func normalizeBacklog(c BacklogConfig) BacklogConfig {
	if c.Order != "oldest" {
		c.Order = "newest"
	}
	if c.MaxSlots < 1 {
		c.MaxSlots = 1
	}
	if c.MaxSlots > MaxMaxRuns {
		c.MaxSlots = MaxMaxRuns
	}
	return c
}

// BacklogConfig returns a workspace's backlog catch-up settings for kind (pr or issue).
func (s *Store) BacklogConfig(wsID int64, kind string) BacklogConfig {
	raw, err := s.Setting(backlogKey(wsID, kind))
	if err != nil || raw == "" {
		return normalizeBacklog(BacklogConfig{})
	}
	var c BacklogConfig
	if json.Unmarshal([]byte(raw), &c) != nil {
		return normalizeBacklog(BacklogConfig{})
	}
	return normalizeBacklog(c)
}

// SetBacklogConfig validates and stores a workspace's backlog catch-up settings for kind.
func (s *Store) SetBacklogConfig(ws *Workspace, kind string, cfg BacklogConfig, actor *User) error {
	if kind != "pr" && kind != "issue" {
		return invalid("backlog catch-up is for pull requests or issues")
	}
	if cfg.Order == "" {
		cfg.Order = "newest"
	}
	if cfg.Order != "newest" && cfg.Order != "oldest" {
		return invalid("choose newest first or oldest first")
	}
	maxRuns := s.MaxRuns(ws.ID)
	if cfg.MaxSlots < 1 || cfg.MaxSlots > maxRuns {
		return invalid("idle slots must be between 1 and %d (this workspace's Runs at once limit)", maxRuns)
	}
	if cfg.Enabled && strings.TrimSpace(cfg.NodeID) == "" {
		return invalid("choose an event block to run")
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := s.SetSetting(backlogKey(ws.ID, kind), string(b)); err != nil {
		return err
	}
	what := "pull requests"
	if kind == "issue" {
		what = "issues"
	}
	msg := fmt.Sprintf("disabled backlog catch-up for %s", what)
	if cfg.Enabled {
		msg = fmt.Sprintf("enabled backlog catch-up for %s (%s first, up to %d idle)", what, cfg.Order, cfg.MaxSlots)
	}
	return addEvent(s.db, ws.ID, actor, msg)
}

// EnabledBacklogs lists every workspace/kind with backlog catch-up switched on, PRs before issues.
func (s *Store) EnabledBacklogs() ([]BacklogEntry, error) {
	rows, err := s.db.Query(`SELECT key, value FROM settings WHERE key LIKE 'backlog/%' ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var prs, issues []BacklogEntry
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		parts := strings.Split(k, "/")
		if len(parts) != 3 {
			continue
		}
		wsID, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || wsID <= 0 {
			continue
		}
		kind := parts[2]
		if kind != "pr" && kind != "issue" {
			continue
		}
		var cfg BacklogConfig
		if json.Unmarshal([]byte(v), &cfg) != nil || !cfg.Enabled || cfg.NodeID == "" {
			continue
		}
		e := BacklogEntry{WorkspaceID: wsID, Kind: kind, Config: normalizeBacklog(cfg)}
		if kind == "pr" {
			prs = append(prs, e)
		} else {
			issues = append(issues, e)
		}
	}
	return append(prs, issues...), rows.Err()
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
