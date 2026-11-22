package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"
)

// ErrTokenUnreadable means a stored token cannot be decrypted, which happens when the key file has
// changed since it was saved. The fix is to save the token again.
var ErrTokenUnreadable = errors.New("the saved GitHub token cannot be decrypted with the current key; save it again")

var errNoKey = errors.New("no encryption key is configured, so GitHub tokens cannot be stored")

// UseKey sets the 32-byte key that seals GitHub tokens. Call it once, before serving.
func (s *Store) UseKey(key []byte) error {
	if len(key) != 32 {
		return errors.New("the encryption key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	s.aead, err = cipher.NewGCM(block)
	return err
}

// Each sealed token is bound to its workspace, so a sealed value copied to another row will not open.
func tokenLabel(workspaceID int64) []byte {
	return []byte("autobot workspace github token " + strconv.FormatInt(workspaceID, 10))
}

func (s *Store) seal(workspaceID int64, token string) ([]byte, error) {
	if s.aead == nil {
		return nil, errNoKey
	}
	nonce := make([]byte, s.aead.NonceSize())
	rand.Read(nonce)
	return s.aead.Seal(nonce, nonce, []byte(token), tokenLabel(workspaceID)), nil
}

func (s *Store) unseal(workspaceID int64, sealed []byte) (string, error) {
	if s.aead == nil {
		return "", errNoKey
	}
	n := s.aead.NonceSize()
	if len(sealed) < n {
		return "", ErrTokenUnreadable
	}
	plain, err := s.aead.Open(nil, sealed[:n], sealed[n:], tokenLabel(workspaceID))
	if err != nil {
		return "", ErrTokenUnreadable
	}
	return string(plain), nil
}

// GitHubIdentity is the GitHub user Autobot acts as in a workspace.
type GitHubIdentity struct {
	Login   string
	Token   string // empty when the token could not be decrypted (see Err)
	SavedBy string
	SavedAt time.Time
	Err     error // ErrTokenUnreadable when the key no longer opens the token
}

// GitHub returns a workspace's GitHub identity, or ErrNotFound when it has none.
func (s *Store) GitHub(workspaceID int64) (*GitHubIdentity, error) {
	var id GitHubIdentity
	var sealed []byte
	var at int64
	err := s.db.QueryRow(`SELECT g.login, g.token, `+authorName+`, g.saved_at FROM workspace_github g
		LEFT JOIN users u ON u.id = g.saved_by WHERE g.workspace_id = ?`, workspaceID).Scan(&id.Login, &sealed, &id.SavedBy, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	id.SavedAt = time.Unix(at, 0)
	id.Token, id.Err = s.unseal(workspaceID, sealed)
	return &id, nil
}

func (s *Store) setGitHub(q querier, workspaceID int64, login, token string, actor *User) error {
	login, token = strings.TrimSpace(login), strings.TrimSpace(token)
	sealed, err := s.seal(workspaceID, token)
	if err != nil {
		return err
	}
	if _, err := q.Exec(`INSERT INTO workspace_github (workspace_id, login, token, saved_by, saved_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (workspace_id) DO UPDATE SET login = excluded.login, token = excluded.token,
		saved_by = excluded.saved_by, saved_at = excluded.saved_at`,
		workspaceID, login, sealed, actor.ID, now()); err != nil {
		return err
	}
	return addEvent(q, workspaceID, actor, "set the GitHub identity to "+login)
}

// SetGitHub saves the GitHub user and token Autobot uses in a workspace, replacing any before.
// Check them with GitHub first; this only stores them.
func (s *Store) SetGitHub(workspaceID int64, login, token string, actor *User) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.setGitHub(tx, workspaceID, login, token, actor); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveGitHub deletes a workspace's GitHub identity.
func (s *Store) RemoveGitHub(workspaceID int64, actor *User) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`DELETE FROM workspace_github WHERE workspace_id = ?`, workspaceID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	if err := addEvent(tx, workspaceID, actor, "removed the GitHub identity"); err != nil {
		return err
	}
	return tx.Commit()
}
