package discord

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Messages mention no one unless a role is named, never show link previews, and a forum post
// carries its first message inside "message".
func TestMessages(t *testing.T) {
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bot tok" {
			t.Errorf("auth %q", r.Header.Get("Authorization"))
		}
		var p map[string]any
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
			r.ParseMultipartForm(1 << 20)
			json.Unmarshal([]byte(r.FormValue("payload_json")), &p)
		} else {
			json.NewDecoder(r.Body).Decode(&p)
		}
		p["_path"] = r.URL.Path
		got = append(got, p)
		io.WriteString(w, `{"id":"9","channel_id":"5","message":{"id":"10"}}`)
	}))
	defer srv.Close()
	API = srv.URL
	b := Bot{Token: "tok"}
	ctx := context.Background()
	if _, err := b.Send(ctx, "5", Message{Text: "@everyone look at https://evil.example/?d=x"}); err != nil {
		t.Fatal(err)
	}
	am := got[0]["allowed_mentions"].(map[string]any)
	if len(am["parse"].([]any)) != 0 || am["roles"] != nil || got[0]["flags"].(float64) != suppressEmbeds {
		t.Fatalf("send: %+v", got[0])
	}
	b.Send(ctx, "5", Message{Text: "please approve", Role: "77", Buttons: []Button{{ID: "a:1", Label: "Approve", Style: 3}}})
	am = got[1]["allowed_mentions"].(map[string]any)
	if roles := am["roles"].([]any); len(roles) != 1 || roles[0] != "77" || len(am["parse"].([]any)) != 0 {
		t.Fatalf("role ping: %+v", am)
	}
	post, err := b.CreatePost(ctx, "42", "PR #7: Retry uploads", Message{Text: strings.Repeat("x", 2500)})
	if err != nil || post.Channel != "9" || post.ID != "10" {
		t.Fatalf("%+v %v", post, err)
	}
	msg := got[2]["message"].(map[string]any)
	if got[2]["_path"] != "/channels/42/threads" || got[2]["name"] != "PR #7: Retry uploads" || msg["flags"].(float64) != suppressEmbeds ||
		len(msg["allowed_mentions"].(map[string]any)["parse"].([]any)) != 0 || !strings.Contains(msg["content"].(string), "attached") {
		t.Fatalf("forum post: %+v", got[2])
	}
}

func TestVerify(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	body := []byte(`{"type":1}`)
	sig := hex.EncodeToString(ed25519.Sign(priv, append([]byte("123"), body...)))
	key := hex.EncodeToString(pub)
	if !Verify(key, sig, "123", body) {
		t.Fatal("a good signature was refused")
	}
	if Verify(key, sig, "124", body) || Verify(key, sig, "123", []byte(`{"type":2}`)) || Verify("00", sig, "123", body) {
		t.Fatal("a bad signature passed")
	}
}
