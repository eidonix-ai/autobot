package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eidonix-ai/autobot/internal/github"
	"github.com/eidonix-ai/autobot/internal/store"
)

// SecretPath is where a private repository keeps the secret that lets contributors' agents read
// its case files. Its owner commits it; Autobot only reads it.
const SecretPath = ".github/autobot-secret.txt"

// repoAccess remembers, for a few minutes, whether a workspace's repository is private and the
// secret it holds, so the case API does not ask GitHub on every request.
type repoAccess struct {
	mu   sync.Mutex
	byWS map[int64]repoFacts
}

type repoFacts struct {
	private  bool
	readable bool   // Autobot can read the repository (and so its secret file)
	secret   string // "" when the repository has none Autobot can read
	at       time.Time
}

const repoFactsTTL = 5 * time.Minute

// facts returns what is known about a workspace's repository. Anything uncertain counts as private
// with no secret, so a case is never served by mistake.
func (s *Server) facts(ctx context.Context, ws *store.Workspace) repoFacts {
	s.access.mu.Lock()
	f, ok := s.access.byWS[ws.ID]
	s.access.mu.Unlock()
	if ok && time.Since(f.at) < repoFactsTTL {
		return f
	}
	f = repoFacts{private: true, at: time.Now()}
	token, err := s.token(ws)
	switch {
	case err != nil:
	case token == "":
		// Without a token Autobot sees only public repositories.
		if _, err := s.gh.RepoName(ctx, "", ws.Name); err == nil {
			f.private = false
		}
	default:
		if private, err := s.gh.Private(ctx, token, ws.Name); err == nil {
			f.private, f.readable = private, true
		}
		if f.private {
			if secret, err := s.gh.File(ctx, token, ws.Name, SecretPath); err == nil {
				f.secret = strings.TrimSpace(secret)
			}
		}
	}
	s.access.mu.Lock()
	s.access.byWS[ws.ID] = f
	s.access.mu.Unlock()
	return f
}

// signedIn returns the signed-in user, or nil, without sending anyone to the sign-in page.
func (s *Server) signedIn(r *http.Request) *store.User {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return nil
	}
	u, _, err := s.st.Session(c.Value)
	if err != nil {
		return nil
	}
	return u
}

// caseReadable decides whether the case API may serve a workspace's case to this request: always
// for a public repository; for a private one, to a signed-in user of the workspace, or to a
// request carrying the repository's secret as a bearer token. It also returns what it learned.
func (s *Server) caseReadable(r *http.Request, ws *store.Workspace) (bool, repoFacts) {
	if u := s.signedIn(r); u != nil {
		if ok, err := s.st.CanOpen(u, ws.ID); err == nil && ok {
			return true, repoFacts{}
		}
	}
	f := s.facts(r.Context(), ws)
	if !f.private {
		return true, f
	}
	given, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || len(f.secret) < minSecret {
		return false, f
	}
	// Compare hashes, so the comparison takes the same time whatever was sent.
	a, b := sha256.Sum256([]byte(strings.TrimSpace(given))), sha256.Sum256([]byte(f.secret))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1, f
}

const minSecret = 32

// apiCase serves a case file as Markdown, by its uuid, for contributors' agents. An unknown uuid
// gets a bare 404, so guessing teaches nothing. A real case of a private repository, asked for
// without the right secret, gets a 401 with instructions an agent can follow to fix it; the uuid
// is unguessable, so that confirms the case only to someone who already holds its link. The
// instructions never name the repository.
func (s *Server) apiCase(w http.ResponseWriter, r *http.Request) {
	c, err := s.st.Case(r.PathValue("uuid"))
	if err != nil {
		http.Error(w, "No such case.", http.StatusNotFound)
		return
	}
	ws, err := s.st.WorkspaceByID(c.WorkspaceID)
	if err != nil {
		http.Error(w, "No such case.", http.StatusNotFound)
		return
	}
	ok, f := s.caseReadable(r, ws)
	if !ok {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("WWW-Authenticate", `Bearer realm="autobot case file"`)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(secretHelp(f)))
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/markdown; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Robots-Tag", "noindex")
	w.Write([]byte(c.Contents))
}

// openCase resolves the case in a workspace page's URL.
func (s *Server) openCase(w http.ResponseWriter, r *http.Request) (*store.Workspace, *store.Case, bool) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return nil, nil, false
	}
	n, _ := strconv.Atoi(r.PathValue("number"))
	c, err := s.st.CaseByNumber(ws.ID, n)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return nil, nil, false
	}
	if err != nil {
		s.fail(w, err)
		return nil, nil, false
	}
	return ws, c, true
}

func casePath(ws *store.Workspace, c *store.Case) string {
	return ws.Path() + "/cases/" + strconv.Itoa(c.Number)
}

func (s *Server) casePage(w http.ResponseWriter, r *http.Request) {
	ws, c, ok := s.openCase(w, r)
	if !ok {
		return
	}
	contributions, err := s.st.Contributions(c.UUID)
	if err != nil {
		s.fail(w, err)
		return
	}
	var html bytes.Buffer
	if err := s.md.Convert([]byte(c.Contents), &html); err != nil {
		s.fail(w, err)
		return
	}
	newest := make([]store.Contribution, len(contributions))
	for i, x := range contributions {
		newest[len(contributions)-1-i] = x
	}
	data := map[string]any{"Title": c.Label() + " · " + ws.Name, "WS": ws, "Case": c, "Newest": newest,
		"HTML": template.HTML(html.String()), "API": publicURL(r) + "/api/v1/cases/" + c.UUID, "Private": s.facts(r.Context(), ws).private}
	if r.URL.Query().Has("saved") {
		data["Notice"] = "Saved."
	}
	s.render(w, r, http.StatusOK, "case.html", data)
}

