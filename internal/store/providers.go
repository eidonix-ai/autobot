package store

import (
	"database/sql"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ErrKeyUnreadable means a provider's saved API key cannot be decrypted, which happens when the key
// file has changed since it was saved. The fix is to save the API key again.
var ErrKeyUnreadable = errors.New("the saved API key cannot be decrypted with the current key; save it again")

// The two kinds of provider API Autobot speaks.
const (
	KindOpenAI    = "openai"    // any OpenAI-compatible API
	KindAnthropic = "anthropic" // the Anthropic API
)

// Provider is an inference provider. Its API key is never part of this value; ask for it with
// ProviderKey when a call needs it.
type Provider struct {
	ID         int64
	Name       string
	Kind       string
	Endpoint   string
	Auth       string // how Autobot signs in: "api_key"
	HasKey     bool
	ModelCount int      // how many models are permitted
	Models     []string // the permitted models, sorted; filled by Provider, not by Providers
	CreatedBy  string
	UpdatedAt  time.Time
}

// ProviderEdit is what a person enters for a provider. An empty Key keeps the saved key, unless
// RemoveKey is set.
type ProviderEdit struct {
	Name, Kind, Endpoint string
	Key                  string
	RemoveKey            bool
}

// A provider's API key is bound to its row, so a sealed value copied elsewhere will not open.
func providerKeyLabel(id int64) []byte {
	return []byte("autobot provider api key " + strconv.FormatInt(id, 10))
}

// NormalizeEndpoint checks a base URL and drops a trailing slash.
func NormalizeEndpoint(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return "", invalid("The endpoint must be a full web address such as https://openrouter.ai/api/v1.")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", invalid("The endpoint must be just the base address, with no user name, ? part or # part.")
	}
	return strings.TrimRight(raw, "/"), nil
}

// checkProviderEdit checks everything a person entered and returns it tidied.
func checkProviderEdit(e ProviderEdit) (ProviderEdit, error) {
	e.Name, e.Key = strings.TrimSpace(e.Name), strings.TrimSpace(e.Key)
	if e.Name == "" || utf8.RuneCountInString(e.Name) > 100 || strings.ContainsFunc(e.Name, unicode.IsControl) {
		return e, invalid("Give the provider a name of up to 100 characters, such as OpenRouter.")
	}
	if e.Kind != KindOpenAI && e.Kind != KindAnthropic {
		return e, invalid("Choose the kind of API: OpenAI-compatible or Anthropic.")
	}
	var err error
	if e.Endpoint, err = NormalizeEndpoint(e.Endpoint); err != nil {
		return e, err
	}
	if strings.ContainsFunc(e.Key, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return e, invalid("The API key cannot contain spaces or line breaks.")
	}
	return e, nil
}

// ValidModelID reports whether id is 1 to 200 printable characters with no spaces.
func ValidModelID(id string) bool {
	if id == "" || utf8.RuneCountInString(id) > 200 {
		return false
	}
	return !strings.ContainsFunc(id, func(r rune) bool { return !unicode.IsPrint(r) || unicode.IsSpace(r) })
}

const providerCols = `p.id, p.name, p.kind, p.endpoint, p.auth, length(p.api_key) > 0, ` + authorName + `, p.updated_at`

