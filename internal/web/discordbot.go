package web

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/eidonix-ai/autobot/internal/discord"
	"github.com/eidonix-ai/autobot/internal/store"
)

// bot returns a workspace's Discord bot and its public key, or nil when it has none.
func (s *Server) bot(ws *store.Workspace) (*discord.Bot, string) {
	token, key, err := s.st.DiscordBot(ws.ID)
	if err != nil || token == "" {
		return nil, ""
	}
	return &discord.Bot{Token: token}, key
}

// interactionsPath is where a workspace's bot sends clicks: one address per workspace, so each
// click is checked with that workspace's public key and can answer only its own requests.
func interactionsPath(ws *store.Workspace) string { return "/discord/interactions/" + ws.Name }

// discordSettings adds a workspace's Discord state to its settings page: the bot Discord signs it
// in as, and the forums it can see.
func (s *Server) discordSettings(r *http.Request, ws *store.Workspace, data map[string]any) {
	data["ForumID"], data["ForumName"] = s.st.DiscordForum(ws.ID)
	data["Interactions"] = publicURL(r) + interactionsPath(ws)
	b, key := s.bot(ws)
	if b == nil {
		return
	}
	data["Bot"], data["PublicKey"] = true, key
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	me, err := b.Me(ctx)
	if err != nil {
		if _, ok := data["DiscordError"]; !ok {
			data["DiscordError"] = err.Error() + "."
		}
		return
	}
	data["BotName"] = me.Username
	if forums, err := b.Forums(ctx); err == nil {
		data["Forums"] = forums
	} else if _, ok := data["DiscordError"]; !ok {
		data["DiscordError"] = "Could not ask Discord for its forums: " + err.Error() + "."
	}
}

// saveBot sets up a workspace's bot, once Discord accepts its token. Any member may.
func (s *Server) saveBot(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return
	}
	f := r.PostForm
	fail := func(msg string) {
		s.settingsPage(w, r, http.StatusBadRequest, ws, map[string]any{"DiscordError": msg})
	}
	if f.Get("remove") == "on" {
		if err := s.st.RemoveDiscordBot(ws, me(r)); err != nil {
			s.fail(w, err)
			return
		}
		http.Redirect(w, r, ws.Path()+"/settings?saved=1#discord", http.StatusSeeOther)
		return
	}
	token, key := strings.TrimSpace(f.Get("token")), strings.ToLower(strings.TrimSpace(f.Get("public_key")))
	if k, err := hex.DecodeString(key); err != nil || len(k) != 32 {
		fail("The public key is 64 hexadecimal characters: the application's General Information page shows it.")
		return
	}
	if token == "" {
		if old, _, err := s.st.DiscordBot(ws.ID); err == nil {
			token = old
		}
	}
	if token == "" {
		fail("Give the bot's token: the application's Bot page makes one.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if _, err := (discord.Bot{Token: token}).Me(ctx); err != nil {
		fail("Not saved: " + err.Error() + ".")
		return
	}
	if err := s.st.SetDiscordBot(ws, f.Get("token"), key, me(r)); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, ws.Path()+"/settings?saved=1#discord", http.StatusSeeOther)
}

// setForum sets the forum a workspace's cases are posted in: one its bot can see.
func (s *Server) setForum(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.openWorkspace(w, r)
	if !ok {
		return
	}
	id := r.PostForm.Get("forum")
	if id == "" {
		if err := s.st.SetDiscordForum(ws, "", "", me(r)); err != nil {
			s.fail(w, err)
			return
		}
		http.Redirect(w, r, ws.Path()+"/settings?saved=1#discord", http.StatusSeeOther)
		return
	}
	b, _ := s.bot(ws)
	if b == nil {
		http.Error(w, "This workspace has no Discord bot.", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	forums, err := b.Forums(ctx)
	if err != nil {
		s.settingsPage(w, r, http.StatusBadGateway, ws, map[string]any{"DiscordError": "Could not ask Discord for its forums: " + err.Error() + "."})
		return
	}
	for _, f := range forums {
		if f.ID == id {
			if err := s.st.SetDiscordForum(ws, f.ID, f.Name, me(r)); err != nil {
				s.fail(w, err)
				return
			}
			http.Redirect(w, r, ws.Path()+"/settings?saved=1#discord", http.StatusSeeOther)
			return
		}
	}
	s.settingsPage(w, r, http.StatusBadRequest, ws, map[string]any{"DiscordError": "Choose a forum the bot can see."})
}

// interaction is what a workspace's bot sends when someone clicks one of its buttons. Each one is
// checked to be signed by Discord with that workspace's application's public key; anything else
// is refused.
func (s *Server) interaction(w http.ResponseWriter, r *http.Request) {
	ws, err := s.st.Workspace(r.PathValue("owner") + "/" + r.PathValue("repo"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	_, key, err := s.st.DiscordBot(ws.ID)
	if err != nil || key == "" {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || !discord.Verify(key, r.Header.Get("X-Signature-Ed25519"), r.Header.Get("X-Signature-Timestamp"), body) {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	var in struct {
		Type int `json:"type"`
	}
	json.Unmarshal(body, &in)
	w.Header().Set("Content-Type", "application/json")
	if in.Type == 1 { // Discord checking the endpoint
		io.WriteString(w, `{"type":1}`)
		return
	}
	s.approvalClick(w, ws, body)
}
