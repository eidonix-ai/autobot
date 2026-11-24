package store

import (
	"database/sql"
	"errors"
	"net/url"
	"strconv"
	"strings"
)

// Setting returns an instance setting, or ErrNotFound when it was never set.
func (s *Store) Setting(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

// SetSetting stores an instance setting.
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// TrustedEndpoints is the setting that lists the hosts providers may use on Autobot's own network.
const TrustedEndpoints = "trusted_endpoints"

// TrustedHosts returns the hosts providers may use on the local network. The first time, before
// the rule existed, it starts as the hosts of the providers already saved, so none stops working.
func (s *Store) TrustedHosts() ([]string, error) {
	v, err := s.Setting(TrustedEndpoints)
	if errors.Is(err, ErrNotFound) {
		rows, err := s.db.Query(`SELECT DISTINCT endpoint FROM providers`)
		if err != nil {
			return nil, err
		}
		var hosts []string
		for rows.Next() {
			var e string
			if rows.Scan(&e) == nil {
				if u, err := url.Parse(e); err == nil && u.Host != "" {
					hosts = append(hosts, strings.ToLower(u.Host))
				}
			}
		}
		rows.Close()
		v = strings.Join(hosts, "\n")
		if err := s.SetSetting(TrustedEndpoints, v); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	var out []string
	for _, h := range strings.Split(v, "\n") {
		if h = strings.TrimSpace(h); h != "" {
			out = append(out, h)
		}
	}
	return out, nil
}

// DiscordPost returns a case's forum post and its summary message, or "" when it has none.
func (s *Store) DiscordPost(caseUUID string) (thread, summary string) {
	s.db.QueryRow(`SELECT thread_id, summary_id FROM discord_posts WHERE case_uuid = ?`, caseUUID).Scan(&thread, &summary)
	return thread, summary
}

// SetDiscordPost records a case's forum post.
func (s *Store) SetDiscordPost(caseUUID, thread, summary string) error {
	_, err := s.db.Exec(`INSERT INTO discord_posts (case_uuid, thread_id, summary_id) VALUES (?, ?, ?)
		ON CONFLICT (case_uuid) DO UPDATE SET thread_id = excluded.thread_id, summary_id = excluded.summary_id`, caseUUID, thread, summary)
	return err
}

// SetSettingIfEmpty stores an instance setting only when it has none (or an empty one), as one
// step, so of several requests racing to fill it exactly one wins. It reports whether this call stored it.
func (s *Store) SetSettingIfEmpty(key, value string) (bool, error) {
	res, err := s.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value WHERE settings.value = ''`, key, value)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func optOutKey(workspaceID int64) string { return "opt_out/" + strconv.FormatInt(workspaceID, 10) }

// OptOutTag returns a workspace's opt-out tag: what the author of a pull request or issue, or a
// maintainer, writes to keep Autobot off it. "" means no opt-out. ErrNotFound means it was never set.
func (s *Store) OptOutTag(workspaceID int64) (string, error) {
	return s.Setting(optOutKey(workspaceID))
}

// SetOptOutTag sets a workspace's opt-out tag ("" for none).
func (s *Store) SetOptOutTag(ws *Workspace, tag string, actor *User) error {
	tag = strings.TrimSpace(tag)
	if err := s.SetSetting(optOutKey(ws.ID), tag); err != nil {
		return err
	}
	what := "removed the opt-out tag"
	if tag != "" {
		what = "set the opt-out tag to “" + tag + "”"
	}
	return addEvent(s.db, ws.ID, actor, what)
}
