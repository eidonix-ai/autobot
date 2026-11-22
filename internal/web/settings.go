package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/eidonix-ai/autobot/internal/github"
	"github.com/eidonix-ai/autobot/internal/store"
)

// checkRepo validates a workspace name typed by an admin and confirms the repository exists on
// GitHub. It returns the name as GitHub spells it, or a *store.InputError to show the admin. When
// GitHub cannot be asked, the name is refused rather than saved unchecked.
func (s *Server) checkRepo(r *http.Request, input string) (string, error) {
	name, err := store.NormalizeWorkspaceName(input)
	if err != nil {
		return "", err
	}
	canonical, err := s.repos.RepoName(r.Context(), name)
	if errors.Is(err, github.ErrNoRepo) {
		return "", &store.InputError{Msg: fmt.Sprintf("GitHub has no public repository named %s. Check the spelling; private repositories are not supported yet.", name)}
	}
	if err != nil {
		return "", &store.InputError{Msg: fmt.Sprintf("Could not confirm %s on GitHub (%v), so nothing was saved.", name, err)}
	}
	return canonical, nil
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
	data["Title"], data["WS"], data["Users"], data["Assigned"] = "Settings · "+ws.Name, ws, users, assigned
	s.render(w, r, status, "settings.html", data)
}

func (s *Server) renameWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return
	}
	name, err := s.checkRepo(r, r.PostForm.Get("name"))
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
