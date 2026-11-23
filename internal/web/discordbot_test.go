package web

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/eidonix-ai/autobot/internal/discord"
	"github.com/eidonix-ai/autobot/internal/store"
)

// fakeDiscord answers as Discord does for a bot in one server with one forum.
// discordSent records what the bot sent to the fake Discord.
var discordSent []string

func readBody(r *http.Request) string { b, _ := io.ReadAll(r.Body); return string(b) }

func fakeDiscord(t *testing.T) {
	discordSent = nil
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bot good-token" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"message":"401: Unauthorized"}`)
			return
		}
		switch r.URL.Path {
		case "/users/@me":
			io.WriteString(w, `{"id":"1","username":"flare-autobot"}`)
		case "/users/@me/guilds":
			io.WriteString(w, `[{"id":"g1","name":"Local Inference Lab"}]`)
		case "/guilds/g1/channels":
			io.WriteString(w, `[{"id":"f1","name":"flare-autobot","type":15},{"id":"t1","name":"general","type":0}]`)
		case "/channels/f1/threads":
			discordSent = append(discordSent, "post: "+readBody(r))
			io.WriteString(w, `{"id":"th1","message":{"id":"m1"}}`)
		case "/channels/th1/messages":
			discordSent = append(discordSent, "send: "+readBody(r))
			io.WriteString(w, `{"id":"m2","channel_id":"th1"}`)
		default:
			if r.Method == http.MethodPatch {
				discordSent = append(discordSent, "edit "+r.URL.Path+": "+readBody(r))
				io.WriteString(w, `{}`)
				return
			}
			http.NotFound(w, r)
		}
	}))
	old := discord.API
	discord.API = srv.URL
	t.Cleanup(func() { discord.API = old; srv.Close() })
}

func TestDiscordBotSetup(t *testing.T) {
	fakeDiscord(t)
	e := newEnv(t)
	e.user("derek", true)
	e.user("alice", false)
	ws, _ := e.st.CreateWorkspace("o/r", mustUserByName(t, e, "derek"), "", "")
	e.st.SetMembers(ws.ID, []int64{mustUserByName(t, e, "alice").ID}, mustUserByName(t, e, "derek"))
	pub, priv, _ := ed25519.GenerateKey(nil)
	key := hex.EncodeToString(pub)

	alice := e.signIn("alice")
	if code, _ := alice.get("/admin/discord"); code != http.StatusNotFound {
		t.Fatalf("a user opened the bot's page: %d", code)
	}
	admin := e.signIn("derek")
	if code, _, _ := admin.post("/admin/discord", url.Values{"csrf": {admin.csrf()}, "token": {"bad-token"}, "public_key": {key}}); code != http.StatusBadRequest {
		t.Fatalf("a token Discord refuses was saved: %d", code)
	}
	if code, _, _ := admin.post("/admin/discord", url.Values{"csrf": {admin.csrf()}, "token": {"good-token"}, "public_key": {key}}); code != http.StatusSeeOther {
		t.Fatalf("saving: %d", code)
	}
	if _, body := admin.get("/admin/discord"); !strings.Contains(body, "flare-autobot") || strings.Contains(body, "good-token") {
		t.Fatal("the page does not show the bot, or shows its token")
	}
	if v, _ := e.st.Setting("discord_bot_token"); strings.Contains(v, "good-token") {
		t.Fatal("the token is stored in the clear")
	}

	// A workspace picks only a forum the bot can see.
	if code, _, _ := alice.post("/w/o/r/settings/discord", url.Values{"csrf": {alice.csrf()}, "forum": {"t1"}}); code != http.StatusBadRequest {
		t.Fatalf("a text channel was taken as the forum: %d", code)
	}
	if code, _, _ := alice.post("/w/o/r/settings/discord", url.Values{"csrf": {alice.csrf()}, "forum": {"f1"}}); code != http.StatusSeeOther {
		t.Fatalf("choosing the forum: %d", code)
	}
	if id, name := e.st.DiscordForum(ws.ID); id != "f1" || name != "flare-autobot" {
		t.Fatalf("forum %q %q", id, name)
	}

	// Discord's clicks are taken only when Discord signed them.
	post := func(body, ts, sig string) int {
		req, _ := http.NewRequest("POST", e.srv.URL+"/discord/interactions", bytes.NewReader([]byte(body)))
		req.Header.Set("X-Signature-Ed25519", sig)
		req.Header.Set("X-Signature-Timestamp", ts)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	ping := `{"type":1}`
	good := hex.EncodeToString(ed25519.Sign(priv, []byte("100"+ping)))
	if code := post(ping, "100", good); code != http.StatusOK {
		t.Fatalf("Discord's signed check was refused: %d", code)
	}
	if code := post(ping, "101", good); code != http.StatusUnauthorized {
		t.Fatalf("a forged interaction was taken: %d", code)
	}
}

func mustUserByName(t *testing.T, e *env, name string) *store.User {
	t.Helper()
	u, err := e.st.UserByUsername(name)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
