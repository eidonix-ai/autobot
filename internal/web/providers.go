package web

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/eidonix-ai/autobot/internal/decisions"
	"github.com/eidonix-ai/autobot/internal/providers"
	"github.com/eidonix-ai/autobot/internal/search"
	"github.com/eidonix-ai/autobot/internal/store"
)

// preset is a well-known provider the Add form can fill in. "custom" takes the endpoint typed in.
type preset struct {
	Key, Label, Kind, Endpoint string
}

var presets = []preset{
	{"openai", "OpenAI", store.KindOpenAI, "https://api.openai.com/v1"},
	{"anthropic", "Anthropic", store.KindAnthropic, "https://api.anthropic.com"},
	{"openrouter", "OpenRouter", store.KindOpenAI, "https://openrouter.ai/api/v1"},
	{"together", "Together AI", store.KindOpenAI, "https://api.together.xyz/v1"},
	{"groq", "Groq", store.KindOpenAI, "https://api.groq.com/openai/v1"},
	{"fireworks", "Fireworks", store.KindOpenAI, "https://api.fireworks.ai/inference/v1"},
	{"deepinfra", "DeepInfra", store.KindOpenAI, "https://api.deepinfra.com/v1/openai"},
	{"mistral", "Mistral", store.KindOpenAI, "https://api.mistral.ai/v1"},
	{"xai", "xAI", store.KindOpenAI, "https://api.x.ai/v1"},
	{"gemini", "Google Gemini", store.KindOpenAI, "https://generativelanguage.googleapis.com/v1beta/openai"},
	{"deepseek", "DeepSeek", store.KindOpenAI, "https://api.deepseek.com"},
	{"systemone", "System One (decisions)", store.KindSystemOne, "https://system-one.dev/v1"},
	{"tavily", "Tavily (web search)", store.KindTavily, "https://api.tavily.com"},
	{"brave", "Brave Search (web search)", store.KindBrave, "https://api.search.brave.com/res/v1"},
	{"custom", "Custom OpenAI-compatible (vLLM, llama.cpp, LiteLLM, any proxy)", store.KindOpenAI, ""},
	{"custom-decisions", "Custom /v1/decisions server", store.KindDecisions, ""},
}

func findPreset(key string) (preset, bool) {
	for _, p := range presets {
		if p.Key == key {
			return p, true
		}
	}
	return preset{}, false
}

func (s *Server) providerList(w http.ResponseWriter, r *http.Request) {
	s.providersPage(w, r, http.StatusOK, nil)
}

