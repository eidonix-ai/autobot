package store

import (
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
)

// Each workspace has its own Discord bot, installed on its project's own server (Derek,
// 2026-11-23): its token (sealed to the workspace, like every secret), its application's public
// key (which checks that a click came from Discord), and the forum its cases are posted in. Any
// member of the workspace sets them up.

func discordTokenLabel(workspaceID int64) []byte {
	return []byte("autobot discord bot token " + strconv.FormatInt(workspaceID, 10))
}

func discordKey(workspaceID int64, what string) string {
	return "discord/" + strconv.FormatInt(workspaceID, 10) + "/" + what
}

// SetDiscordBot saves a workspace's bot: its token and public key. An empty token keeps the saved one.
func (s *Store) SetDiscordBot(ws *Workspace, token, publicKey string, actor *User) error {
	if token = strings.TrimSpace(token); token != "" {
		sealed, err := s.sealWith(discordTokenLabel(ws.ID), token)
		if err != nil {
			return err
		}
		if err := s.SetSetting(discordKey(ws.ID, "token"), base64.StdEncoding.EncodeToString(sealed)); err != nil {
			return err
		}
	}
	if err := s.SetSetting(discordKey(ws.ID, "public_key"), strings.ToLower(strings.TrimSpace(publicKey))); err != nil {
		return err
	}
	return addEvent(s.db, ws.ID, actor, "set up the Discord bot")
}

// DiscordBot returns a workspace's bot token and public key, or ErrNotFound when it has none.
func (s *Store) DiscordBot(workspaceID int64) (token, publicKey string, err error) {
	v, err := s.Setting(discordKey(workspaceID, "token"))
	if err != nil {
		return "", "", err
	}
	sealed, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return "", "", err
	}
	token, err = s.unsealWith(discordTokenLabel(workspaceID), sealed)
	if errors.Is(err, errUnreadable) {
		return "", "", errors.New("the saved bot token cannot be decrypted with the current key; save it again")
	}
	if err != nil {
		return "", "", err
	}
	publicKey, _ = s.Setting(discordKey(workspaceID, "public_key"))
	return token, publicKey, nil
}

// RemoveDiscordBot forgets a workspace's bot and its forum.
func (s *Store) RemoveDiscordBot(ws *Workspace, actor *User) error {
	_, err := s.db.Exec(`DELETE FROM settings WHERE key IN (?, ?, ?)`,
		discordKey(ws.ID, "token"), discordKey(ws.ID, "public_key"), discordKey(ws.ID, "forum"))
	if err != nil {
		return err
	}
	return addEvent(s.db, ws.ID, actor, "removed the Discord bot")
}

// SetDiscordForum sets the forum a workspace's cases are posted in ("" for none), with its name to show.
func (s *Store) SetDiscordForum(ws *Workspace, channel, name string, actor *User) error {
	if err := s.SetSetting(discordKey(ws.ID, "forum"), channel+"\t"+name); err != nil {
		return err
	}
	what := "stopped posting to Discord"
	if channel != "" {
		what = "set the Discord forum to #" + name
	}
	return addEvent(s.db, ws.ID, actor, what)
}

// DiscordForum returns the forum a workspace's cases are posted in, or "" for none.
func (s *Store) DiscordForum(workspaceID int64) (channel, name string) {
	v, err := s.Setting(discordKey(workspaceID, "forum"))
	if err != nil {
		return "", ""
	}
	channel, name, _ = strings.Cut(v, "\t")
	return channel, name
}
