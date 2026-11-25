package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// Gateway intents required to hear @mentions in guild channels:
// GUILDS (1<<0) | GUILD_MESSAGES (1<<9) | MESSAGE_CONTENT (1<<15).
const gatewayIntents = (1 << 0) | (1 << 9) | (1 << 15)

// MentionEvent is a guild message mentioning the bot, delivered by RunGateway.
type MentionEvent struct {
	GuildID       string
	ChannelID     string
	MessageID     string
	AuthorID      string
	Author        string // display name (GlobalName or Username)
	AuthorHandle  string // Discord username
	Content       string // raw message text
	CleanContent  string // text with the bot's own <@id> mention removed
	ReplyToAuthor string // author of the replied-to message, if any
	ReplyToText   string // text of the replied-to message, if any
	URL           string // jump URL to the message
	BotID         string
	BotUsername   string
}

// GatewayConfig configures a persistent Discord Gateway listener for one bot.
type GatewayConfig struct {
	Token      string
	GatewayURL string // optional override for tests; when empty, Bot.GatewayURL is called
	OnMention  func(MentionEvent)
	OnStatus   func(connected bool, botName, problem string)
	OnLog      func(msg string)
}

// GatewayFatalError is returned when Discord closes the connection with a non-retryable code
// (such as invalid token or disallowed privileged intent).
type GatewayFatalError struct {
	Code int
	Msg  string
}

func (e *GatewayFatalError) Error() string { return e.Msg }

func fatalCloseError(code ws.StatusCode) error {
	switch int(code) {
	case 4004:
		return &GatewayFatalError{4004, "Discord rejected the bot token"}
	case 4010, 4011, 4012:
		return &GatewayFatalError{int(code), fmt.Sprintf("Discord Gateway closed with code %d", code)}
	case 4013:
		return &GatewayFatalError{4013, "Discord rejected the Gateway intents"}
	case 4014:
		return &GatewayFatalError{4014, "Enable Message Content Intent in the Discord Developer Portal (Bot → Privileged Gateway Intents)"}
	}
	return nil
}

type gatewayPayload struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d,omitempty"`
	S  *int64          `json:"s,omitempty"`
	T  string          `json:"t,omitempty"`
}

type gatewaySession struct {
	sessionID   string
	resumeURL   string
	botID       string
	botUsername string
	botRoles    map[string]bool // managed role IDs owned by this bot (tags.bot_id == botID)
	seq         atomic.Int64
}

