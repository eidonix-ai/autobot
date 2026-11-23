// Package web serves Autobot's browser interface: server-rendered pages and plain forms, no
// JavaScript.
package web

import (
	"bytes"
	"context"
	"crypto/subtle"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	_ "time/tzdata" // the Eastern-time display works on hosts without a zoneinfo database

	"github.com/eidonix-ai/autobot/internal/agent"
	"github.com/eidonix-ai/autobot/internal/github"
	"github.com/eidonix-ai/autobot/internal/netguard"
	"github.com/eidonix-ai/autobot/internal/store"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
)

//go:embed templates static
var assets embed.FS

const (
	cookieName = "autobot_session"
	maxForm    = 1 << 20 // bytes accepted in one form post
)

// Server is the web interface over a store.
type Server struct {
	st        *store.Store
	gh        GitHub
	live      *live
	sessions  sessionQueues // the command queues of agents' sessions
	idleLimit time.Duration // how long a session may sit without a command
	access    *repoAccess
	pages     map[string]*template.Template
	md        goldmark.Markdown
	mux       *http.ServeMux
	ready     atomic.Bool // set once a user exists; users are never deleted, so it never goes back
	fails     loginFails  // failed sign-ins by account
}

// GitHub is what the web interface asks of GitHub. *github.Client is the real one; tests use a fake.
type GitHub interface {
	RepoName(ctx context.Context, token, name string) (string, error)
	CheckIdentity(ctx context.Context, login, token, name string) (*github.Identity, error)
	CommitSHA(ctx context.Context, token, name, ref string) (string, error)
	Private(ctx context.Context, token, name string) (bool, error)
	File(ctx context.Context, token, name, path string) (string, error)
	Tarball(ctx context.Context, token, name, commit string) (io.ReadCloser, error)
	PullRequest(ctx context.Context, token, name string, number int) (*github.PullRequest, error)
	Item(ctx context.Context, token, name string, number int) (*github.Item, error)
	PullRequestFiles(ctx context.Context, token, name string, number int) ([]github.ChangedFile, error)
	agent.Reader
}

// New builds the web interface.
func New(st *store.Store, gh GitHub) (*Server, error) {
	eastern, err := time.LoadLocation("America/New_York")
	if err != nil {
		return nil, err
	}
	funcs := template.FuncMap{
		"ts":   func(t time.Time) string { return t.In(eastern).Format("Jan 2, 2006 3:04 PM") + " ET" },
		"join": strings.Join,
		// num shows a reading with the given number of decimals, or "–" when it is not reported.
		"num": func(v *float64, digits int) string {
			if v == nil {
				return "–"
			}
			return strconv.FormatFloat(*v, 'f', digits, 64)
		},
		"gib":     func(mib int) string { return strconv.FormatFloat(float64(mib)/1024, 'f', 1, 64) + " GiB" },
		"bytes":   humanBytes,
		"short":   func(s string, n int) string { s = strings.TrimPrefix(s, "sha256:"); return s[:min(n, len(s))] },
		"ago":     func(t time.Time) string { return time.Since(t).Round(time.Second).String() + " ago" },
		"pct":     func(p float64) float64 { return p * 100 },
		"sub":     func(a, b int) int { return a - b },
		"tsShort": func(t time.Time) string { return t.In(eastern).Format("Jan 2, 3:04 PM") },
	}
	s := &Server{
		st:        st,
		gh:        gh,
		idleLimit: idleLimit,
		live:      &live{started: time.Now(), byID: map[int64]liveStatus{}},
		access:    &repoAccess{byWS: map[int64]repoFacts{}},
		pages:     map[string]*template.Template{},
		// Goldmark escapes raw HTML and drops javascript: links unless told otherwise; keep it that way.
		md:  goldmark.New(goldmark.WithExtensions(extension.GFM), goldmark.WithParserOptions(parser.WithAutoHeadingID())),
		mux: http.NewServeMux(),
	}
	names, err := fs.Glob(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		if n == "templates/layout.html" {
			continue
		}
		t, err := template.New("").Funcs(funcs).ParseFS(assets, "templates/layout.html", n)
		if err != nil {
			return nil, err
		}
		s.pages[strings.TrimPrefix(n, "templates/")] = t
	}
	s.routes()
	return s, nil
}

