package discord

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

func TestReplyAndChannelHelpers(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/channels/ch1/messages":
			gotPath = r.URL.Path
			json.NewDecoder(r.Body).Decode(&gotBody)
			json.NewEncoder(w).Encode(Posted{Channel: "ch1", ID: "m2"})
		case r.Method == "GET" && r.URL.Path == "/channels/ch1":
			json.NewEncoder(w).Encode(map[string]any{"id": "ch1", "name": "general", "type": 0})
		case r.Method == "GET" && r.URL.Path == "/channels/ch1/messages":
			// Discord returns newest first.
			json.NewEncoder(w).Encode([]map[string]any{
				{"id": "102", "content": "second", "author": map[string]any{"username": "bob", "global_name": "Bob"}},
				{"id": "101", "content": "first", "author": map[string]any{"username": "alice"}},
			})
		case r.Method == "GET" && r.URL.Path == "/gateway/bot":
			json.NewEncoder(w).Encode(map[string]any{"url": "wss://gateway.discord.gg"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	oldAPI := API
	API = srv.URL
	defer func() { API = oldAPI }()

	b := Bot{Token: "tok"}
	p, err := b.Reply(t.Context(), "ch1", "m1", Message{Text: "Check @everyone and <@&123> https://example.com"})
	if err != nil || p.ID != "m2" || gotPath != "/channels/ch1/messages" {
		t.Fatalf("Reply: %+v %v", p, err)
	}
	if int(gotBody["flags"].(float64)) != suppressEmbeds {
		t.Fatalf("flags = %v, want suppressEmbeds", gotBody["flags"])
	}
	am := gotBody["allowed_mentions"].(map[string]any)
	if parse := am["parse"].([]any); len(parse) != 0 {
		t.Fatalf("allowed_mentions.parse must be empty: %v", parse)
	}
	if am["replied_user"] != true {
		t.Fatalf("replied_user = %v", am["replied_user"])
	}
	ref := gotBody["message_reference"].(map[string]any)
	if ref["message_id"] != "m1" {
		t.Fatalf("message_reference = %v", ref)
	}

	ch, err := b.ChannelInfo(t.Context(), "ch1")
	if err != nil || ch.Name != "general" {
		t.Fatalf("ChannelInfo: %+v %v", ch, err)
	}
	msgs, err := b.RecentMessages(t.Context(), "ch1", "103", 10)
	if err != nil || len(msgs) != 2 || msgs[0].Content != "first" || msgs[1].Content != "second" || msgs[1].Author != "Bob" {
		t.Fatalf("RecentMessages: %+v %v", msgs, err)
	}
	gw, err := b.GatewayURL(t.Context())
	if err != nil || gw != "wss://gateway.discord.gg/?v=10&encoding=json" {
		t.Fatalf("GatewayURL: %q %v", gw, err)
	}
}

func TestRunGatewayMentionsAndFiltering(t *testing.T) {
	mentions := make(chan MentionEvent, 4)
	ready := make(chan string, 1)
	var gotIntents int
	var gotToken string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, _, err := ws.UpgradeHTTP(r, w)
		if err != nil {
			return
		}
		defer conn.Close()
		send := func(v any) {
			b, _ := json.Marshal(v)
			wsutil.WriteServerText(conn, b)
		}
		// 1. Hello
		send(map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 60000}})
		// 2. Read Identify
		raw, _, err := wsutil.ReadClientData(conn)
		if err != nil {
			return
		}
		var id struct {
			Op int `json:"op"`
			D  struct {
				Token   string `json:"token"`
				Intents int    `json:"intents"`
			} `json:"d"`
		}
		json.Unmarshal(raw, &id)
		gotToken, gotIntents = id.D.Token, id.D.Intents

		// 3. READY
		send(map[string]any{"op": 0, "s": 1, "t": "READY", "d": map[string]any{
			"session_id": "s1",
			"user":       map[string]any{"id": "bot1", "username": "flare-autobot"},
		}})
		// 4. Bot-authored message mentioning the bot (must be ignored)
		send(map[string]any{"op": 0, "s": 2, "t": "MESSAGE_CREATE", "d": map[string]any{
			"id": "m_bot", "guild_id": "g1", "channel_id": "c1", "content": "<@bot1> loop",
			"author": map[string]any{"id": "otherbot", "username": "other", "bot": true},
		}})
		// 5. DM mentioning the bot (no guild_id: must be ignored)
		send(map[string]any{"op": 0, "s": 3, "t": "MESSAGE_CREATE", "d": map[string]any{
			"id": "m_dm", "channel_id": "c_dm", "content": "<@bot1> dm",
			"author": map[string]any{"id": "u1", "username": "alice"},
		}})
		// 6. Regular guild message without mention (must be ignored)
		send(map[string]any{"op": 0, "s": 4, "t": "MESSAGE_CREATE", "d": map[string]any{
			"id": "m_chat", "guild_id": "g1", "channel_id": "c1", "content": "hello everyone",
			"author": map[string]any{"id": "u1", "username": "alice"},
		}})
		// 7. Human guild mention with reply context (must fire!)
		send(map[string]any{"op": 0, "s": 5, "t": "MESSAGE_CREATE", "d": map[string]any{
			"id": "m_ok", "guild_id": "g1", "channel_id": "c1", "content": "<@!bot1> status of PR #498?",
			"author":   map[string]any{"id": "u1", "username": "alice", "global_name": "Alice"},
			"mentions": []map[string]any{{"id": "bot1"}},
			"referenced_message": map[string]any{
				"content": "Is #498 still running?",
				"author":  map[string]any{"username": "bob"},
			},
		}})
		// Keep open until client disconnects.
		wsutil.ReadClientData(conn)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- RunGateway(ctx, GatewayConfig{
			Token:      "secret-tok",
			GatewayURL: "ws" + strings.TrimPrefix(srv.URL, "http"),
			OnStatus: func(connected bool, botName, problem string) {
				if connected {
					select {
					case ready <- botName:
					default:
					}
				}
			},
			OnMention: func(ev MentionEvent) { mentions <- ev },
		})
	}()

	select {
	case name := <-ready:
		if name != "flare-autobot" {
			t.Fatalf("botName = %q", name)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for READY")
	}

	select {
	case ev := <-mentions:
		if ev.MessageID != "m_ok" || ev.CleanContent != "status of PR #498?" || ev.Author != "Alice" || ev.ReplyToText != "Is #498 still running?" {
			t.Fatalf("unexpected mention: %+v", ev)
		}
		if ev.URL != "https://discord.com/channels/g1/c1/m_ok" {
			t.Fatalf("URL = %q", ev.URL)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for mention")
	}
	cancel()
	<-done
	if len(mentions) != 0 {
		t.Fatalf("got unexpected extra mentions: %d", len(mentions))
	}
	if gotToken != "secret-tok" || gotIntents != gatewayIntents {
		t.Fatalf("identify token=%q intents=%d want %d", gotToken, gotIntents, gatewayIntents)
	}
}

func TestRunGatewayFatalClose4014(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, _, err := ws.UpgradeHTTP(r, w)
		if err != nil {
			return
		}
		defer conn.Close()
		b, _ := json.Marshal(map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 60000}})
		wsutil.WriteServerText(conn, b)
		wsutil.ReadClientData(conn) // Identify
		ws.WriteFrame(conn, ws.NewCloseFrame(ws.NewCloseFrameBody(4014, "Disallowed intents")))
	}))
	defer srv.Close()

	var lastProblem string
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := RunGateway(ctx, GatewayConfig{
		Token:      "tok",
		GatewayURL: "ws" + strings.TrimPrefix(srv.URL, "http"),
		OnStatus:   func(_ bool, _, problem string) { lastProblem = problem },
	})
	var fatal *GatewayFatalError
	if !errors.As(err, &fatal) || fatal.Code != 4014 || !strings.Contains(lastProblem, "Message Content Intent") {
		t.Fatalf("err=%v problem=%q", err, lastProblem)
	}
}
