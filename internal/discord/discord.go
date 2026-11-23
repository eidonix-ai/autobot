// Package discord posts to Discord channels through webhooks. A message never mentions anyone:
// what a workflow posts may come from a model, and must not be able to ping @everyone or a role.
package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxContent = 2000 // Discord's limit for a message
	preview    = 1500 // what a long message shows before its attachment
)

// client does not follow redirects, so a webhook's address is never handed to another site.
var client = &http.Client{
	Timeout:       30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// Check asks Discord about a webhook, without posting, and returns the name Discord shows for it.
func Check(ctx context.Context, webhook string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, webhook, nil)
	if err != nil {
		return "", errors.New("the webhook address is not usable")
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", reach(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnauthorized {
		return "", errors.New("Discord does not know this webhook: it may have been deleted, or the address was copied wrong")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Discord answered %s", resp.Status)
	}
	var w struct{ Name string }
	json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&w)
	return w.Name, nil
}

// Post sends text to the webhook's channel as Autobot. Text longer than a Discord message is sent
// as its beginning, with the whole text attached as a Markdown file.
func Post(ctx context.Context, webhook, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("the message is empty")
	}
	payload := map[string]any{"username": "Autobot", "allowed_mentions": map[string]any{"parse": []string{}}}
	var body bytes.Buffer
	contentType := "application/json"
	if len(text) <= maxContent && utf8.RuneCountInString(text) <= maxContent {
		payload["content"] = text
		json.NewEncoder(&body).Encode(payload)
	} else {
		cut := []rune(text)[:preview]
		payload["content"] = strings.TrimSpace(string(cut)) + "\n\n… the full text is attached."
		mw := multipart.NewWriter(&body)
		pj, _ := json.Marshal(payload)
		mw.WriteField("payload_json", string(pj))
		fw, _ := mw.CreateFormFile("files[0]", "autobot.md")
		io.WriteString(fw, text)
		mw.Close()
		contentType = mw.FormDataContentType()
	}
	for try := 0; ; try++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhook+"?wait=true", bytes.NewReader(body.Bytes()))
		if err != nil {
			return errors.New("the webhook address is not usable")
		}
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("User-Agent", "autobot")
		resp, err := client.Do(req)
		if err != nil {
			return reach(err)
		}
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent:
			return nil
		case resp.StatusCode == http.StatusTooManyRequests && try < 2:
			wait, _ := strconv.ParseFloat(resp.Header.Get("Retry-After"), 64)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(min(max(wait, 1), 30) * float64(time.Second))):
			}
		case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnauthorized:
			return errors.New("Discord does not know this webhook any more: it may have been deleted")
		default:
			return fmt.Errorf("Discord answered %s: %s", resp.Status, strings.TrimSpace(string(msg)))
		}
	}
}

// reach describes a failed connection without echoing the address, which holds the webhook's secret.
func reach(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return fmt.Errorf("could not reach Discord: %v", err)
}

// UseTransport sends every request through rt instead of the network. For tests.
func UseTransport(rt http.RoundTripper) { client.Transport = rt }
