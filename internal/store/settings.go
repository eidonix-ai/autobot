package store

import (
	"database/sql"
	"errors"
	"net/url"
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
