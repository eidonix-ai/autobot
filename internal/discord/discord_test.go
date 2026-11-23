package discord

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A post mentions no one, and a long one is cut with the whole text attached.
func TestPost(t *testing.T) {
	var got []map[string]any
	var files []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("wait") != "true" {
			t.Error("not waiting for Discord's answer")
		}
		var p map[string]any
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
			r.ParseMultipartForm(1 << 20)
			json.Unmarshal([]byte(r.FormValue("payload_json")), &p)
			f, _, _ := r.FormFile("files[0]")
			b, _ := io.ReadAll(f)
			files = append(files, string(b))
		} else {
			json.NewDecoder(r.Body).Decode(&p)
		}
		got = append(got, p)
		io.WriteString(w, `{"id":"1"}`)
	}))
	defer srv.Close()
	if err := Post(context.Background(), srv.URL, "@everyone the review is done"); err != nil {
		t.Fatal(err)
	}
	am, _ := got[0]["allowed_mentions"].(map[string]any)
	if parse, ok := am["parse"].([]any); !ok || len(parse) != 0 || got[0]["content"] != "@everyone the review is done" {
		t.Fatalf("mentions not turned off: %+v", got[0])
	}
	long := strings.Repeat("A finding. ", 500)
	if err := Post(context.Background(), srv.URL, long); err != nil {
		t.Fatal(err)
	}
	content, _ := got[1]["content"].(string)
	if len(files) != 1 || files[0] != strings.TrimSpace(long) || len([]rune(content)) > maxContent || !strings.Contains(content, "attached") {
		t.Fatalf("long post: %d characters shown, %d files", len(content), len(files))
	}
	if am, _ := got[1]["allowed_mentions"].(map[string]any); am == nil {
		t.Fatal("a long post can mention people")
	}
}
