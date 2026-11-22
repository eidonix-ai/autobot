package web

import (
	"net/http"
	"strings"

	"github.com/eidonix-ai/autobot/internal/store"
)

// membersPage lists everyone assigned to a workspace, densely.
func (s *Server) membersPage(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return
	}
	members, err := s.st.Members(ws.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, http.StatusOK, "members.html", map[string]any{"Title": "Members · " + ws.Name, "WS": ws, "Members": members})
}

// memberPage shows what the workspace knows about one of its members: who they are, the providers
// they lend it, and what they have done in it.
func (s *Server) memberPage(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return
	}
	members, err := s.st.Members(ws.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	var m *store.User
	for i := range members {
		if strings.EqualFold(members[i].Username, r.PathValue("username")) {
			m = &members[i]
		}
	}
	if m == nil {
		http.NotFound(w, r)
		return
	}
	available, err := s.st.AvailableModels(ws.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	lends := map[string][]string{} // provider name -> models
	var order []string
	for _, a := range available {
		if a.OwnerID != m.ID {
			continue
		}
		if _, seen := lends[a.ProviderName]; !seen {
			order = append(order, a.ProviderName)
		}
		lends[a.ProviderName] = append(lends[a.ProviderName], a.Model)
	}
	events, err := s.st.EventsBy(ws.ID, m.ID, 50)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, http.StatusOK, "member.html", map[string]any{"Title": m.Name() + " · " + ws.Name, "WS": ws, "M": m,
		"Lends": lends, "LendOrder": order, "Events": events})
}
