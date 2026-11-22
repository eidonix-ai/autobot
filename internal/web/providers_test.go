package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eidonix-ai/autobot/internal/store"
)

func storeEdit(name, endpoint, key string) store.ProviderEdit {
	return store.ProviderEdit{Name: name, Kind: store.KindOpenAI, Endpoint: endpoint, Key: key}
}

const providerSecret = "sk-test-SECRET-key-1234"

// modelServer is a provider endpoint: /models lists two models, and /denied/models answers 401
// and echoes whatever key it was sent, as a careless server might.
func modelServer(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/denied/") {
			http.Error(w, "bad key "+r.Header.Get("Authorization"), http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"data":[{"id":"vendor/small","name":"Vendor: Small"},{"id":"vendor/large"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestProviderKeyIsNeverExposed(t *testing.T) {
	e := newEnv(t)
	owner := e.user("derek", true)
	admin := e.signIn("derek")
	tok := admin.csrf()
	models := modelServer(t)

	code, loc, _ := admin.post("/providers", url.Values{"csrf": {tok}, "preset": {"custom"}, "name": {"Local"},
		"endpoint": {models.URL + "/"}, "api_key": {providerSecret}})
	if code != http.StatusSeeOther || loc != "/providers/1?created=1" {
		t.Fatalf("create: %d %q", code, loc)
	}
	p := strings.SplitN(loc, "?", 2)[0]
	if key, err := e.st.ProviderKey(1); err != nil || key != providerSecret {
		t.Fatalf("the key was not saved: %q, %v", key, err)
	}

	// Every page that could show the key: the list, the page, and each way a form comes back.
	pages := map[string]string{}
	_, pages["list"] = admin.get("/providers")
	_, pages["page"] = admin.get(p)
	var status int
	status, _, pages["discover"] = admin.post(p+"/discover", url.Values{"csrf": {tok}})
	if status != http.StatusOK || !strings.Contains(pages["discover"], "Vendor: Small") {
		t.Fatalf("discover: %d", status)
	}
	status, _, pages["bad update"] = admin.post(p, url.Values{"csrf": {tok}, "name": {"Local"}, "kind": {"openai"},
		"endpoint": {"not a url"}, "api_key": {providerSecret}})
	if status != http.StatusBadRequest || !strings.Contains(pages["bad update"], "full web address") {
		t.Fatalf("bad update: %d", status)
	}
	status, _, pages["bad create"] = admin.post("/providers", url.Values{"csrf": {tok}, "preset": {"custom"},
		"name": {"Other"}, "endpoint": {"ftp://x"}, "api_key": {providerSecret}})
	if status != http.StatusBadRequest {
		t.Fatalf("bad create: %d", status)
	}
	// A provider whose endpoint refuses the key and echoes it.
	denied, err := e.st.CreateProvider(storeEdit("Denied", models.URL+"/denied", providerSecret), owner)
	if err != nil {
		t.Fatal(err)
	}
	status, _, pages["denied"] = admin.post("/providers/"+itoa(denied.ID)+"/discover", url.Values{"csrf": {tok}})
	if status != http.StatusBadGateway || !strings.Contains(pages["denied"], "401 Unauthorized: check the API key") {
		t.Fatalf("denied discovery: %d", status)
	}
	for name, body := range pages {
		if strings.Contains(body, providerSecret) {
			t.Errorf("the %s page shows the API key", name)
		}
	}

	// And the database file, including its write-ahead log, holds it only sealed.
	files, _ := filepath.Glob(filepath.Join(e.dir, "test.db*"))
	var all []byte
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, b...)
	}
	if !strings.Contains(string(all), "Denied") {
		t.Fatal("the database files hold no provider data, so this check proves nothing")
	}
	if strings.Contains(string(all), providerSecret) {
		t.Fatal("the API key is stored in the clear")
	}
}

func TestProviderPages(t *testing.T) {
	e := newEnv(t)
	e.user("derek", true)
	admin := e.signIn("derek")
	tok := admin.csrf()
	models := modelServer(t)

	// A preset fills in kind and endpoint, and names the provider after itself.
	code, loc, _ := admin.post("/providers", url.Values{"csrf": {tok}, "preset": {"anthropic"}, "api_key": {providerSecret}})
	if code != http.StatusSeeOther {
		t.Fatalf("add preset: %d", code)
	}
	p, err := e.st.Provider(1)
	if err != nil || p.Name != "Anthropic" || p.Kind != "anthropic" || p.Endpoint != "https://api.anthropic.com" || !p.HasKey {
		t.Fatalf("preset: %+v, %v", p, err)
	}
	if code, _, body := admin.post("/providers", url.Values{"csrf": {tok}, "preset": {"anthropic"}}); code != http.StatusBadRequest || !strings.Contains(body, "You already have a provider named") {
		t.Fatalf("duplicate name: %d", code)
	}

	// Connection: a blank key keeps the saved one; the checkbox removes it.
	path := loc[:strings.Index(loc, "?")]
	conn := url.Values{"csrf": {tok}, "name": {"Anthropic"}, "kind": {"openai"}, "endpoint": {models.URL + "//"}}
	if code, _, _ := admin.post(path, conn); code != http.StatusSeeOther {
		t.Fatalf("save connection: %d", code)
	}
	if p, _ = e.st.Provider(1); p.Kind != "openai" || p.Endpoint != models.URL || !p.HasKey {
		t.Fatalf("after save: %+v", p)
	}
	conn.Set("remove_key", "on")
	admin.post(path, conn)
	if p, _ = e.st.Provider(1); p.HasKey {
		t.Fatal("the key was not removed")
	}

	// Models: discovery fills the checklist with display names; saving replaces the permitted set
	// with the ticked ones plus the typed ones.
	_, _, body := admin.post(path+"/discover", url.Values{"csrf": {tok}})
	if !strings.Contains(body, "Vendor: Small") || !strings.Contains(body, `value="vendor/large"`) {
		t.Fatalf("discovered list missing:\n%s", body)
	}
	code, _, _ = admin.post(path+"/models", url.Values{"csrf": {tok}, "model": {"vendor/small", "vendor/small"}, "other": {" local/one \r\n\nvendor/small\nlocal/two\n"}})
	if code != http.StatusSeeOther {
		t.Fatalf("save models: %d", code)
	}
	p, _ = e.st.Provider(1)
	if strings.Join(p.Models, ",") != "local/one,local/two,vendor/small" {
		t.Fatalf("permitted models: %v", p.Models)
	}
	if _, body := admin.get(path); !strings.Contains(body, `value="local/one" checked`) {
		t.Fatal("permitted models are not the checklist")
	}
	// A bad ID is refused, the typed text comes back, and nothing changes.
	code, _, body = admin.post(path+"/models", url.Values{"csrf": {tok}, "other": {"has space inside\nfine"}})
	if code != http.StatusBadRequest || !strings.Contains(body, "has space inside") {
		t.Fatalf("bad model ID: %d", code)
	}
	if p, _ = e.st.Provider(1); len(p.Models) != 3 {
		t.Fatalf("a refused save changed the models: %v", p.Models)
	}

	// Deleting needs the name typed.
	if code, _, _ := admin.post(path+"/delete", url.Values{"csrf": {tok}, "confirm": {"nope"}}); code != http.StatusBadRequest {
		t.Fatalf("delete without the name: %d", code)
	}
	if code, loc, _ := admin.post(path+"/delete", url.Values{"csrf": {tok}, "confirm": {"anthropic"}}); code != http.StatusSeeOther || loc != "/providers?deleted=1" {
		t.Fatalf("delete: %d %q", code, loc)
	}
	if _, err := e.st.Provider(1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the provider survived its deletion: %v", err)
	}
}

// A provider is its owner's: only the owner changes it or lends it, an admin may only look, and
// other users cannot see it. A workspace uses only what is lent to it, and only while the owner is
// enabled.
func TestProvidersBelongToTheirOwner(t *testing.T) {
	e := newEnv(t)
	derek := e.user("derek", true)
	e.user("bob", true)
	alice := e.user("alice", false)
	wsA, _ := e.st.CreateWorkspace("o/a", derek, "", "")
	wsB, _ := e.st.CreateWorkspace("o/b", derek, "", "")
	e.st.UpdateUser(alice.ID, store.UserUpdate{Workspaces: []int64{wsA.ID}}, derek)
	mine, err := e.st.CreateProvider(storeEdit("OpenRouter", "https://openrouter.ai/api/v1", providerSecret), derek)
	if err != nil {
		t.Fatal(err)
	}
	path := "/providers/" + itoa(mine.ID)

	// Another user cannot see it at all.
	a := e.signIn("alice")
	atok := a.csrf()
	if _, body := a.get("/providers"); strings.Contains(body, `href="`+path+`"`) {
		t.Error("another user's provider is listed")
	}
	form := url.Values{"csrf": {atok}, "name": {"Mine"}, "kind": {"openai"}, "endpoint": {"https://x.example"}, "confirm": {"OpenRouter"}, "workspace": {itoa(wsA.ID)}}
	if code, _ := a.get(path); code != http.StatusNotFound {
		t.Errorf("another user opens it: %d", code)
	}
	for _, op := range []string{"", "/discover", "/models", "/lend", "/delete"} {
		if code, _, _ := a.post(path+op, form); code != http.StatusNotFound {
			t.Errorf("another user POSTs %s: %d", op, code)
		}
	}

	// An admin who does not own it can look, but change nothing.
	b := e.signIn("bob")
	btok := b.csrf()
	if code, body := b.get(path); code != http.StatusOK || !strings.Contains(body, "belongs to derek") || strings.Contains(body, providerSecret) {
		t.Fatalf("admin view: %d", code)
	}
	form.Set("csrf", btok)
	for _, op := range []string{"", "/discover", "/models", "/lend", "/delete", "/try"} {
		if code, _, _ := b.post(path+op, form); code != http.StatusForbidden {
			t.Errorf("admin who is not the owner POSTs %s: %d", op, code)
		}
	}
	if p, _ := e.st.Provider(mine.ID); p.Name != "OpenRouter" {
		t.Fatalf("an admin changed another owner's provider: %q", p.Name)
	}
	if loans, _ := e.st.ProviderLoans(mine.ID); len(loans) != 0 {
		t.Fatal("an admin lent another owner's provider")
	}

	// A user lends her own provider, but only to workspaces she is assigned to.
	hers, _ := e.st.CreateProvider(storeEdit("Hers", "https://api.deepseek.com", providerSecret), alice)
	e.st.SetProviderModels(hers.ID, []string{"deepseek-chat"})
	hpath := "/providers/" + itoa(hers.ID)
	if code, _, _ := a.post(hpath+"/lend", url.Values{"csrf": {atok}, "workspace": {itoa(wsB.ID)}}); code != http.StatusForbidden {
		t.Errorf("lent to a workspace she is not assigned to: %d", code)
	}
	if code, _, _ := a.post(hpath+"/lend", url.Values{"csrf": {atok}, "workspace": {itoa(wsA.ID)}}); code != http.StatusSeeOther {
		t.Fatalf("lend: %d", code)
	}
	if got, _ := e.st.AvailableModels(wsA.ID); len(got) != 1 || got[0].Model != "deepseek-chat" || got[0].Owner != "alice" {
		t.Fatalf("available in o/a: %+v", got)
	}
	if got, _ := e.st.AvailableModels(wsB.ID); len(got) != 0 {
		t.Fatalf("available in o/b, where nothing is lent: %+v", got)
	}

	// A disabled owner's providers stop working everywhere.
	e.st.UpdateUser(alice.ID, store.UserUpdate{Disabled: true, Workspaces: []int64{wsA.ID}}, derek)
	if got, _ := e.st.AvailableModels(wsA.ID); len(got) != 0 {
		t.Fatalf("a disabled owner's models are still available: %+v", got)
	}
}