func scanProvider(row scanner, extra ...any) (*Provider, error) {
	var p Provider
	var at int64
	dest := append([]any{&p.ID, &p.Name, &p.Kind, &p.Endpoint, &p.Auth, &p.HasKey, &p.CreatedBy, &at}, extra...)
	if err := row.Scan(dest...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	p.UpdatedAt = time.Unix(at, 0)
	return &p, nil
}

// Providers lists every provider by name, with how many models each permits.
func (s *Store) Providers() ([]*Provider, error) {
	rows, err := s.db.Query(`SELECT ` + providerCols + `, (SELECT count(*) FROM provider_models m WHERE m.provider_id = p.id)
		FROM providers p LEFT JOIN users u ON u.id = p.created_by ORDER BY p.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Provider
	for rows.Next() {
		var count int
		p, err := scanProvider(rows, &count)
		if err != nil {
			return nil, err
		}
		p.ModelCount = count
		out = append(out, p)
	}
	return out, rows.Err()
}

// Provider returns one provider with its permitted models, or ErrNotFound.
func (s *Store) Provider(id int64) (*Provider, error) {
	p, err := scanProvider(s.db.QueryRow(`SELECT `+providerCols+` FROM providers p
		LEFT JOIN users u ON u.id = p.created_by WHERE p.id = ?`, id))
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT model_id FROM provider_models WHERE provider_id = ? ORDER BY model_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		p.Models = append(p.Models, m)
	}
	p.ModelCount = len(p.Models)
	return p, rows.Err()
}

// ProviderKey returns a provider's API key, or "" when it has none.
func (s *Store) ProviderKey(id int64) (string, error) {
	var sealed []byte
	err := s.db.QueryRow(`SELECT api_key FROM providers WHERE id = ?`, id).Scan(&sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil || len(sealed) == 0 {
		return "", err
	}
	key, err := s.unsealWith(providerKeyLabel(id), sealed)
	if errors.Is(err, errUnreadable) {
		return "", ErrKeyUnreadable
	}
	return key, err
}

// nameTaken reports whether a provider other than exceptID already has the name.
func nameTaken(q querier, name string, exceptID int64) (bool, error) {
	var taken bool
	err := q.QueryRow(`SELECT EXISTS (SELECT 1 FROM providers WHERE name = ? AND id <> ?)`, name, exceptID).Scan(&taken)
	return taken, err
}

// CreateProvider adds a provider with no permitted models yet.
func (s *Store) CreateProvider(e ProviderEdit, actor *User) (*Provider, error) {
	e, err := checkProviderEdit(e)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if taken, err := nameTaken(tx, e.Name, 0); err != nil {
		return nil, err
	} else if taken {
		return nil, invalid("A provider named %s already exists.", e.Name)
	}
	res, err := tx.Exec(`INSERT INTO providers (name, kind, endpoint, created_by, updated_at) VALUES (?, ?, ?, ?, ?)`,
		e.Name, e.Kind, e.Endpoint, actor.ID, now())
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	if e.Key != "" {
		if err := s.saveProviderKey(tx, id, e.Key); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.Provider(id)
}

// saveProviderKey seals key for the provider and stores it.
func (s *Store) saveProviderKey(q querier, id int64, key string) error {
	sealed, err := s.sealWith(providerKeyLabel(id), key)
	if err != nil {
		return err
	}
	_, err = q.Exec(`UPDATE providers SET api_key = ? WHERE id = ?`, sealed, id)
	return err
}

// UpdateProvider changes a provider's connection settings.
func (s *Store) UpdateProvider(id int64, e ProviderEdit) error {
	e, err := checkProviderEdit(e)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if taken, err := nameTaken(tx, e.Name, id); err != nil {
		return err
	} else if taken {
		return invalid("Another provider is already named %s.", e.Name)
	}
	res, err := tx.Exec(`UPDATE providers SET name = ?, kind = ?, endpoint = ?, updated_at = ? WHERE id = ?`,
		e.Name, e.Kind, e.Endpoint, now(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	switch {
	case e.Key != "":
		err = s.saveProviderKey(tx, id, e.Key)
	case e.RemoveKey:
		_, err = tx.Exec(`UPDATE providers SET api_key = x'' WHERE id = ?`, id)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// SetProviderModels replaces the set of models Autobot may use from a provider.
func (s *Store) SetProviderModels(id int64, models []string) error {
	seen := map[string]bool{}
	var clean []string
	for _, m := range models {
		m = strings.TrimSpace(m)
		if !ValidModelID(m) {
			return invalid("%q is not a model ID: use 1 to 200 characters with no spaces.", m)
		}
		if !seen[m] {
			seen[m] = true
			clean = append(clean, m)
		}
	}
	sort.Strings(clean)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE providers SET updated_at = ? WHERE id = ?`, now(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(`DELETE FROM provider_models WHERE provider_id = ?`, id); err != nil {
		return err
	}
	for _, m := range clean {
		if _, err := tx.Exec(`INSERT INTO provider_models (provider_id, model_id) VALUES (?, ?)`, id, m); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteProvider removes a provider and its permitted models.
func (s *Store) DeleteProvider(id int64) error {
	res, err := s.db.Exec(`DELETE FROM providers WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
