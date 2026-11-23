// Package discord is Autobot's bot on Discord, over Discord's HTTP API: it reads which channels it
// can see, opens a forum post per case, posts and edits messages, and checks the signature on the
// interactions (button clicks) Discord sends to Autobot. Every message it sends mentions no one
// unless a caller names a role, and shows no link previews: what it posts may come from a model,
// and must neither ping people nor make Discord fetch an address a model chose.
package discord

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// API is Discord's HTTP API; tests point it elsewhere.
var API = "https://discord.com/api/v10"

const (
	maxContent     = 2000 // Discord's limit for a message
	preview        = 1500 // what a long message shows before its attachment
	suppressEmbeds = 1 << 2
	typeForum      = 15
	typeText       = 0
)

var client = &http.Client{Timeout: 30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// Bot is the bot, by its token.
type Bot struct{ Token string }

// Error is an answer Discord gave other than success.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("Discord answered %d: %s", e.Status, e.Message) }

// call sends a request as the bot and decodes the answer into out. A rate limit is waited out,
// twice at most.
func (b Bot) call(ctx context.Context, method, path string, body any, out any) error {
	var payload []byte
	contentType := ""
	switch v := body.(type) {
	case nil:
	case *multipartBody:
		payload, contentType = v.data, v.contentType
	default:
		payload, _ = json.Marshal(v)
		contentType = "application/json"
	}
	for try := 0; ; try++ {
		req, err := http.NewRequestWithContext(ctx, method, API+path, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bot "+b.Token)
		req.Header.Set("User-Agent", "DiscordBot (https://github.com/eidonix-ai/autobot, 1)")
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("could not reach Discord: %v", unwrap(err))
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests && try < 2 {
			wait, _ := strconv.ParseFloat(resp.Header.Get("Retry-After"), 64)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(min(max(wait, 1), 30) * float64(time.Second))):
			}
			continue
		}
		if resp.StatusCode/100 != 2 {
			var e struct{ Message string }
			json.Unmarshal(data, &e)
			if e.Message == "" {
				e.Message = http.StatusText(resp.StatusCode)
			}
			return &Error{resp.StatusCode, e.Message}
		}
		if out != nil && len(data) > 0 {
			return json.Unmarshal(data, out)
		}
		return nil
	}
}

func unwrap(err error) error {
	for {
		u := errors.Unwrap(err)
		if u == nil {
			return err
		}
		err = u
	}
}

// Me is the bot's own account.
type Me struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

// Me checks the token and returns the bot's account.
func (b Bot) Me(ctx context.Context) (*Me, error) {
	var m Me
	if err := b.call(ctx, "GET", "/users/@me", nil, &m); err != nil {
		var de *Error
		if errors.As(err, &de) && de.Status == http.StatusUnauthorized {
			return nil, errors.New("Discord does not accept this bot token")
		}
		return nil, err
	}
	return &m, nil
}

// Channel is a channel the bot can see.
type Channel struct {
	ID, Name, Server string
	Forum            bool
}

// Forums lists the forum channels of the servers the bot is in, by server then name.
func (b Bot) Forums(ctx context.Context) ([]Channel, error) {
	var guilds []struct{ ID, Name string }
	if err := b.call(ctx, "GET", "/users/@me/guilds", nil, &guilds); err != nil {
		return nil, err
	}
	var out []Channel
	for _, g := range guilds {
		var chans []struct {
			ID, Name string
			Type     int
		}
		if err := b.call(ctx, "GET", "/guilds/"+g.ID+"/channels", nil, &chans); err != nil {
			return nil, err
		}
		for _, c := range chans {
			if c.Type == typeForum {
				out = append(out, Channel{ID: c.ID, Name: c.Name, Server: g.Name, Forum: true})
			}
		}
	}
	return out, nil
}

// Button is a button on a message; its ID comes back in the interaction when someone clicks it.
type Button struct {
	ID, Label string
	Style     int // 1 blue, 2 grey, 3 green, 4 red
	Disabled  bool
}

