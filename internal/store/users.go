package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// User is a person who can sign in. Admins create workspaces, manage users and can open every
// workspace; regular users open only the workspaces they are assigned to.
type User struct {
	ID          int64
	Username    string
	DisplayName string
	IsAdmin     bool
	Disabled    bool
	CreatedAt   time.Time
}

// Name is how the user is shown: the display name, or the username when there is none.
func (u User) Name() string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.Username
}

// SessionTTL is how long a sign-in lasts.
const SessionTTL = 30 * 24 * time.Hour

var usernameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

const userCols = `u.id, u.username, u.display_name, u.is_admin, u.disabled, u.created_at`

type scanner interface{ Scan(dest ...any) error }

func scanUser(row scanner, extra ...any) (*User, error) {
	var u User
	var created int64
	dest := append([]any{&u.ID, &u.Username, &u.DisplayName, &u.IsAdmin, &u.Disabled, &created}, extra...)
	if err := row.Scan(dest...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	u.CreatedAt = time.Unix(created, 0)
	return &u, nil
}

func hashPassword(pw string) (string, error) {
	if len(pw) < 8 {
		return "", invalid("Passwords need at least 8 characters.")
	}
	if len(pw) > 72 {
		return "", invalid("Passwords can be at most 72 bytes long.")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(h), err
}

// ErrSetupDone means the first admin has already been created.
var ErrSetupDone = errors.New("setup is already done")

// newUser checks a new user's username and password and returns the stored username and hash.
func newUser(username, password string) (string, string, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if !usernameRE.MatchString(username) {
		return "", "", invalid("Usernames are 1 to 32 lower-case letters, digits, dots, dashes or underscores.")
	}
	hash, err := hashPassword(password)
	return username, hash, err
}

func insertUser(q querier, username, displayName, hash string, admin bool) (int64, error) {
	res, err := q.Exec(`INSERT INTO users (username, display_name, password_hash, is_admin, created_at)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT (username) DO NOTHING`,
		username, strings.TrimSpace(displayName), hash, admin, now())
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, invalid("A user named %q already exists.", username)
	}
	return res.LastInsertId()
}

// CreateUser adds a user. Usernames are stored in lower case and are unique.
func (s *Store) CreateUser(username, displayName, password string, admin bool) (*User, error) {
	username, hash, err := newUser(username, password)
	if err != nil {
		return nil, err
	}
	id, err := insertUser(s.db, username, displayName, hash, admin)
	if err != nil {
		return nil, err
	}
	return s.User(id)
}

// HasUsers reports whether any user exists. Users are never deleted, so once true it stays true.
func (s *Store) HasUsers() (bool, error) {
	var has bool
	err := s.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM users)`).Scan(&has)
	return has, err
}

// CreateFirstAdmin creates an admin only while no user exists at all, and returns ErrSetupDone
// otherwise. The check and the insert are one statement, so of two simultaneous attempts exactly
// one succeeds.
func (s *Store) CreateFirstAdmin(username, displayName, password string) (*User, error) {
	username, hash, err := newUser(username, password)
	if err != nil {
		return nil, err
	}
	res, err := s.db.Exec(`INSERT INTO users (username, display_name, password_hash, is_admin, created_at)
		SELECT ?, ?, ?, 1, ? WHERE NOT EXISTS (SELECT 1 FROM users)`,
		username, strings.TrimSpace(displayName), hash, now())
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrSetupDone
	}
	id, _ := res.LastInsertId()
	return s.User(id)
}

// User looks a user up by id.
func (s *Store) User(id int64) (*User, error) {
	return scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users u WHERE u.id = ?`, id))
}

