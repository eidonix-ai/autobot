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
	e.user("derek", true)
	admin := e.signIn("derek")
	tok := admin.csrf()
	models := modelServer(t)

	code, loc, _ := admin.post("/admin/providers", url.Values{"csrf": {tok}, "preset": {"custom"}, "name": {"Local"},
		"endpoint": {models.URL + "/"}, "api_key": {providerSecret}})
	if code != http.StatusSeeOther || loc != "/admin/providers/1?created=1" {
		t.Fatalf("create: %d %q", code, loc)
	}
	p := strings.SplitN(loc, "?", 2)[0]
	if key, err := e.st.ProviderKey(1); err != nil || key != providerSecret {
		t.Fatalf("the key was not saved: %q, %v", key, err)
	}

	// Every page that could show the key: the list, the page, and each way a form comes back.
	pages := map[string]string{}
	_, pages["list"] = admin.get("/admin/providers")
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
	status, _, pages["bad create"] = admin.post("/admin/providers", url.Values{"csrf": {tok}, "preset": {"custom"},
		"name": {"Other"}, "endpoint": {"ftp://x"}, "api_key": {providerSecret}})
	if status != http.StatusBadRequest {
		t.Fatalf("bad create: %d", status)
	}
	// A provider whose endpoint refuses the key and echoes it.
	denied, err := e.st.CreateProvider(storeEdit("Denied", models.URL+"/denied", providerSecret), e.user("other", true))
	if err != nil {
		t.Fatal(err)
	}
	status, _, pages["denied"] = admin.post("/admin/providers/"+itoa(denied.ID)+"/discover", url.Values{"csrf": {tok}})
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
	code, loc, _ := admin.post("/admin/providers", url.Values{"csrf": {tok}, "preset": {"anthropic"}, "api_key": {providerSecret}})
	if code != http.StatusSeeOther {
		t.Fatalf("add preset: %d", code)
	}
	p, err := e.st.Provider(1)
	if err != nil || p.Name != "Anthropic" || p.Kind != "anthropic" || p.Endpoint != "https://api.anthropic.com" || !p.HasKey {
		t.Fatalf("preset: %+v, %v", p, err)
	}
	if code, _, body := admin.post("/admin/providers", url.Values{"csrf": {tok}, "preset": {"anthropic"}}); code != http.StatusBadRequest || !strings.Contains(body, "already exists") {
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
	if code, loc, _ := admin.post(path+"/delete", url.Values{"csrf": {tok}, "confirm": {"anthropic"}}); code != http.StatusSeeOther || loc != "/admin/providers?deleted=1" {
		t.Fatalf("delete: %d %q", code, loc)
	}
	if _, err := e.st.Provider(1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the provider survived its deletion: %v", err)
	}
}

func TestProvidersAreAdminOnly(t *testing.T) {
	e := newEnv(t)
	admin := e.user("derek", true)
	e.user("alice", false)
	if _, err := e.st.CreateProvider(storeEdit("OpenRouter", "https://openrouter.ai/api/v1", ""), admin); err != nil {
		t.Fatal(err)
	}
	alice := e.signIn("alice")
	tok := alice.csrf()
	if _, body := alice.get("/"); strings.Contains(body, "/admin/providers") {
		t.Error("a regular user is shown the Providers link")
	}
	for _, p := range []string{"/admin/providers", "/admin/providers/1"} {
		if code, _ := alice.get(p); code != http.StatusNotFound {
			t.Errorf("GET %s as a regular user: %d", p, code)
		}
	}
	form := url.Values{"csrf": {tok}, "preset": {"openai"}, "name": {"Mine"}, "kind": {"openai"}, "endpoint": {"https://x.example"}, "confirm": {"OpenRouter"}}
	for _, p := range []string{"/admin/providers", "/admin/providers/1", "/admin/providers/1/discover", "/admin/providers/1/models", "/admin/providers/1/delete"} {
		if code, _, _ := alice.post(p, form); code != http.StatusNotFound {
			t.Errorf("POST %s as a regular user: %d", p, code)
		}
	}
	if list, _ := e.st.Providers(); len(list) != 1 {
		t.Errorf("a regular user changed the providers: %d left", len(list))
	}
}