// Message is what the bot sends: text, perhaps buttons, perhaps a role to ping.
type Message struct {
	Text    string
	Buttons []Button
	Role    string // a role id this message may ping; "" pings no one
}

type multipartBody struct {
	data        []byte
	contentType string
}

// body builds a message's request: its text (a long one as its beginning, with the whole text
// attached), buttons, no previews, and mentions limited to the role given.
func (m Message) body(extra map[string]any) any {
	allowed := map[string]any{"parse": []string{}}
	if m.Role != "" {
		allowed["roles"] = []string{m.Role}
	}
	p := map[string]any{"allowed_mentions": allowed, "flags": suppressEmbeds}
	for k, v := range extra {
		p[k] = v
	}
	if len(m.Buttons) > 0 {
		var comps []map[string]any
		for _, b := range m.Buttons {
			comps = append(comps, map[string]any{"type": 2, "style": b.Style, "label": b.Label, "custom_id": b.ID, "disabled": b.Disabled})
		}
		p["components"] = []map[string]any{{"type": 1, "components": comps}}
	} else {
		p["components"] = []any{}
	}
	text := strings.TrimSpace(m.Text)
	if utf8.RuneCountInString(text) <= maxContent {
		setContent(p, text)
		return p
	}
	setContent(p, strings.TrimSpace(string([]rune(text)[:preview]))+"\n\n… the full text is attached.")
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	pj, _ := json.Marshal(p)
	mw.WriteField("payload_json", string(pj))
	fw, _ := mw.CreateFormFile("files[0]", "autobot.md")
	io.WriteString(fw, text)
	mw.Close()
	return &multipartBody{buf.Bytes(), mw.FormDataContentType()}
}

// setContent puts the text where it belongs: a forum post's first message is inside "message".
func setContent(p map[string]any, text string) {
	if msg, ok := p["message"].(map[string]any); ok {
		for _, k := range []string{"allowed_mentions", "flags", "components"} {
			msg[k] = p[k]
			delete(p, k)
		}
		msg["content"] = text
		return
	}
	p["content"] = text
}

// Posted is a message the bot sent: its channel (a forum post is a channel) and id.
type Posted struct {
	Channel string `json:"channel_id"`
	ID      string `json:"id"`
}

// CreatePost opens a forum post titled title, with m as its first message, and returns that message.
func (b Bot) CreatePost(ctx context.Context, forum, title string, m Message) (*Posted, error) {
	if utf8.RuneCountInString(title) > 100 {
		title = string([]rune(title)[:99]) + "…"
	}
	var thread struct {
		ID      string `json:"id"`
		Message struct {
			ID string `json:"id"`
		} `json:"message"`
	}
	body := m.body(map[string]any{"name": title, "message": map[string]any{}})
	if err := b.call(ctx, "POST", "/channels/"+forum+"/threads", body, &thread); err != nil {
		return nil, err
	}
	return &Posted{Channel: thread.ID, ID: thread.Message.ID}, nil
}

// Send posts m in a channel or forum post.
func (b Bot) Send(ctx context.Context, channel string, m Message) (*Posted, error) {
	var p Posted
	if err := b.call(ctx, "POST", "/channels/"+channel+"/messages", m.body(nil), &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// Edit replaces one of the bot's messages.
// An edit keeps to one message's length: a summary should.
func (b Bot) Edit(ctx context.Context, channel, message string, m Message) error {
	if t := []rune(strings.TrimSpace(m.Text)); len(t) > maxContent {
		m.Text = string(t[:maxContent-1]) + "…"
	}
	return b.call(ctx, "PATCH", "/channels/"+channel+"/messages/"+message, m.body(nil), nil)
}

// Verify checks that an interaction came from Discord: its Ed25519 signature over the timestamp
// and the body, with the application's public key (hex, from the Developer Portal).
func Verify(publicKey, signature, timestamp string, body []byte) bool {
	key, err := hex.DecodeString(publicKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return false
	}
	sig, err := hex.DecodeString(signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(key, append([]byte(timestamp), body...), sig)
}