func (s *Server) providersPage(w http.ResponseWriter, r *http.Request, status int, data map[string]any) {
	list, err := s.st.Providers(me(r).ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	var others []*store.Provider
	if me(r).IsAdmin { // an admin sees everyone's, read-only
		all, err := s.st.Providers(0)
		if err != nil {
			s.fail(w, err)
			return
		}
		for _, p := range all {
			if p.OwnerID != me(r).ID {
				others = append(others, p)
			}
		}
	}
	if data == nil {
		data = map[string]any{}
	}
	if r.URL.Query().Has("deleted") {
		data["Notice"] = "Provider deleted."
	}
	data["Title"], data["Providers"], data["Others"], data["Presets"] = "Providers", list, others, presets
	s.render(w, r, status, "providers.html", data)
}

func (s *Server) createProvider(w http.ResponseWriter, r *http.Request) {
	f := r.PostForm
	e := store.ProviderEdit{Name: f.Get("name"), Kind: store.KindOpenAI, Endpoint: f.Get("endpoint"), Key: f.Get("api_key")}
	pre, ok := findPreset(f.Get("preset"))
	if ok {
		e.Kind = pre.Kind
		if pre.Endpoint != "" {
			e.Endpoint = pre.Endpoint
		}
	}
	if strings.TrimSpace(e.Name) == "" && ok && pre.Endpoint != "" {
		e.Name, _, _ = strings.Cut(pre.Label, " (") // "System One (decisions)" is named "System One"
	}
	p, err := s.st.CreateProvider(e, me(r))
	if msg, ok := inputError(err); ok {
		f.Del("api_key") // never send a key back to the browser
		s.providersPage(w, r, http.StatusBadRequest, map[string]any{"Error": msg, "Form": f})
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.checkRoutes(r.Context(), p) // a failure shows as "not checked", with a button to ask again
	http.Redirect(w, r, providerPath(p.ID)+"?created=1", http.StatusSeeOther)
}

func providerPath(id int64) string { return "/providers/" + strconv.FormatInt(id, 10) }

// viewProvider resolves the provider in the URL for viewing: its owner and admins may; to anyone
// else it does not exist.
func (s *Server) viewProvider(w http.ResponseWriter, r *http.Request) (*store.Provider, bool) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	p, err := s.st.Provider(id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && p.OwnerID != me(r).ID && !me(r).IsAdmin) {
		http.NotFound(w, r)
		return nil, false
	}
	if err != nil {
		s.fail(w, err)
		return nil, false
	}
	return p, true
}

// targetProvider resolves the provider in the URL for a change, which only its owner may make: an
// admin can look but not change.
func (s *Server) targetProvider(w http.ResponseWriter, r *http.Request) (*store.Provider, bool) {
	p, ok := s.viewProvider(w, r)
	if ok && p.OwnerID != me(r).ID {
		http.Error(w, "Only the provider's owner can change it.", http.StatusForbidden)
		return nil, false
	}
	return p, ok
}

// lendable lists the workspaces a provider's owner may lend it to: every workspace for an admin,
// otherwise the ones the owner is assigned to.
func (s *Server) lendable(p *store.Provider) ([]store.Workspace, error) {
	owner, err := s.st.User(p.OwnerID)
	if err != nil {
		return nil, err
	}
	return s.st.Workspaces(owner)
}

// modelRow is one line of the models checklist.
type modelRow struct {
	ID, Name string // Name is the provider's friendly name, or ""
	Checked  bool
}

// providerPage shows a provider. discovered is the list the provider just offered, if it was asked;
// checked is the set of ticked models, or nil to tick the permitted ones.
func (s *Server) providerPage(w http.ResponseWriter, r *http.Request, status int, p *store.Provider, discovered []providers.Model, checked map[string]bool, data map[string]any) {
	if checked == nil {
		checked = map[string]bool{}
		for _, m := range p.Models {
			checked[m] = true
		}
	}
	names := map[string]string{}
	ids := map[string]bool{}
	for _, m := range discovered {
		names[m.ID], ids[m.ID] = m.Name, true
	}
	for _, m := range p.Models {
		ids[m] = true
	}
	for m := range checked {
		ids[m] = true
	}
	rows := make([]modelRow, 0, len(ids))
	for id := range ids {
		rows = append(rows, modelRow{ID: id, Name: names[id], Checked: checked[id]})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	workspaces, err := s.lendable(p)
	if err != nil {
		s.fail(w, err)
		return
	}
	loans, err := s.st.ProviderLoans(p.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	data["Title"], data["P"], data["Rows"], data["Discovered"] = p.Name+" · Providers", p, rows, discovered != nil
	data["IsOwner"], data["Workspaces"], data["Loans"] = p.OwnerID == me(r).ID, workspaces, loans
	s.render(w, r, status, "provider.html", data)
}

func (s *Server) provider(w http.ResponseWriter, r *http.Request) {
	p, ok := s.viewProvider(w, r)
	if !ok {
		return
	}
	data := map[string]any{}
	switch {
	case r.URL.Query().Has("created"):
		data["Notice"] = "Provider added. Discover its models or enter them below."
	case r.URL.Query().Has("saved"):
		data["Notice"] = "Saved."
	case r.URL.Query().Has("checked"):
		data["Notice"] = "Checked."
	}
	s.providerPage(w, r, http.StatusOK, p, nil, nil, data)
}

func (s *Server) updateProvider(w http.ResponseWriter, r *http.Request) {
	p, ok := s.targetProvider(w, r)
	if !ok {
		return
	}
	f := r.PostForm
	err := s.st.UpdateProvider(p.ID, store.ProviderEdit{
		Name: f.Get("name"), Kind: f.Get("kind"), Endpoint: f.Get("endpoint"),
		Key: f.Get("api_key"), RemoveKey: f.Get("remove_key") == "on",
	})
	if msg, ok := inputError(err); ok {
		f.Del("api_key") // never send a key back to the browser
		s.providerPage(w, r, http.StatusBadRequest, p, nil, nil, map[string]any{"Error": msg, "Form": f})
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	if p, err = s.st.Provider(p.ID); err == nil { // the endpoint or key may have changed
		s.checkRoutes(r.Context(), p)
	}
	http.Redirect(w, r, providerPath(p.ID)+"?saved=1", http.StatusSeeOther)
}

// checkRoutes asks the provider's endpoint which decision routes it serves, with its saved key,
// and records the answer. An Anthropic provider has none, and is not asked.
func (s *Server) checkRoutes(ctx context.Context, p *store.Provider) error {
	if p.Kind != store.KindOpenAI { // the other kinds' APIs are known: nothing to ask
		return s.st.SetProviderRoutes(p.ID, false, false)
	}
	key, err := s.st.ProviderKey(p.ID)
	if err != nil {
		return err
	}
	d, sys, err := providers.Routes(ctx, p.Endpoint, key)
	if err != nil {
		return err
	}
	return s.st.SetProviderRoutes(p.ID, d, sys)
}

// CheckRoutes asks every OpenAI-compatible provider which decision routes it serves. It runs once
// when Autobot starts, so a route added to a gateway since is found without anyone asking.
func (s *Server) CheckRoutes(ctx context.Context) {
	all, err := s.st.Providers(0)
	if err != nil {
		log.Printf("checking decision routes: %v", err)
		return
	}
	for _, p := range all {
		if p.Kind != store.KindOpenAI {
			continue
		}
		if err := s.checkRoutes(ctx, p); err != nil {
			log.Printf("checking decision routes of provider %d: %v", p.ID, err)
		}
	}
}

// recheckRoutes is the provider page's "Check again".
func (s *Server) recheckRoutes(w http.ResponseWriter, r *http.Request) {
	p, ok := s.targetProvider(w, r)
	if !ok {
		return
	}
	err := s.checkRoutes(r.Context(), p)
	if err != nil {
		s.providerPage(w, r, http.StatusBadGateway, p, nil, nil, map[string]any{"Error": "Could not check the decision routes. " + err.Error()})
		return
	}
	http.Redirect(w, r, providerPath(p.ID)+"?checked=1", http.StatusSeeOther)
}

// discoverModels asks the provider, with its saved connection settings, which models it offers.
func (s *Server) discoverModels(w http.ResponseWriter, r *http.Request) {
	p, ok := s.targetProvider(w, r)
	if !ok {
		return
	}
	if store.SearchKind(p.Kind) {
		s.providerPage(w, r, http.StatusBadRequest, p, nil, nil, map[string]any{"Error": "A search provider has no models."})
		return
	}
	key, err := s.st.ProviderKey(p.ID)
	if errors.Is(err, store.ErrKeyUnreadable) {
		s.providerPage(w, r, http.StatusBadRequest, p, nil, nil, map[string]any{"Error": err.Error() + "."})
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	found, err := providers.Discover(r.Context(), p.Kind, p.Endpoint, key)
	if err != nil {
		s.providerPage(w, r, http.StatusBadGateway, p, nil, nil, map[string]any{
			"Error": "Could not list the models. " + err.Error() + " You can still enter model IDs by hand."})
		return
	}
	if found == nil {
		found = []providers.Model{} // asked, and the answer was empty
	}
	if s.checkRoutes(r.Context(), p) == nil {
		if fresh, err := s.st.Provider(p.ID); err == nil {
			p = fresh
		}
	}
	s.providerPage(w, r, http.StatusOK, p, found, nil, map[string]any{})
}

func (s *Server) setProviderModels(w http.ResponseWriter, r *http.Request) {
	p, ok := s.targetProvider(w, r)
	if !ok {
		return
	}
	f := r.PostForm
	picked := map[string]bool{}
	chosen := append([]string{}, f["model"]...)
	for _, m := range chosen {
		picked[m] = true
	}
	for _, line := range strings.Split(f.Get("other"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			chosen = append(chosen, line)
		}
	}
	err := s.st.SetProviderModels(p.ID, chosen)
	if msg, ok := inputError(err); ok {
		s.providerPage(w, r, http.StatusBadRequest, p, nil, picked, map[string]any{"Error": msg, "Form": f})
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, providerPath(p.ID)+"?saved=1", http.StatusSeeOther)
}

func (s *Server) deleteProvider(w http.ResponseWriter, r *http.Request) {
	p, ok := s.targetProvider(w, r)
	if !ok {
		return
	}
	if !strings.EqualFold(strings.TrimSpace(r.PostForm.Get("confirm")), p.Name) {
		s.providerPage(w, r, http.StatusBadRequest, p, nil, nil, map[string]any{
			"Error": "The provider was not deleted: type its name, " + p.Name + ", to confirm."})
		return
	}
	if err := s.st.DeleteProvider(p.ID); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/providers?deleted=1", http.StatusSeeOther)
}

// sampleQuestions is what "Try a decision" asks: enough to show the protocol works end to end.
var sampleInput = `A pull request changes a retry loop: it now retries a failed upload forever, with no delay
between attempts, instead of three times with a one-second back-off.`

func sampleQuestions(kind string) []decisions.Question {
	qs := []decisions.Question{
		{Name: "verdict", Type: "choice", Instructions: "Should a reviewer approve this change?", Options: []decisions.Option{
			{Name: "approve", Description: "The change is safe to merge."},
			{Name: "request-changes", Description: "The change needs work before it can merge."}}},
		{Name: "risky", Type: "yesno", Instructions: "Could this change overload the upload service?"},
	}
	if kind == store.KindSystemOne {
		qs = append(qs, decisions.Question{Name: "severity", Type: "score", Instructions: "How severe is the problem?",
			Levels: []string{"No problem", "Minor", "Serious", "Critical"}})
	}
	return qs
}

// tryDecision asks a permitted model the sample questions and shows its answers.
func (s *Server) tryDecision(w http.ResponseWriter, r *http.Request) {
	p, ok := s.targetProvider(w, r)
	if !ok {
		return
	}
	model := r.PostForm.Get("model")
	permitted := false
	for _, m := range p.Models {
		permitted = permitted || m == model
	}
	api := p.DecisionAPI()
	if !permitted || api == "" {
		s.providerPage(w, r, http.StatusBadRequest, p, nil, nil, map[string]any{"Error": "Choose one of this provider's permitted models."})
		return
	}
	key, err := s.st.ProviderKey(p.ID)
	if err != nil && !errors.Is(err, store.ErrKeyUnreadable) {
		s.fail(w, err)
		return
	}
	answers, err := decisions.Ask(r.Context(), api, p.Endpoint, key, model, sampleInput, sampleQuestions(api))
	data := map[string]any{"Form": r.PostForm}
	if err != nil {
		data["Error"] = "The decision failed: " + err.Error() + "."
		s.providerPage(w, r, http.StatusBadGateway, p, nil, nil, data)
		return
	}
	data["Tried"], data["Notice"] = answers, "Answered by "+model+"."
	s.providerPage(w, r, http.StatusOK, p, nil, nil, data)
}

// trySearch runs a search with a search provider and shows the results.
func (s *Server) trySearch(w http.ResponseWriter, r *http.Request) {
	p, ok := s.targetProvider(w, r)
	if !ok {
		return
	}
	if !store.SearchKind(p.Kind) {
		http.NotFound(w, r)
		return
	}
	key, err := s.st.ProviderKey(p.ID)
	if err != nil && !errors.Is(err, store.ErrKeyUnreadable) {
		s.fail(w, err)
		return
	}
	data := map[string]any{"Form": r.PostForm}
	if key == "" {
		data["Error"] = "Save the API key first."
		s.providerPage(w, r, http.StatusBadRequest, p, nil, nil, data)
		return
	}
	results, err := search.Search(r.Context(), p.Kind, p.Endpoint, key, r.PostForm.Get("q"), 5)
	if err != nil {
		data["Error"] = "The search failed: " + err.Error() + "."
		s.providerPage(w, r, http.StatusBadGateway, p, nil, nil, data)
		return
	}
	data["Results"], data["Searched"] = results, true
	s.providerPage(w, r, http.StatusOK, p, nil, nil, data)
}

// lendProvider sets the workspaces the provider is lent to. Only the owner may, and only to
// workspaces the owner is assigned to.
func (s *Server) lendProvider(w http.ResponseWriter, r *http.Request) {
	p, ok := s.targetProvider(w, r)
	if !ok {
		return
	}
	workspaces, err := s.lendable(p)
	if err != nil {
		s.fail(w, err)
		return
	}
	allowed := map[int64]bool{}
	for _, ws := range workspaces {
		allowed[ws.ID] = true
	}
	var ids []int64
	for _, v := range r.PostForm["workspace"] {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || !allowed[id] {
			http.Error(w, "You can lend a provider only to workspaces you are assigned to.", http.StatusForbidden)
			return
		}
		ids = append(ids, id)
	}
	if err := s.st.LendProvider(p, ids, me(r)); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, providerPath(p.ID)+"?saved=1", http.StatusSeeOther)
}