func (s *Server) caseEdit(w http.ResponseWriter, r *http.Request) {
	ws, c, ok := s.openCase(w, r)
	if !ok {
		return
	}
	s.render(w, r, http.StatusOK, "case_edit.html", map[string]any{"Title": "Edit " + c.Label() + " · " + ws.Name,
		"WS": ws, "Case": c, "Body": c.Contents, "BaseVersion": c.Version, "Path": casePath(ws, c)})
}

func (s *Server) caseSave(w http.ResponseWriter, r *http.Request) {
	ws, c, ok := s.openCase(w, r)
	if !ok {
		return
	}
	base, _ := strconv.Atoi(r.PostForm.Get("base_version"))
	body := r.PostForm.Get("body")
	err := s.st.EditCase(c.UUID, base, body, me(r))
	data := map[string]any{"Title": "Edit " + c.Label() + " · " + ws.Name, "WS": ws, "Case": c, "Body": body, "BaseVersion": base, "Path": casePath(ws, c)}
	var conflict *store.ConflictingEdit
	switch {
	case errors.As(err, &conflict):
		data["Conflict"], data["BaseVersion"] = conflict.Current, conflict.Current.Version
		s.render(w, r, http.StatusConflict, "case_edit.html", data)
	case err != nil:
		if msg, ok := inputError(err); ok {
			data["Error"] = msg
			s.render(w, r, http.StatusBadRequest, "case_edit.html", data)
			return
		}
		s.fail(w, err)
	default:
		http.Redirect(w, r, casePath(ws, c)+"?saved=1", http.StatusSeeOther)
	}
}

// newSecret is a fresh value for a repository's secret file, offered on the settings page.
func newSecret() string {
	b := make([]byte, 32)
	rand.Read(b)
	return "autobot-" + hex.EncodeToString(b)
}

// OpenCase opens (or finds) a workspace's case for an issue or pull request, reading its kind and
// title from GitHub. Intake will call this; until then, so does the command line.
func OpenCase(ctx context.Context, st *store.Store, gh *github.Client, token string, ws *store.Workspace, number int) (*store.Case, bool, error) {
	if c, err := st.CaseByNumber(ws.ID, number); err == nil {
		return c, false, nil
	}
	item, err := gh.Item(ctx, token, ws.Name, number)
	if err != nil {
		return nil, false, err
	}
	return st.OpenCase(ws, item.Number, item.Kind, item.Title, store.CaseHeadline(time.Now(), ws.Name, item.Kind, item.Number, item.Title))
}

// secretHelp tells a contributor's agent how to read a private repository's case file.
func secretHelp(f repoFacts) string {
	var b strings.Builder
	b.WriteString("# This case file belongs to a private repository\n\n")
	b.WriteString("To read it, send the repository's Autobot secret as a bearer token:\n\n")
	b.WriteString("    curl -H \"Authorization: Bearer $(cat " + SecretPath + ")\" <this address>\n\n")
	switch {
	case !f.readable:
		b.WriteString("Autobot cannot read the repository yet, so it cannot check a secret. Ask the people who run ")
		b.WriteString("this repository's Autobot workspace to give it a GitHub identity that can see the repository.\n")
	case f.secret == "":
		b.WriteString("**The repository has no secret yet.** Anyone with write access to the repository can set one up:\n\n")
		b.WriteString("1. Create `" + SecretPath + "` on the default branch, containing a random value of at least ")
		b.WriteString(strconv.Itoa(minSecret) + " characters on one line, for example:\n\n")
		b.WriteString("       " + newSecret() + "\n\n")
		b.WriteString("2. Commit and push it to the default branch.\n")
		b.WriteString("3. Within five minutes, send that value as the bearer token, as above.\n\n")
		b.WriteString("Only people who can read the repository can read the file, so the secret shows that the reader has access. ")
		b.WriteString("Change the value to rotate it, for example when someone loses access.\n")
	case len(f.secret) < minSecret:
		b.WriteString("**The secret in `" + SecretPath + "` is too short.** Replace it with a random value of at least ")
		b.WriteString(strconv.Itoa(minSecret) + " characters, such as `" + newSecret() + "`, and commit it to the default branch.\n")
	default:
		b.WriteString("The repository has a secret in `" + SecretPath + "` on its default branch. The request did not send it, ")
		b.WriteString("or sent a value that does not match. If the file changed in the last five minutes, try again shortly.\n")
	}
	return b.String()
}

// casesPage lists a workspace's cases densely, with a search box.
func (s *Server) casesPage(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return
	}
	q := r.URL.Query().Get("q")
	cases, err := s.st.FindCases(ws.ID, q, 1000)
	if err != nil {
		s.fail(w, err)
		return
	}
	total, err := s.st.CaseCount(ws.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, http.StatusOK, "cases.html", map[string]any{"Title": "Case files · " + ws.Name, "WS": ws,
		"Cases": cases, "Q": q, "Total": total})
}