// UserByUsername looks a user up by username, ignoring case.
func (s *Store) UserByUsername(username string) (*User, error) {
	return scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users u WHERE u.username = ?`, strings.TrimSpace(username)))
}

// Users lists every user by username.
func (s *Store) Users() ([]User, error) {
	rows, err := s.db.Query(`SELECT ` + userCols + ` FROM users u ORDER BY u.username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// dummyHash is compared against when a username does not exist, so a wrong username takes as long
// to reject as a wrong password.
var dummyHash = sync.OnceValue(func() []byte {
	h, _ := bcrypt.GenerateFromPassword([]byte("autobot: no such user"), bcrypt.DefaultCost)
	return h
})

// CheckPassword returns the user when the username and password match an enabled account, and
// ErrNotFound otherwise (without saying which part was wrong).
func (s *Store) CheckPassword(username, password string) (*User, error) {
	var hash string
	u, err := scanUser(s.db.QueryRow(`SELECT `+userCols+`, u.password_hash FROM users u WHERE u.username = ?`,
		strings.TrimSpace(username)), &hash)
	if errors.Is(err, ErrNotFound) {
		bcrypt.CompareHashAndPassword(dummyHash(), []byte(password))
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil || u.Disabled {
		return nil, ErrNotFound
	}
	return u, nil
}

// SetPassword changes a user's password and signs out all of their sessions except keepToken
// (pass "" to sign out everywhere).
func (s *Store) SetPassword(userID int64, password, keepToken string) error {
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, hash, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(`DELETE FROM sessions WHERE user_id = ? AND token_hash <> ?`, userID, hashToken(keepToken)); err != nil {
		return err
	}
	return tx.Commit()
}

// UserUpdate is what an admin can change about a user in one save.
type UserUpdate struct {
	DisplayName string
	IsAdmin     bool
	Disabled    bool
	Workspaces  []int64 // the workspace IDs the user is assigned to, replacing the current set
	Password    string  // a new password, or "" to keep the current one
}

// UpdateUser applies an admin's changes to a user. It refuses to leave the system without an
// enabled admin, and records assignment changes in each workspace's history.
func (s *Store) UpdateUser(id int64, up UserUpdate, actor *User) error {
	var hash string
	if up.Password != "" {
		var err error
		if hash, err = hashPassword(up.Password); err != nil {
			return err
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	cur, err := scanUser(tx.QueryRow(`SELECT `+userCols+` FROM users u WHERE u.id = ?`, id))
	if err != nil {
		return err
	}
	if cur.IsAdmin && !cur.Disabled && (!up.IsAdmin || up.Disabled) {
		var others int
		if err := tx.QueryRow(`SELECT count(*) FROM users WHERE is_admin = 1 AND disabled = 0 AND id <> ?`, id).Scan(&others); err != nil {
			return err
		}
		if others == 0 {
			return invalid("%s is the only active admin. Make someone else an admin first.", cur.Name())
		}
	}
	if _, err := tx.Exec(`UPDATE users SET display_name = ?, is_admin = ?, disabled = ? WHERE id = ?`,
		strings.TrimSpace(up.DisplayName), up.IsAdmin, up.Disabled, id); err != nil {
		return err
	}
	if hash != "" {
		if _, err := tx.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, hash, id); err != nil {
			return err
		}
	}
	if up.Disabled || hash != "" {
		if _, err := tx.Exec(`DELETE FROM sessions WHERE user_id = ?`, id); err != nil {
			return err
		}
	}

	have, err := memberships(tx, id)
	if err != nil {
		return err
	}
	want := map[int64]bool{}
	for _, ws := range up.Workspaces {
		want[ws] = true
	}
	for ws := range want {
		if have[ws] {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO workspace_members (workspace_id, user_id) VALUES (?, ?)`, ws, id); err != nil {
			return err
		}
		if err := addEvent(tx, ws, actor, "assigned "+cur.Name()); err != nil {
			return err
		}
	}
	for ws := range have {
		if want[ws] {
			continue
		}
		if _, err := tx.Exec(`DELETE FROM workspace_members WHERE workspace_id = ? AND user_id = ?`, ws, id); err != nil {
			return err
		}
		if err := addEvent(tx, ws, actor, "unassigned "+cur.Name()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func memberships(q querier, userID int64) (map[int64]bool, error) {
	rows, err := q.Query(`SELECT workspace_id FROM workspace_members WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// Memberships returns the set of workspace IDs a user is assigned to.
func (s *Store) Memberships(userID int64) (map[int64]bool, error) { return memberships(s.db, userID) }

// Assignments maps each user ID to the names of the workspaces they are assigned to.
func (s *Store) Assignments() (map[int64][]string, error) {
	rows, err := s.db.Query(`SELECT m.user_id, w.name FROM workspace_members m
		JOIN workspaces w ON w.id = m.workspace_id ORDER BY w.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]string{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = append(out[id], name)
	}
	return out, rows.Err()
}

func newToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Only a hash of each session token is stored, so a copy of the database cannot be used to sign in.
func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

// CreateSession signs a user in and returns the token for their cookie.
func (s *Store) CreateSession(userID int64) (string, error) {
	t := now()
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at <= ?`, t); err != nil {
		return "", err
	}
	token := newToken()
	_, err := s.db.Exec(`INSERT INTO sessions (token_hash, user_id, csrf, expires_at) VALUES (?, ?, ?, ?)`,
		hashToken(token), userID, newToken(), t+int64(SessionTTL/time.Second))
	return token, err
}

// Session returns the signed-in user and the session's form token, or ErrNotFound when the token
// is unknown, expired, or belongs to a disabled user.
func (s *Store) Session(token string) (*User, string, error) {
	var csrf string
	u, err := scanUser(s.db.QueryRow(`SELECT `+userCols+`, s.csrf FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ? AND s.expires_at > ? AND u.disabled = 0`, hashToken(token), now()), &csrf)
	return u, csrf, err
}

// DeleteSession signs a session out.
func (s *Store) DeleteSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, hashToken(token))
	return err
}
