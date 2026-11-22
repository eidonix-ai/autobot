package web

import (
	"bytes"
	"errors"
	"html/template"
	"net/http"
	"strconv"

	"github.com/eidonix-ai/autobot/internal/store"
)

// --- first-time setup ---

// toSetup sends the visitor to the setup page while no user exists, and reports whether it did.
func (s *Server) toSetup(w http.ResponseWriter, r *http.Request) bool {
	need, err := s.setupNeeded()
	if err != nil {
		s.fail(w, err)
		return true
	}
	if need {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
	}
	return need
}

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	need, err := s.setupNeeded()
	if err != nil {
		s.fail(w, err)
		return
	}
	if !need {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "setup.html", map[string]any{"Title": "Set up Autobot"})
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxForm)
	r.ParseForm()
	f := r.PostForm
	again := func(msg string) {
		s.render(w, r, http.StatusBadRequest, "setup.html", map[string]any{"Title": "Set up Autobot", "Form": f, "Error": msg})
	}
	if f.Get("password") != f.Get("confirm") {
		again("The two passwords did not match.")
		return
	}
	u, err := s.st.CreateFirstAdmin(f.Get("username"), f.Get("display_name"), f.Get("password"))
	if errors.Is(err, store.ErrSetupDone) {
		s.ready.Store(true)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if msg, ok := inputError(err); ok {
		again(msg)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.ready.Store(true)
	token, err := s.st.CreateSession(u.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	setSessionCookie(w, r, token, int(store.SessionTTL.Seconds()))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// --- signing in and out ---

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if s.toSetup(w, r) {
		return
	}
	s.render(w, r, http.StatusOK, "login.html", map[string]any{"Title": "Sign in", "Next": safeNext(r.URL.Query().Get("next"))})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if s.toSetup(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxForm)
	r.ParseForm()
	next := safeNext(r.PostForm.Get("next"))
	u, err := s.st.CheckPassword(r.PostForm.Get("username"), r.PostForm.Get("password"))
	if errors.Is(err, store.ErrNotFound) {
		s.render(w, r, http.StatusUnauthorized, "login.html", map[string]any{
			"Title": "Sign in", "Next": next, "Form": r.PostForm, "Error": "Wrong username or password."})
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	token, err := s.st.CreateSession(u.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	setSessionCookie(w, r, token, int(store.SessionTTL.Seconds()))
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteSession(sess(r).token); err != nil {
		s.fail(w, err)
		return
	}
	setSessionCookie(w, r, "", -1)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) account(w http.ResponseWriter, r *http.Request) {
	s.accountPage(w, r, http.StatusOK, nil)
}

func (s *Server) accountPage(w http.ResponseWriter, r *http.Request, status int, data map[string]any) {
	wss, err := s.st.Workspaces(me(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	if data == nil {
		data = map[string]any{}
	}
	data["Title"], data["Workspaces"] = "Your account", wss
	if r.URL.Query().Has("changed") {
		data["Notice"] = "Your password has been changed. Your other sessions were signed out."
	}
	s.render(w, r, status, "account.html", data)
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	u := me(r)
	if _, err := s.st.CheckPassword(u.Username, r.PostForm.Get("current")); err != nil {
		s.accountPage(w, r, http.StatusBadRequest, map[string]any{"Error": "Your current password was not right."})
		return
	}
	if r.PostForm.Get("new") != r.PostForm.Get("confirm") {
		s.accountPage(w, r, http.StatusBadRequest, map[string]any{"Error": "The two new passwords did not match."})
		return
	}
	if err := s.st.SetPassword(u.ID, r.PostForm.Get("new"), sess(r).token); err != nil {
		if msg, ok := inputError(err); ok {
			s.accountPage(w, r, http.StatusBadRequest, map[string]any{"Error": msg})
			return
		}
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/account?changed=1", http.StatusSeeOther)
}

// --- workspaces ---

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	s.homePage(w, r, http.StatusOK, nil)
}

func (s *Server) homePage(w http.ResponseWriter, r *http.Request, status int, data map[string]any) {
	wss, err := s.st.Workspaces(me(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	if data == nil {
		data = map[string]any{}
	}
	data["Title"], data["Workspaces"] = "Workspaces", wss
	if r.URL.Query().Has("deleted") {
		data["Notice"] = "Workspace deleted."
	}
	s.render(w, r, status, "home.html", data)
}

// openWorkspace resolves the workspace in the URL and checks the user may open it. To someone who
// may not, it does not exist.
func (s *Server) openWorkspace(w http.ResponseWriter, r *http.Request) (*store.Workspace, bool) {
	ws, err := s.st.Workspace(r.PathValue("owner") + "/" + r.PathValue("repo"))
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return nil, false
	}
	if err != nil {
		s.fail(w, err)
		return nil, false
	}
	ok, err := s.st.CanOpen(me(r), ws.ID)
	if err != nil {
		s.fail(w, err)
		return nil, false
	}
	if !ok {
		http.NotFound(w, r)
		return nil, false
	}
	return ws, true
}

func (s *Server) workspace(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return
	}
	charter, err := s.st.Charter(ws.ID, 0)
	if err != nil {
		s.fail(w, err)
		return
	}
	members, err := s.st.Members(ws.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	events, err := s.st.Events(ws.ID, 500)
	if err != nil {
		s.fail(w, err)
		return
	}
	machines, err := s.machineViews(ws)
	if err != nil {
		s.fail(w, err)
		return
	}
	available, err := s.st.AvailableModels(ws.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	cases, err := s.st.FindCases(ws.ID, "", 3)
	if err != nil {
		s.fail(w, err)
		return
	}
	caseCount, err := s.st.CaseCount(ws.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, http.StatusOK, "workspace.html", map[string]any{
		"Title": ws.Name, "WS": ws, "Charter": charter, "Members": members, "Events": events, "Machines": machines,
		"Available": available, "Cases": cases, "CaseCount": caseCount, "Res": summarize(machines),
		"ChatModels": countKind(available, false), "DecisionModels": countKind(available, true)})
}

func (s *Server) charter(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return
	}
	rev, _ := strconv.Atoi(r.URL.Query().Get("rev"))
	c, err := s.st.Charter(ws.ID, rev)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	history, err := s.st.CharterHistory(ws.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	var html bytes.Buffer
	if err := s.md.Convert([]byte(c.Body), &html); err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, http.StatusOK, "charter.html", map[string]any{
		"Title": "Charter · " + ws.Name, "WS": ws, "Charter": c, "History": history,
		"IsLatest": c.Rev == history[0].Rev,
		"HTML":     template.HTML(html.String()), // goldmark output with raw HTML escaped
	})
}

func (s *Server) charterEdit(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return
	}
	c, err := s.st.Charter(ws.ID, 0)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, http.StatusOK, "charter_edit.html", map[string]any{
		"Title": "Edit charter · " + ws.Name, "WS": ws, "Body": c.Body, "BaseRev": c.Rev})
}

func (s *Server) charterSave(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return
	}
	base, _ := strconv.Atoi(r.PostForm.Get("base_rev"))
	body := r.PostForm.Get("body")
	_, err := s.st.SaveCharter(ws.ID, base, body, me(r))
	data := map[string]any{"Title": "Edit charter · " + ws.Name, "WS": ws, "Body": body, "BaseRev": base}
	var conflict *store.ConflictError
	switch {
	case errors.As(err, &conflict):
		// Keep their text, and base the next save on the revision they have now been shown.
		data["Conflict"], data["BaseRev"] = conflict.Current, conflict.Current.Rev
		s.render(w, r, http.StatusConflict, "charter_edit.html", data)
	case err != nil:
		if msg, ok := inputError(err); ok {
			data["Error"] = msg
			s.render(w, r, http.StatusBadRequest, "charter_edit.html", data)
			return
		}
		s.fail(w, err)
	default:
		http.Redirect(w, r, ws.Path()+"/charter", http.StatusSeeOther)
	}
}

// --- users (admin) ---

func (s *Server) users(w http.ResponseWriter, r *http.Request) {
	s.usersPage(w, r, http.StatusOK, nil)
}

func (s *Server) usersPage(w http.ResponseWriter, r *http.Request, status int, data map[string]any) {
	users, err := s.st.Users()
	if err != nil {
		s.fail(w, err)
		return
	}
	assignments, err := s.st.Assignments()
	if err != nil {
		s.fail(w, err)
		return
	}
	if data == nil {
		data = map[string]any{}
	}
	data["Title"], data["Users"], data["Assignments"] = "Users", users, assignments
	s.render(w, r, status, "users.html", data)
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	f := r.PostForm
	u, err := s.st.CreateUser(f.Get("username"), f.Get("display_name"), f.Get("password"), f.Get("is_admin") == "on")
	if msg, ok := inputError(err); ok {
		s.usersPage(w, r, http.StatusBadRequest, map[string]any{"Error": msg, "Form": f})
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	// Straight to their page, where the workspace assignments are.
	http.Redirect(w, r, "/admin/users/"+strconv.FormatInt(u.ID, 10)+"?created=1", http.StatusSeeOther)
}

func (s *Server) targetUser(w http.ResponseWriter, r *http.Request) (*store.User, bool) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	u, err := s.st.User(id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return nil, false
	}
	if err != nil {
		s.fail(w, err)
		return nil, false
	}
	return u, true
}

func (s *Server) userPage(w http.ResponseWriter, r *http.Request) {
	u, ok := s.targetUser(w, r)
	if !ok {
		return
	}
	assigned, err := s.st.Memberships(u.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	data := map[string]any{}
	switch {
	case r.URL.Query().Has("created"):
		data["Notice"] = "User added. Assign their workspaces below."
	case r.URL.Query().Has("saved"):
		data["Notice"] = "Saved."
	}
	s.renderUser(w, r, http.StatusOK, u, assigned, data)
}

func (s *Server) renderUser(w http.ResponseWriter, r *http.Request, status int, u *store.User, assigned map[int64]bool, data map[string]any) {
	wss, err := s.st.Workspaces(&store.User{IsAdmin: true}) // every workspace
	if err != nil {
		s.fail(w, err)
		return
	}
	data["Title"], data["U"], data["Workspaces"], data["Assigned"] = u.Username, u, wss, assigned
	s.render(w, r, status, "user.html", data)
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request) {
	u, ok := s.targetUser(w, r)
	if !ok {
		return
	}
	f := r.PostForm
	up := store.UserUpdate{
		DisplayName: f.Get("display_name"),
		IsAdmin:     f.Get("is_admin") == "on",
		Disabled:    f.Get("disabled") == "on",
		Password:    f.Get("password"),
	}
	assigned := map[int64]bool{}
	for _, v := range f["workspace"] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			up.Workspaces = append(up.Workspaces, id)
			assigned[id] = true
		}
	}
	err := s.st.UpdateUser(u.ID, up, me(r))
	if msg, ok := inputError(err); ok {
		// Show what they submitted, not what is stored, so nothing they set is lost.
		shown := *u
		shown.DisplayName, shown.IsAdmin, shown.Disabled = up.DisplayName, up.IsAdmin, up.Disabled
		s.renderUser(w, r, http.StatusBadRequest, &shown, assigned, map[string]any{"Error": msg})
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	if u.ID == me(r).ID && up.Disabled {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin/users/"+strconv.FormatInt(u.ID, 10)+"?saved=1", http.StatusSeeOther)
}

// countKind counts the available models that answer decisions (natively or through System One),
// or the ones that answer chat (text models). A model on a provider that does both counts in both.
func countKind(available []store.AvailableModel, decisions bool) int {
	n := 0
	for _, a := range available {
		if (decisions && a.DecisionAPI != "") || (!decisions && a.Text) {
			n++
		}
	}
	return n
}