func (s *Server) routes() {
	static, _ := fs.Sub(assets, "static")
	s.mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))

	s.mux.HandleFunc("GET /setup", s.setupPage)
	s.mux.HandleFunc("POST /setup", s.setup)
	s.mux.HandleFunc("GET /login", s.loginPage)
	s.mux.HandleFunc("POST /login", s.login)
	s.mux.Handle("POST /logout", s.auth(s.logout))
	s.mux.Handle("GET /account", s.auth(s.account))
	s.mux.Handle("POST /account/password", s.auth(s.changePassword))

	s.mux.Handle("GET /{$}", s.auth(s.home))
	s.mux.Handle("POST /workspaces", s.admin(s.createWorkspace))
	s.mux.Handle("GET /w/{owner}/{repo}", s.auth(s.workspace))
	s.mux.Handle("GET /w/{owner}/{repo}/charter", s.auth(s.charter))
	s.mux.Handle("GET /w/{owner}/{repo}/charter/edit", s.auth(s.charterEdit))
	s.mux.Handle("POST /w/{owner}/{repo}/charter", s.auth(s.charterSave))
	s.mux.Handle("GET /w/{owner}/{repo}/settings", s.auth(s.settings))
	s.mux.Handle("POST /w/{owner}/{repo}/settings/name", s.admin(s.renameWorkspace))
	s.mux.Handle("POST /w/{owner}/{repo}/settings/github", s.auth(s.setGitHub))
	s.mux.Handle("POST /w/{owner}/{repo}/settings/github/remove", s.auth(s.removeGitHub))
	s.mux.Handle("POST /w/{owner}/{repo}/settings/members", s.admin(s.setMembers))
	s.mux.Handle("POST /w/{owner}/{repo}/settings/delete", s.admin(s.deleteWorkspace))

	s.mux.Handle("GET /w/{owner}/{repo}/machines/new", s.auth(s.newMachinePage))
	s.mux.Handle("POST /w/{owner}/{repo}/machines", s.auth(s.addMachine))
	s.mux.Handle("GET /w/{owner}/{repo}/machines/{id}", s.auth(s.machinePage))
	s.mux.Handle("POST /w/{owner}/{repo}/machines/{id}/runs", s.auth(s.startRun))
	s.mux.Handle("POST /w/{owner}/{repo}/machines/{id}/remove", s.auth(s.removeMachine))
	s.mux.Handle("POST /w/{owner}/{repo}/machines/{id}/enabled", s.auth(s.setMachineEnabled))
	s.mux.Handle("GET /w/{owner}/{repo}/runs/{id}", s.auth(s.runPage))
	s.mux.Handle("POST /w/{owner}/{repo}/runs/{id}/stop", s.auth(s.stopRun))

	// The connectors on donated machines, signed in by machine token.
	s.mux.Handle("POST /connector/v1/heartbeat", s.connectorAPI(s.heartbeat))
	s.mux.Handle("GET /connector/v1/runs/{id}/source", s.connectorAPI(s.runSource))
	s.mux.Handle("POST /connector/v1/runs/{id}/output", s.connectorAPI(s.runOutput))
	s.mux.Handle("POST /connector/v1/runs/{id}/result", s.connectorAPI(s.runResult))
	s.mux.Handle("GET /connector/v1/runs/{id}/commands", s.connectorAPI(s.sessionCommands))
	s.mux.Handle("POST /connector/v1/runs/{id}/commands/{cid}/result", s.connectorAPIMax(8<<20, s.sessionResult))

	s.mux.Handle("GET /w/{owner}/{repo}/workflows", s.auth(s.workflowsIndex))
	s.mux.Handle("GET /w/{owner}/{repo}/workflows/{kind}", s.auth(s.workflowPage))
	s.mux.Handle("POST /w/{owner}/{repo}/workflows/{kind}/layout", s.auth(s.saveLayout))
	s.mux.Handle("GET /w/{owner}/{repo}/workflows/{kind}/nodes/{node}", s.auth(s.nodeSettings))
	s.mux.Handle("POST /w/{owner}/{repo}/workflows/{kind}/nodes/{node}", s.auth(s.saveNodeSettings))
	s.mux.Handle("GET /w/{owner}/{repo}/workflows/{kind}/nodes/{node}/test", s.auth(s.testForm))
	s.mux.Handle("POST /w/{owner}/{repo}/workflows/{kind}/nodes/{node}/test", s.auth(s.runTest))
	s.mux.Handle("GET /w/{owner}/{repo}/agent-runs/{id}", s.auth(s.agentRun))
	s.mux.Handle("POST /w/{owner}/{repo}/workflows/{kind}/nodes/{node}/run", s.auth(s.runWorkflow))
	s.mux.Handle("GET /w/{owner}/{repo}/workflow-runs/{id}", s.auth(s.workflowRun))
	s.mux.Handle("GET /w/{owner}/{repo}/models", s.auth(s.modelsPage))
	s.mux.Handle("GET /w/{owner}/{repo}/members", s.auth(s.membersPage))
	s.mux.Handle("GET /w/{owner}/{repo}/members/{username}", s.auth(s.memberPage))
	s.mux.Handle("GET /w/{owner}/{repo}/cases", s.auth(s.casesPage))
	s.mux.Handle("GET /w/{owner}/{repo}/cases/{number}", s.auth(s.casePage))
	s.mux.Handle("GET /w/{owner}/{repo}/cases/{number}/edit", s.auth(s.caseEdit))
	s.mux.Handle("POST /w/{owner}/{repo}/cases/{number}", s.auth(s.caseSave))
	// Read by contributors' agents: public repositories' cases by uuid; private ones with the secret.
	s.mux.HandleFunc("GET /api/v1/cases/{uuid}", s.apiCase)

	s.mux.Handle("GET /admin/users", s.admin(s.users))
	s.mux.Handle("GET /admin/discord", s.admin(s.discordPage))
	s.mux.Handle("POST /admin/discord", s.admin(s.saveDiscord))
	s.mux.Handle("POST /discord/interactions", http.HandlerFunc(s.interaction))
	s.mux.Handle("POST /w/{owner}/{repo}/settings/discord", s.auth(s.setForum))
	s.mux.Handle("POST /admin/users", s.admin(s.createUser))
	s.mux.Handle("GET /admin/users/{id}", s.admin(s.userPage))
	s.mux.Handle("POST /admin/users/{id}", s.admin(s.updateUser))

	s.mux.Handle("GET /providers", s.auth(s.providerList))
	s.mux.Handle("POST /providers", s.auth(s.createProvider))
	s.mux.Handle("POST /providers/trusted", s.admin(s.saveTrusted))
	s.mux.Handle("GET /providers/{id}", s.auth(s.provider))
	s.mux.Handle("POST /providers/{id}", s.auth(s.updateProvider))
	s.mux.Handle("POST /providers/{id}/discover", s.auth(s.discoverModels))
	s.mux.Handle("POST /providers/{id}/try", s.auth(s.tryDecision))
	s.mux.Handle("POST /providers/{id}/routes", s.auth(s.recheckRoutes))
	s.mux.Handle("POST /providers/{id}/search", s.auth(s.trySearch))
	s.mux.Handle("POST /providers/{id}/lend", s.auth(s.lendProvider))
	s.mux.Handle("POST /providers/{id}/models", s.auth(s.setProviderModels))
	s.mux.Handle("POST /providers/{id}/delete", s.auth(s.deleteProvider))
}