// RunGateway maintains a Discord Gateway v10 WebSocket connection until ctx is cancelled or Discord
// returns a fatal close code (bad token, disallowed intents). Transient disconnects reconnect with
// resume and exponential backoff.
func RunGateway(ctx context.Context, cfg GatewayConfig) error {
	sess := &gatewaySession{}
	backoff := 2 * time.Second
	const maxBackoff = 60 * time.Second
	report := func(connected bool, botName, problem string) {
		if cfg.OnStatus != nil {
			cfg.OnStatus(connected, botName, problem)
		}
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		url := cfg.GatewayURL
		if sess.resumeURL != "" && sess.sessionID != "" && url == "" {
			url = sess.resumeURL
		}
		if url == "" {
			u, err := (Bot{Token: cfg.Token}).GatewayURL(ctx)
			if err != nil {
				report(false, sess.botUsername, err.Error())
				if !sleepCtx(ctx, backoff) {
					return ctx.Err()
				}
				backoff = min(backoff*2, maxBackoff)
				continue
			}
			url = u
		}
		started := time.Now()
		err := runGatewayOnce(ctx, url, cfg, sess, report)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var fatal *GatewayFatalError
		if errors.As(err, &fatal) {
			report(false, sess.botUsername, fatal.Msg)
			if cfg.OnLog != nil {
				cfg.OnLog(fmt.Sprintf("fatal close %d: %s", fatal.Code, fatal.Msg))
			}
			return fatal
		}
		if time.Since(started) > 30*time.Second {
			backoff = 2 * time.Second
		}
		msg := "disconnected"
		if err != nil {
			msg = err.Error()
		}
		if cfg.OnLog != nil {
			cfg.OnLog("disconnected (" + msg + "); reconnecting in " + backoff.String())
		}
		report(false, sess.botUsername, "Reconnecting to Discord: "+msg)
		if !sleepCtx(ctx, backoff) {
			return ctx.Err()
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func runGatewayOnce(ctx context.Context, url string, cfg GatewayConfig, sess *gatewaySession, report func(bool, string, string)) error {
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	conn, br, _, err := ws.Dial(dctx, url)
	cancel()
	if err != nil {
		return err
	}
	connCtx, stopConn := context.WithCancel(ctx)
	defer stopConn()
	defer conn.Close()
	go func() {
		<-connCtx.Done()
		conn.Close()
	}()

	var wmu sync.Mutex
	sendJSON := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		wmu.Lock()
		defer wmu.Unlock()
		conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
		return wsutil.WriteClientText(conn, b)
	}
	sendHeartbeat := func() error {
		var d any
		if s := sess.seq.Load(); s > 0 {
			d = s
		}
		return sendJSON(map[string]any{"op": 1, "d": d})
	}

	// Read frames using the buffered reader returned by ws.Dial when present.
	readFrame := func() ([]byte, error) {
		var src net.Conn = conn
		if br != nil && br.Buffered() > 0 {
			// Wrap br + conn in an io.ReadWriter while buffered bytes remain.
			return readClientData(&bufferedConn{br: br, Conn: conn})
		}
		return readClientData(src)
	}

	// First frame must be Op 10 (Hello).
	raw, err := readFrame()
	if err != nil {
		return err
	}
	var hello gatewayPayload
	if err := json.Unmarshal(raw, &hello); err != nil || hello.Op != 10 {
		return errors.New("expected Hello from Discord Gateway")
	}
	var hd struct {
		HeartbeatInterval int `json:"heartbeat_interval"`
	}
	if err := json.Unmarshal(hello.D, &hd); err != nil || hd.HeartbeatInterval <= 0 {
		return errors.New("invalid heartbeat_interval from Discord Gateway")
	}
	interval := time.Duration(hd.HeartbeatInterval) * time.Millisecond

	// Start heartbeat loop with ACK tracking.
	var acked atomic.Bool
	acked.Store(true)
	hbErr := make(chan error, 1)
	go func() {
		jitter := time.Duration(float64(interval) * (0.1 + 0.8*rand.Float64()))
		if !sleepCtx(connCtx, jitter) {
			return
		}
		for {
			if !acked.Swap(false) {
				hbErr <- errors.New("heartbeat timed out")
				stopConn()
				return
			}
			if err := sendHeartbeat(); err != nil {
				hbErr <- err
				stopConn()
				return
			}
			if !sleepCtx(connCtx, interval) {
				return
			}
		}
	}()

	// Identify or Resume.
	if sess.sessionID != "" && sess.seq.Load() > 0 {
		if err := sendJSON(map[string]any{"op": 6, "d": map[string]any{
			"token": cfg.Token, "session_id": sess.sessionID, "seq": sess.seq.Load(),
		}}); err != nil {
			return err
		}
	} else {
		if err := sendJSON(map[string]any{"op": 2, "d": map[string]any{
			"token":   cfg.Token,
			"intents": gatewayIntents,
			"properties": map[string]string{
				"os": "linux", "browser": "autobot", "device": "autobot",
			},
		}}); err != nil {
			return err
		}
	}

	for {
		data, err := readFrame()
		if err != nil {
			select {
			case herr := <-hbErr:
				return herr
			default:
			}
			return err
		}
		var p gatewayPayload
		if err := json.Unmarshal(data, &p); err != nil {
			continue
		}
		if p.S != nil && *p.S > 0 {
			sess.seq.Store(*p.S)
		}
		switch p.Op {
		case 1: // Heartbeat request
			if err := sendHeartbeat(); err != nil {
				return err
			}
		case 11: // Heartbeat ACK
			acked.Store(true)
		case 7: // Reconnect
			return errors.New("Discord requested reconnect")
		case 9: // Invalid Session
			var resumable bool
			json.Unmarshal(p.D, &resumable)
			if !resumable {
				sess.sessionID, sess.resumeURL = "", ""
				sess.seq.Store(0)
			}
			return errors.New("invalid session")
		case 0: // Dispatch
			switch p.T {
			case "READY":
				var r struct {
					SessionID        string `json:"session_id"`
					ResumeGatewayURL string `json:"resume_gateway_url"`
					User             struct {
						ID       string `json:"id"`
						Username string `json:"username"`
					} `json:"user"`
				}
				if json.Unmarshal(p.D, &r) == nil {
					sess.sessionID = r.SessionID
					if r.ResumeGatewayURL != "" {
						sess.resumeURL = strings.TrimRight(r.ResumeGatewayURL, "/") + "/?v=10&encoding=json"
					}
					sess.botID, sess.botUsername = r.User.ID, r.User.Username
					if sess.botRoles == nil {
						sess.botRoles = map[string]bool{}
					}
					report(true, sess.botUsername, "")
					if cfg.OnLog != nil {
						cfg.OnLog(fmt.Sprintf("READY as %s (id=%s)", sess.botUsername, sess.botID))
					}
				}
			case "RESUMED":
				report(true, sess.botUsername, "")
				if cfg.OnLog != nil {
					cfg.OnLog(fmt.Sprintf("RESUMED as %s", sess.botUsername))
				}
			case "GUILD_CREATE":
				var g struct {
					ID    string `json:"id"`
					Name  string `json:"name"`
					Roles []struct {
						ID   string `json:"id"`
						Name string `json:"name"`
						Tags *struct {
							BotID string `json:"bot_id"`
						} `json:"tags"`
					} `json:"roles"`
				}
				if json.Unmarshal(p.D, &g) == nil && sess.botID != "" {
					if sess.botRoles == nil {
						sess.botRoles = map[string]bool{}
					}
					var added []string
					for _, role := range g.Roles {
						if role.Tags != nil && role.Tags.BotID == sess.botID {
							sess.botRoles[role.ID] = true
							added = append(added, role.ID)
						}
					}
					if cfg.OnLog != nil {
						cfg.OnLog(fmt.Sprintf("GUILD_CREATE %q (id=%s, bot_roles=%v)", g.Name, g.ID, added))
					}
				}
			case "MESSAGE_CREATE":
				if ev, ok := parseMention(p.D, sess.botID, sess.botUsername, sess.botRoles); ok && cfg.OnMention != nil {
					cfg.OnMention(ev)
				} else if cfg.OnLog != nil {
					if note := missedMentionNote(p.D, sess.botID); note != "" {
						cfg.OnLog(note)
					}
				}
			}
		}
	}
}

type bufferedConn struct {
	br interface{ Read([]byte) (int, error) }
	net.Conn
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.br.Read(p) }

// readClientData reads the next text/binary data frame from a WebSocket connection, checking
// Close frames for fatal Discord close codes.
func readClientData(rw interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
}) ([]byte, error) {
	data, _, err := wsutil.ReadServerData(rw)
	if err != nil {
		var cerr wsutil.ClosedError
		if errors.As(err, &cerr) {
			if ferr := fatalCloseError(cerr.Code); ferr != nil {
				if cerr.Reason != "" {
					ferr.(*GatewayFatalError).Msg += " (Discord: " + cerr.Reason + ")"
				}
				return nil, ferr
			}
			return nil, fmt.Errorf("close %d %s", cerr.Code, cerr.Reason)
		}
		return nil, err
	}
	return data, nil
}

// parseMention checks whether a MESSAGE_CREATE payload is a guild message from a human mentioning
// the bot (either its user ID or its managed bot role), returning the parsed MentionEvent.
func parseMention(raw json.RawMessage, botID, botUsername string, botRoles map[string]bool) (MentionEvent, bool) {
	if botID == "" {
		return MentionEvent{}, false
	}
	var m struct {
		ID        string `json:"id"`
		ChannelID string `json:"channel_id"`
		GuildID   string `json:"guild_id"`
		Content   string `json:"content"`
		Author    struct {
			ID         string `json:"id"`
			Username   string `json:"username"`
			GlobalName string `json:"global_name"`
			Bot        bool   `json:"bot"`
		} `json:"author"`
		Mentions []struct {
			ID string `json:"id"`
		} `json:"mentions"`
		MentionRoles []string `json:"mention_roles"`
		Referenced   *struct {
			Content string `json:"content"`
			Author  struct {
				Username   string `json:"username"`
				GlobalName string `json:"global_name"`
			} `json:"author"`
		} `json:"referenced_message"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return MentionEvent{}, false
	}
	// Never respond to bots (including ourselves) or DMs.
	if m.Author.Bot || m.Author.ID == botID || m.GuildID == "" || m.ChannelID == "" || m.ID == "" {
		return MentionEvent{}, false
	}
	mentioned := false
	for _, u := range m.Mentions {
		if u.ID == botID {
			mentioned = true
			break
		}
	}
	var strip []string
	tag1, tag2 := "<@"+botID+">", "<@!"+botID+">"
	strip = append(strip, tag1, "", tag2, "")
	if strings.Contains(m.Content, tag1) || strings.Contains(m.Content, tag2) {
		mentioned = true
	}
	for _, rid := range m.MentionRoles {
		if botRoles[rid] {
			mentioned = true
		}
	}
	for rid := range botRoles {
		rtag := "<@&" + rid + ">"
		strip = append(strip, rtag, "")
		if strings.Contains(m.Content, rtag) {
			mentioned = true
		}
	}
	if !mentioned {
		return MentionEvent{}, false
	}
	clean := strings.TrimSpace(strings.NewReplacer(strip...).Replace(m.Content))
	author := m.Author.GlobalName
	if author == "" {
		author = m.Author.Username
	}
	ev := MentionEvent{
		GuildID:      m.GuildID,
		ChannelID:    m.ChannelID,
		MessageID:    m.ID,
		AuthorID:     m.Author.ID,
		Author:       author,
		AuthorHandle: m.Author.Username,
		Content:      m.Content,
		CleanContent: clean,
		URL:          fmt.Sprintf("https://discord.com/channels/%s/%s/%s", m.GuildID, m.ChannelID, m.ID),
		BotID:        botID,
		BotUsername:  botUsername,
	}
	if m.Referenced != nil {
		ra := m.Referenced.Author.GlobalName
		if ra == "" {
			ra = m.Referenced.Author.Username
		}
		ev.ReplyToAuthor = ra
		ev.ReplyToText = strings.TrimSpace(m.Referenced.Content)
	}
	return ev, true
}

// missedMentionNote logs IDs only (no message text) when a non-bot guild message has user/role
// mentions that didn't match the bot, to make permission/role debugging immediate.
func missedMentionNote(raw json.RawMessage, botID string) string {
	var m struct {
		ChannelID string `json:"channel_id"`
		GuildID   string `json:"guild_id"`
		Author    struct {
			ID  string `json:"id"`
			Bot bool   `json:"bot"`
		} `json:"author"`
		Mentions     []struct{ ID string `json:"id"` } `json:"mentions"`
		MentionRoles []string                           `json:"mention_roles"`
	}
	if json.Unmarshal(raw, &m) != nil || m.Author.Bot || m.Author.ID == botID || m.GuildID == "" {
		return ""
	}
	if len(m.Mentions) == 0 && len(m.MentionRoles) == 0 {
		return ""
	}
	var uids []string
	for _, u := range m.Mentions {
		uids = append(uids, u.ID)
	}
	return fmt.Sprintf("MESSAGE_CREATE ch=%s had other mentions (users=%v roles=%v)", m.ChannelID, uids, m.MentionRoles)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
