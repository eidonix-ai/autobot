package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

const secret = "sk-test-SECRET-key"

// serve answers every request with body and records the requests it saw.
func serve(t *testing.T, body string) (*httptest.Server, *[]*http.Request) {
	t.Helper()
	var seen []*http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func TestDiscoverResponseShapes(t *testing.T) {
	cases := map[string]struct {
		body string
		want []Model
	}{
		"data object, sorted, de-duplicated, Gemini prefix dropped": {
			`{"object":"list","data":[{"id":"models/gemini-b","name":"Gemini B"},{"id":"a"},{"id":"a","name":"The A"},{"id":"models/gemini-b"}]}`,
			[]Model{{"a", "The A", 0}, {"gemini-b", "Gemini B", 0}},
		},
		"bare array (Together)": {
			`[{"id":"meta/llama","display_name":"Llama"},{"id":"mistral"}]`,
			[]Model{{"meta/llama", "Llama", 0}, {"mistral", "", 0}},
		},
		"models object with name": {
			`{"models":[{"name":"models/gemini-pro","display_name":"Gemini Pro"},{"id":"x"}]}`,
			[]Model{{"gemini-pro", "Gemini Pro", 0}, {"x", "", 0}},
		},
	}
	for name, c := range cases {
		srv, seen := serve(t, c.body)
		got, err := Discover(context.Background(), "openai", srv.URL+"/v1/", secret)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %+v, want %+v", name, got, c.want)
		}
		r := (*seen)[0]
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer "+secret || r.Header.Get("User-Agent") != "autobot" {
			t.Errorf("%s: request %s with headers %v", name, r.URL.Path, r.Header)
		}
	}
}

func TestDiscoverWithoutKeySendsNoCredentials(t *testing.T) {
	srv, seen := serve(t, `{"data":[]}`)
	if _, err := Discover(context.Background(), "openai", srv.URL, ""); err != nil {
		t.Fatal(err)
	}
	if h := (*seen)[0].Header; h.Get("Authorization") != "" || h.Get("x-api-key") != "" {
		t.Fatalf("credentials sent with no key: %v", h)
	}
}

func TestDiscoverAnthropicHeadersAndPages(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("x-api-key") != secret || r.Header.Get("anthropic-version") != "2023-06-01" || r.Header.Get("Authorization") != "" {
			http.Error(w, "bad request: "+r.URL.String(), http.StatusBadRequest)
			return
		}
		after := r.URL.Query().Get("after_id")
		asked = append(asked, after)
		switch after {
		case "":
			w.Write([]byte(`{"data":[{"id":"claude-b","display_name":"Claude B"}],"has_more":true,"last_id":"claude-b"}`))
		case "claude-b":
			w.Write([]byte(`{"data":[{"id":"claude-a","display_name":"Claude A"}],"has_more":false,"last_id":"claude-a"}`))
		default:
			http.Error(w, "unexpected after_id", http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	got, err := Discover(context.Background(), "anthropic", srv.URL, secret)
	if err != nil {
		t.Fatal(err)
	}
	want := []Model{{"claude-a", "Claude A", 0}, {"claude-b", "Claude B", 0}}
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(asked, []string{"", "claude-b"}) {
		t.Fatalf("got %+v after asking %q", got, asked)
	}
}

func TestDiscoverAnthropicStopsAtTenPages(t *testing.T) {
	pages := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		w.Write([]byte(`{"data":[{"id":"m` + string(rune('a'+pages)) + `"}],"has_more":true,"last_id":"x"}`))
	}))
	defer srv.Close()
	if _, err := Discover(context.Background(), "anthropic", srv.URL, secret); err != nil || pages != 10 {
		t.Fatalf("%d pages, %v", pages, err)
	}
}

func TestDiscoverErrorsNeverShowTheKey(t *testing.T) {
	status := func(code int) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// A careless server that echoes the key back must not leak it into the message.
			http.Error(w, "bad key "+r.Header.Get("Authorization")+r.Header.Get("x-api-key"), code)
		}))
		defer srv.Close()
		var msgs []string
		for _, kind := range []string{"openai", "anthropic"} {
			_, err := Discover(context.Background(), kind, srv.URL, secret)
			if err == nil {
				t.Fatalf("status %d: no error", code)
			}
			msgs = append(msgs, err.Error())
		}
		return strings.Join(msgs, "\n")
	}
	if msg := status(401); !strings.Contains(msg, "answered 401 Unauthorized: check the API key.") {
		t.Errorf("401: %q", msg)
	}
	if msg := status(404); !strings.Contains(msg, "404 Not Found") {
		t.Errorf("404: %q", msg)
	}

	// An endpoint that is not there, one that sends junk, and one that redirects elsewhere.
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	junk, _ := serve(t, `<html>not json</html>`)
	other, otherSeen := serve(t, `{"data":[{"id":"stolen"}]}`)
	redirect := httptest.NewServer(http.RedirectHandler(other.URL, http.StatusFound))
	defer redirect.Close()
	for name, endpoint := range map[string]string{"dead": dead.URL, "junk": junk.URL, "redirect": redirect.URL} {
		_, err := Discover(context.Background(), "anthropic", endpoint, secret)
		if err == nil {
			t.Errorf("%s: no error", name)
		} else if strings.Contains(err.Error(), secret) {
			t.Errorf("%s: the key is in the error: %v", name, err)
		}
	}
	if len(*otherSeen) != 0 {
		t.Errorf("the redirect was followed, sending the key to another host")
	}
}