// Handler is the root handler. It sets security headers on every response and refuses form posts
// sent from other sites, which protects the sign-in and setup forms that have no session token.
func (s *Server) Handler() http.Handler {
	guarded := http.NewCrossOriginProtection().Handler(s.mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		netguard.NoteOwnHost(r.Host) // so agents' fetches cannot loop back in through the router
		guarded.ServeHTTP(w, r)
	})
}

// setupNeeded reports whether no user exists yet, in which case the first visitor creates the admin.
func (s *Server) setupNeeded() (bool, error) {
	if s.ready.Load() {
		return false, nil
	}
	has, err := s.st.HasUsers()
	if err != nil {
		return false, err
	}
	if has {
		s.ready.Store(true)
	}
	return !has, nil
}

type session struct {
	user  *store.User
	csrf  string
	token string
}

type ctxKey struct{}

func sess(r *http.Request) *session  { return r.Context().Value(ctxKey{}).(*session) }
func me(r *http.Request) *store.User { return sess(r).user }

// auth admits signed-in users and checks the form token on every POST.
func (s *Server) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(cookieName)
		if err != nil {
			toLogin(w, r)
			return
		}
		u, csrf, err := s.st.Session(c.Value)
		if errors.Is(err, store.ErrNotFound) {
			toLogin(w, r)
			return
		}
		if err != nil {
			s.fail(w, err)
			return
		}
		if r.Method == http.MethodPost {
			r.Body = http.MaxBytesReader(w, r.Body, maxForm)
			// Forms send the token as a field; the workflow canvas's requests, as a header. The form is
			// parsed either way (a JSON body is left alone), so handlers can read r.PostForm.
			r.ParseForm()
			token := r.Header.Get("X-Autobot-CSRF")
			if token == "" {
				token = r.PostFormValue("csrf")
			}
			if subtle.ConstantTimeCompare([]byte(token), []byte(csrf)) != 1 {
				http.Error(w, "This form has expired. Go back, reload the page and try again.", http.StatusForbidden)
				return
			}
		}
		// A signed-in page is for this person only: no browser or proxy keeps a copy.
		w.Header().Set("Cache-Control", "no-store")
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, &session{u, csrf, c.Value})))
	})
}

