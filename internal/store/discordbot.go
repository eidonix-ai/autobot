package store

import (
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
)

// The Discord bot is one for the whole instance, set by an admin: its token (sealed, like every
// secret) and its application's public key, which checks that a click came from Discord. Each
// workspace picks the forum its cases are posted in.

var discordTokenLabel = []byte("autobot discord bot token")

const (
	discordToken  = "discord_bot_token"
	discordPubKey = "discord_public_key"
)

// SetDiscordBot saves the bot's token and public key. An empty token keeps the saved one.
func (s *Store) SetDiscordBot(token, publicKey string) error {
	if token = strings.TrimSpace(token); token != "" {
		sealed, err := s.sealWith(discordTokenLabel, token)
		if err != nil {
			return err
		}
		if err := s.SetSetting(discordToken, base64.StdEncoding.EncodeToString(sealed)); err != nil {
			return err
		}
	}
	return s.SetSetting(discordPubKey, strings.ToLower(strings.TrimSpace(publicKey)))
}

// DiscordBot returns the bot's token and public key, or ErrNotFound when no bot is set.
func (s *Store) DiscordBot() (token, publicKey string, err error) {
	v, err := s.Setting(discordToken)
	if err != nil {
		return "", "", err
	}
	sealed, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return "", "", err
	}
	token, err = s.unsealWith(discordTokenLabel, sealed)
	if errors.Is(err, errUnreadable) {
		return "", "", errors.New("the saved bot token cannot be decrypted with the current key; save it again")
	}
	if err != nil {
		return "", "", err
	}
	publicKey, _ = s.Setting(discordPubKey)
	return token, publicKey, nil
}

// RemoveDiscordBot forgets the bot.
func (s *Store) RemoveDiscordBot() error {
	_, err := s.db.Exec(`DELETE FROM settings WHERE key IN (?, ?)`, discordToken, discordPubKey)
	return err
}

func forumKey(workspaceID int64) string { return "discord_forum/" + strconv.FormatInt(workspaceID, 10) }

// SetDiscordForum sets the forum a workspace's cases are posted in ("" for none), with its name to show.
func (s *Store) SetDiscordForum(ws *Workspace, channel, name string, actor *User) error {
	if err := s.SetSetting(forumKey(ws.ID), channel+"\t"+name); err != nil {
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
	v, err := s.Setting(forumKey(workspaceID))
	if err != nil {
		return "", ""
	}
	channel, name, _ = strings.Cut(v, "\t")
	return channel, name
}
