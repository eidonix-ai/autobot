package web

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/eidonix-ai/autobot/internal/decisions"
	"github.com/eidonix-ai/autobot/internal/providers"
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
	list, err := s.st.Providers()
	if err != nil {
		s.fail(w, err)
		return
	}
	if data == nil {
		data = map[string]any{}
	}
	if r.URL.Query().Has("deleted") {
		data["Notice"] = "Provider deleted."
	}
	available, err := s.st.AvailableModels()
	if err != nil {
		s.fail(w, err)
		return
	}
	data["Title"], data["Providers"], data["Presets"], data["Available"] = "Providers", list, presets, available
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
	http.Redirect(w, r, providerPath(p.ID)+"?created=1", http.StatusSeeOther)
}

func providerPath(id int64) string { return "/admin/providers/" + strconv.FormatInt(id, 10) }

// targetProvider resolves the provider in the URL.
func (s *Server) targetProvider(w http.ResponseWriter, r *http.Request) (*store.Provider, bool) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	p, err := s.st.Provider(id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return nil, false
	}
	if err != nil {
		s.fail(w, err)
		return nil, false
	}
	return p, true
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
	data["Title"], data["P"], data["Rows"], data["Discovered"] = p.Name+" · Providers", p, rows, discovered != nil
	s.render(w, r, status, "provider.html", data)
}

func (s *Server) provider(w http.ResponseWriter, r *http.Request) {
	p, ok := s.targetProvider(w, r)
	if !ok {
		return
	}
	data := map[string]any{}
	switch {
	case r.URL.Query().Has("created"):
		data["Notice"] = "Provider added. Discover its models or enter them below."
	case r.URL.Query().Has("saved"):
		data["Notice"] = "Saved."
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
	http.Redirect(w, r, providerPath(p.ID)+"?saved=1", http.StatusSeeOther)
}

// discoverModels asks the provider, with its saved connection settings, which models it offers.
func (s *Server) discoverModels(w http.ResponseWriter, r *http.Request) {
	p, ok := s.targetProvider(w, r)
	if !ok {
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
	http.Redirect(w, r, "/admin/providers?deleted=1", http.StatusSeeOther)
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
	if !permitted || (p.Kind != store.KindDecisions && p.Kind != store.KindSystemOne) {
		s.providerPage(w, r, http.StatusBadRequest, p, nil, nil, map[string]any{"Error": "Choose one of this provider's permitted models."})
		return
	}
	key, err := s.st.ProviderKey(p.ID)
	if err != nil && !errors.Is(err, store.ErrKeyUnreadable) {
		s.fail(w, err)
		return
	}
	answers, err := decisions.Ask(r.Context(), p.Kind, p.Endpoint, key, model, sampleInput, sampleQuestions(p.Kind))
	data := map[string]any{"Form": r.PostForm}
	if err != nil {
		data["Error"] = "The decision failed: " + err.Error() + "."
		s.providerPage(w, r, http.StatusBadGateway, p, nil, nil, data)
		return
	}
	data["Tried"], data["Notice"] = answers, "Answered by "+model+"."
	s.providerPage(w, r, http.StatusOK, p, nil, nil, data)
}