// survive keeps a panic in background work (a run, a step, an agent) from ending the whole
// service: it is logged, and calls onPanic, if given, with what happened.
func survive(what string, onPanic func(string)) {
	if r := recover(); r != nil {
		msg := fmt.Sprintf("%s crashed: %v", what, r)
		log.Printf("%s\n%s", msg, debug.Stack())
		if onPanic != nil {
			onPanic(msg)
		}
	}
}

// admin admits admins only; to anyone else the page does not exist.
func (s *Server) admin(next http.HandlerFunc) http.Handler {
	return s.auth(func(w http.ResponseWriter, r *http.Request) {
		if !me(r).IsAdmin {
			http.NotFound(w, r)
			return
		}
		next(w, r)
	})
}

func toLogin(w http.ResponseWriter, r *http.Request) {
	target := "/login"
	if r.Method == http.MethodGet && r.URL.Path != "/" {
		target += "?next=" + url.QueryEscape(r.URL.RequestURI())
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// safeNext keeps post-login redirects on this site.
// safeNext keeps a redirect after sign-in on this site: a path, never another host. Browsers drop
// tabs and line breaks from addresses and read a backslash as a slash, so "/\t/evil.example" would
// become "//evil.example"; anything with those, or that parses with a host, goes home instead.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.ContainsAny(next, "\\") ||
		strings.ContainsFunc(next, func(r rune) bool { return r < 0x20 || r == 0x7f || r == ' ' }) {
		return "/"
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || strings.HasPrefix(u.Path, "//") {
		return "/"
	}
	return next
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	})
}

// render executes a page inside the layout. data may be nil.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page string, data map[string]any) {
	t, ok := s.pages[page]
	if !ok {
		s.fail(w, errors.New("no page "+page))
		return
	}
	if data == nil {
		data = map[string]any{}
	}
	if sv, ok := r.Context().Value(ctxKey{}).(*session); ok {
		data["Me"], data["CSRF"] = sv.user, sv.csrf
	}
	if _, ok := data["Form"]; !ok {
		data["Form"] = url.Values{}
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", data); err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	log.Printf("web: %v", err)
	http.Error(w, "Something went wrong on our side; the details are in the server log.", http.StatusInternalServerError)
}

// inputError returns the message of an error meant for the person, if it is one.
func inputError(err error) (string, bool) {
	var ie *store.InputError
	if errors.As(err, &ie) {
		return ie.Msg, true
	}
	return "", false
}

// humanBytes shows a size in GB or MB.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return strconv.FormatFloat(float64(n)/(1<<30), 'f', 1, 64) + " GB"
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 0, 64) + " MB"
	default:
		return strconv.FormatInt(n>>10, 10) + " KB"
	}
}
