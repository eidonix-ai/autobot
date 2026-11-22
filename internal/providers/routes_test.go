package providers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A route counts as served only when the server itself answers it; a missing route, a refused key
// and a catch-all web page must not.
func TestRoutes(t *testing.T) {
	answer := map[string]func(http.ResponseWriter){}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("%s with %q", r.Method, r.Header.Get("Authorization"))
		}
		if f, ok := answer[r.URL.Path]; ok {
			f(w)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	jsonStatus := func(code int) func(http.ResponseWriter) {
		return func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			io.WriteString(w, `{"error":"x"}`)
		}
	}
	check := func(name string, wantD, wantS, wantErr bool) {
		t.Helper()
		d, s, err := Routes(context.Background(), srv.URL+"/v1/", "k")
		if d != wantD || s != wantS || (err != nil) != wantErr {
			t.Errorf("%s: decisions %v, systemone %v, err %v", name, d, s, err)
		}
	}
	check("neither", false, false, false)
	answer["/v1/decisions"] = jsonStatus(http.StatusUnprocessableEntity)
	check("decisions", true, false, false)
	answer["/v1/systemone"] = jsonStatus(http.StatusBadRequest)
	check("both", true, true, false)
	answer["/v1/systemone"] = func(w http.ResponseWriter) { w.Header().Set("Content-Type", "text/html"); io.WriteString(w, "<html>") }
	check("catch-all page", true, false, true)
	answer["/v1/systemone"] = jsonStatus(http.StatusUnauthorized)
	check("refused key", true, false, true)
}
