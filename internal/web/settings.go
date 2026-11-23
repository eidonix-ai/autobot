package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/eidonix-ai/autobot/internal/github"
	"github.com/eidonix-ai/autobot/internal/index"
	"github.com/eidonix-ai/autobot/internal/store"
)

// checkRepo confirms the repository name exists on GitHub, using token when there is one, and
// returns it as GitHub spells it, or a *store.InputError to show the admin. When GitHub cannot be
// asked, the name is refused rather than saved unchecked.
func (s *Server) checkRepo(ctx context.Context, name, token string) (string, error) {
	canonical, err := s.gh.RepoName(ctx, token, name)
	switch {
	case errors.Is(err, github.ErrNoRepo) && token == "":
		return "", inputf("GitHub has no public repository named %s. If it is private, enter the GitHub user and token Autobot should use for it.", name)
	case errors.Is(err, github.ErrNoRepo):
		return "", inputf("%s is not on GitHub, or this workspace's GitHub user cannot see it.", name)
	case err != nil:
		return "", inputf("Could not confirm %s on GitHub (%v), so nothing was saved.", name, err)
	}
	return canonical, nil
}

// checkIdentity confirms a GitHub user and token can see the repository, as an input error if not.
func (s *Server) checkIdentity(ctx context.Context, login, token, name string) (*github.Identity, error) {
	if login == "" || token == "" {
		return nil, inputf("Enter both the GitHub user and that user's token.")
	}
	id, err := s.gh.CheckIdentity(ctx, login, token, name)
	if err != nil {
		return nil, inputf("The GitHub identity was not saved: %v.", err)
	}
	return id, nil
}

func inputf(format string, a ...any) error { return &store.InputError{Msg: fmt.Sprintf(format, a...)} }

// identity returns a workspace's GitHub identity, or nil when it has none.
func (s *Server) identity(ws *store.Workspace) (*store.GitHubIdentity, error) {
	id, err := s.st.GitHub(ws.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	return id, err
}

// token is the workspace's GitHub token for API calls, or "" when it has none that can be read.
func (s *Server) token(ws *store.Workspace) (string, error) {
	id, err := s.identity(ws)
	if err != nil || id == nil {
		return "", err
	}
	return id.Token, nil
}

func (s *Server) createWorkspace(w http.ResponseWriter, r *http.Request) {
	f := r.PostForm
	login, token := strings.TrimSpace(f.Get("gh_login")), strings.TrimSpace(f.Get("gh_token"))
	name, err := store.NormalizeWorkspaceName(f.Get("name"))
	if err == nil {
		if login == "" && token == "" {
			name, err = s.checkRepo(r.Context(), name, "")
		} else {
			var id *github.Identity
			if id, err = s.checkIdentity(r.Context(), login, token, name); err == nil {
				name, login = id.Repo, id.Login
			}
		}
	}
	var ws *store.Workspace
	if err == nil {
		ws, err = s.st.CreateWorkspace(name, me(r), login, token)
	}
	if msg, ok := inputError(err); ok {
		f.Del("gh_token") // never send a token back to the browser
		s.homePage(w, r, http.StatusBadRequest, map[string]any{"Error": msg, "Form": f})
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, ws.Path(), http.StatusSeeOther)
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return
	}
	data := map[string]any{}
	if r.URL.Query().Has("saved") {
		data["Notice"] = "Saved."
	}
	s.settingsPage(w, r, http.StatusOK, ws, data)
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request, status int, ws *store.Workspace, data map[string]any) {
	users, err := s.st.Users()
	if err != nil {
		s.fail(w, err)
		return
	}
	members, err := s.st.Members(ws.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	assigned := map[int64]bool{}
	for _, m := range members {
		assigned[m.ID] = true
	}
	gh, err := s.identity(ws)
	if err != nil {
		s.fail(w, err)
		return
	}
	login := ""
	if f, ok := data["Form"].(url.Values); ok && f.Has("gh_login") {
		login = f.Get("gh_login")
	}
	if gh != nil {
		if login == "" {
			login = gh.Login
		}
		// Ask GitHub every time, so a revoked or expired token shows here at once.
		if gh.Err != nil {
			data["GHProblem"] = gh.Err.Error()
		} else if live, err := s.gh.CheckIdentity(r.Context(), gh.Login, gh.Token, ws.Name); err != nil {
			data["GHProblem"] = "GitHub no longer accepts this identity: " + err.Error() + "."
		} else {
			data["Live"] = live
		}
	}
	data["Title"], data["WS"], data["Users"], data["Assigned"] = "Settings · "+ws.Name, ws, users, assigned
	data["GH"], data["GHLogin"] = gh, login
	data["SecretPath"], data["NewSecret"] = SecretPath, newSecret()
	s.discordSettings(r, ws, data)
	if st, err := index.StatusOf(s.st, ws.ID); err == nil {
		data["Index"] = st
	}
	s.repoSettings(ws, data)

	s.render(w, r, status, "settings.html", data)
}

func (s *Server) renameWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return
	}
	token, err := s.token(ws)
	if err != nil {
		s.fail(w, err)
		return
	}
	name, err := store.NormalizeWorkspaceName(r.PostForm.Get("name"))
	if err == nil {
		name, err = s.checkRepo(r.Context(), name, token)
	}
	var renamed *store.Workspace
	if err == nil {
		renamed, err = s.st.RenameWorkspace(ws.ID, name, me(r))
	}
	if msg, ok := inputError(err); ok {
		s.settingsPage(w, r, http.StatusBadRequest, ws, map[string]any{"Error": msg, "Form": r.PostForm})
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, renamed.Path()+"/settings?saved=1", http.StatusSeeOther)
}

