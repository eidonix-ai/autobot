package store

import (
	"database/sql"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// DiscordChannel is a channel a workspace's workflows can post to, through a Discord webhook. Its
// webhook address is never part of this value; ask for it with DiscordWebhook when posting.
type DiscordChannel struct {
	ID          int64
	WorkspaceID int64
	Name        string
	WebhookID   string // the webhook's number, safe to show
	AddedBy     string
	AddedAt     time.Time
}

// webhookRE is a Discord webhook address. Only Discord's own hosts are accepted, so a saved
// "webhook" can never point Autobot at another site or at its own network.
var webhookRE = regexp.MustCompile(`^https://(?:(?:ptb|canary)\.)?discord(?:app)?\.com/api/(?:v\d{1,2}/)?webhooks/(\d{5,30})/[A-Za-z0-9_-]{20,120}$`)

// WebhookID checks a Discord webhook address and returns its number.
func WebhookID(url string) (string, error) {
	m := webhookRE.FindStringSubmatch(strings.TrimSpace(url))
	if m == nil {
		return "", invalid("That is not a Discord webhook address. In Discord: channel settings, Integrations, Webhooks, Copy Webhook URL.")
	}
	return m[1], nil
}

func discordLabel(id int64) []byte {
	return []byte("autobot discord webhook " + strconv.FormatInt(id, 10))
}

// AddDiscordChannel saves a channel under a name of the workspace's choosing.
func (s *Store) AddDiscordChannel(ws *Workspace, name, webhook string, actor *User) (*DiscordChannel, error) {
	name = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(name), "#"))
	if name == "" || utf8.RuneCountInString(name) > 60 || strings.ContainsFunc(name, unicode.IsControl) {
		return nil, invalid("Give the channel a name of up to 60 characters, such as reviews.")
	}
	webhook = strings.TrimSpace(webhook)
	hookID, err := WebhookID(webhook)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var taken bool
	if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM discord_channels WHERE workspace_id = ? AND name = ?)`, ws.ID, name).Scan(&taken); err != nil {
		return nil, err
	}
	if taken {
		return nil, invalid("This workspace already has a Discord channel named %s.", name)
	}
	res, err := tx.Exec(`INSERT INTO discord_channels (workspace_id, name, webhook_id, webhook, added_by, added_at) VALUES (?, ?, ?, x'', ?, ?)`,
		ws.ID, name, hookID, actor.ID, now())
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	sealed, err := s.sealWith(discordLabel(id), webhook)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE discord_channels SET webhook = ? WHERE id = ?`, sealed, id); err != nil {
		return nil, err
	}
	if err := addEvent(tx, ws.ID, actor, "added the Discord channel "+name); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &DiscordChannel{ID: id, WorkspaceID: ws.ID, Name: name, WebhookID: hookID}, nil
}

// DiscordChannels lists a workspace's channels by name.
func (s *Store) DiscordChannels(workspaceID int64) ([]DiscordChannel, error) {
	rows, err := s.db.Query(`SELECT d.id, d.workspace_id, d.name, d.webhook_id, `+authorName+`, d.added_at FROM discord_channels d
		LEFT JOIN users u ON u.id = d.added_by WHERE d.workspace_id = ? ORDER BY d.name`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DiscordChannel
	for rows.Next() {
		var c DiscordChannel
		var at int64
		if err := rows.Scan(&c.ID, &c.WorkspaceID, &c.Name, &c.WebhookID, &c.AddedBy, &at); err != nil {
			return nil, err
		}
		c.AddedAt = time.Unix(at, 0)
		out = append(out, c)
	}
	return out, rows.Err()
}

// DiscordWebhook returns the webhook address of one of the workspace's channels, or ErrNotFound
// (also for another workspace's channel).
func (s *Store) DiscordWebhook(workspaceID, id int64) (string, error) {
	var sealed []byte
	err := s.db.QueryRow(`SELECT webhook FROM discord_channels WHERE id = ? AND workspace_id = ?`, id, workspaceID).Scan(&sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	hook, err := s.unsealWith(discordLabel(id), sealed)
	if errors.Is(err, errUnreadable) {
		return "", errors.New("the saved webhook cannot be decrypted with the current key; add the channel again")
	}
	return hook, err
}

// RemoveDiscordChannel removes one of the workspace's channels.
func (s *Store) RemoveDiscordChannel(ws *Workspace, id int64, actor *User) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var name string
	if err := tx.QueryRow(`SELECT name FROM discord_channels WHERE id = ? AND workspace_id = ?`, id, ws.ID).Scan(&name); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM discord_channels WHERE id = ?`, id); err != nil {
		return err
	}
	if err := addEvent(tx, ws.ID, actor, "removed the Discord channel "+name); err != nil {
		return err
	}
	return tx.Commit()
}
