// Package web serves Autobot's browser interface: server-rendered pages and plain forms, no
// JavaScript.
package web

import (
	"bytes"
	"context"
	"crypto/subtle"
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
	_ "time/tzdata" // the Eastern-time display works on hosts without a zoneinfo database

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
	st    *store.Store
	pages map[string]*template.Template
	md    goldmark.Markdown
	mux   *http.ServeMux
	ready atomic.Bool // set once a user exists; users are never deleted, so it never goes back
}

// New builds the web interface.
func New(st *store.Store) (*Server, error) {
	eastern, err := time.LoadLocation("America/New_York")
	if err != nil {
		return nil, err
	}
	funcs := template.FuncMap{
		"ts":   func(t time.Time) string { return t.In(eastern).Format("Jan 2, 2006 3:04 PM") + " ET" },
		"join": strings.Join,
	}
	s := &Server{
		st:    st,
		pages: map[string]*template.Template{},
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

	s.mux.Handle("GET /admin/users", s.admin(s.users))
	s.mux.Handle("POST /admin/users", s.admin(s.createUser))
	s.mux.Handle("GET /admin/users/{id}", s.admin(s.userPage))
	s.mux.Handle("POST /admin/users/{id}", s.admin(s.updateUser))
}

// Handler is the root handler. It sets security headers on every response and refuses form posts
// sent from other sites, which protects the sign-in and setup forms that have no session token.
func (s *Server) Handler() http.Handler {
	guarded := http.NewCrossOriginProtection().Handler(s.mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' https: data:; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
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
			if subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(csrf)) != 1 {
				http.Error(w, "This form has expired. Go back, reload the page and try again.", http.StatusForbidden)
				return
			}
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, &session{u, csrf, c.Value})))
	})
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
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
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