func (s *Server) setGitHub(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return
	}
	login, token := strings.TrimSpace(r.PostForm.Get("gh_login")), strings.TrimSpace(r.PostForm.Get("gh_token"))
	id, err := s.checkIdentity(r.Context(), login, token, ws.Name)
	if err == nil {
		err = s.st.SetGitHub(ws.ID, id.Login, token, me(r))
	}
	if msg, ok := inputError(err); ok {
		form := r.PostForm
		form.Del("gh_token")
		s.settingsPage(w, r, http.StatusBadRequest, ws, map[string]any{"Error": msg, "Form": form})
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, ws.Path()+"/settings?saved=1", http.StatusSeeOther)
}

func (s *Server) removeGitHub(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return
	}
	// A private repository needs an identity: check it is visible without one before removing it.
	if _, err := s.gh.RepoName(r.Context(), "", ws.Name); err != nil {
		msg := fmt.Sprintf("%s is private, so Autobot needs a GitHub identity for it. Save a new user and token instead of removing it.", ws.Name)
		if !errors.Is(err, github.ErrNoRepo) {
			msg = fmt.Sprintf("Could not check whether %s is public (%v), so the identity was kept.", ws.Name, err)
		}
		s.settingsPage(w, r, http.StatusBadRequest, ws, map[string]any{"Error": msg})
		return
	}
	if err := s.st.RemoveGitHub(ws.ID, me(r)); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, ws.Path()+"/settings?saved=1", http.StatusSeeOther)
}

func (s *Server) setMembers(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return
	}
	var ids []int64
	for _, v := range r.PostForm["member"] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	if err := s.st.SetMembers(ws.ID, ids, me(r)); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "One of those users no longer exists; reload the page.", http.StatusBadRequest)
			return
		}
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, ws.Path()+"/settings?saved=1", http.StatusSeeOther)
}

func (s *Server) deleteWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return
	}
	if !strings.EqualFold(strings.TrimSpace(r.PostForm.Get("confirm")), ws.Name) {
		s.settingsPage(w, r, http.StatusBadRequest, ws, map[string]any{
			"Error": "The workspace was not deleted: type its name, " + ws.Name + ", to confirm."})
		return
	}
	if err := s.st.DeleteWorkspace(ws.ID); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/?deleted=1", http.StatusSeeOther)
}
