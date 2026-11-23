// Package decisions asks a model structured questions and gets back probabilities, over either of
// two protocols:
//
//	/v1/decisions  a native decisions route on an OpenAI-style server: choice and yes/no questions,
//	               answered in order (the protocol the earlier harness spoke)
//	/v1/systemone  System One (https://docs.system-one.dev/en/docs/api): choice, score and yes/no
//	               ("noul") questions, answered by name
//
// The endpoint is the base URL including /v1, as for any provider.
package decisions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/eidonix-ai/autobot/internal/netguard"
)

// The two protocols, as provider kinds.
const (
	Native    = "decisions"
	SystemOne = "systemone"
)

// Question is one question about the input.
type Question struct {
	Name         string   // unique in the request: letters, digits, '.', '_' or '-'
	Type         string   // "choice", "yesno" or "score" (score: System One only)
	Instructions string   // what to decide
	Options      []Option // choice: at least two
	Levels       []string // score: two to ten, lowest first
}

// Option is one answer a choice question offers.
type Option struct{ Name, Description string }

// Answer is one question's result.
type Answer struct {
	Name          string
	Choice        string             // choice: the likeliest option
	Score         float64            // score: the expected level, from 0
	PTrue         float64            // yesno: probability of yes
	Probabilities map[string]float64 // choice: per option; score: per level ("0", "1", …)
}

var client = &http.Client{
	// Only public addresses, or hosts an admin trusts: the address comes from a person.
	Transport: netguard.Transport(), Timeout: 60 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// Ask asks the questions about input with model, over the protocol kind names.
func Ask(ctx context.Context, kind, endpoint, key, model, input string, qs []Question) ([]Answer, error) {
	if len(qs) == 0 || len(qs) > 32 {
		return nil, errors.New("ask 1 to 32 questions")
	}
	switch kind {
	case Native:
		return askNative(ctx, endpoint, key, model, input, qs)
	case SystemOne:
		return askSystemOne(ctx, endpoint, key, model, input, qs)
	}
	return nil, fmt.Errorf("%q is not a decisions protocol", kind)
}

func post(ctx context.Context, url, key string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return errors.New("the endpoint is not a usable web address")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "autobot")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach %s", req.URL.Host)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		detail := resp.Header.Get("X-System-One-Error-Code")
		if detail == "" {
			detail = strings.TrimSpace(string(raw[:min(len(raw), 300)]))
		}
		if key != "" {
			detail = strings.ReplaceAll(detail, key, "[key]")
		}
		return fmt.Errorf("%s answered %s: %s", req.URL.Host, resp.Status, detail)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s sent an answer that is not the expected JSON", req.URL.Host)
	}
	return nil
}

func askNative(ctx context.Context, endpoint, key, model, input string, qs []Question) ([]Answer, error) {
	type choice struct {
		Value       string `json:"value"`
		Description string `json:"description,omitempty"`
	}
	type question struct {
		Type         string   `json:"type"`
		Name         string   `json:"name"`
		Instructions string   `json:"instructions"`
		Choices      []choice `json:"choices,omitempty"`
	}
	var body struct {
		Model     string     `json:"model"`
		Input     string     `json:"input"`
		Questions []question `json:"questions"`
	}
	body.Model, body.Input = model, input
	for _, q := range qs {
		nq := question{Name: q.Name, Instructions: q.Instructions}
		switch q.Type {
		case "choice":
			nq.Type = "choice"
			for _, o := range q.Options {
				nq.Choices = append(nq.Choices, choice{o.Name, o.Description})
			}
		case "yesno":
			nq.Type = "predicate"
		default:
			return nil, fmt.Errorf("question %s: a /v1/decisions server answers choice and yes/no questions only", q.Name)
		}
		body.Questions = append(body.Questions, nq)
	}
	var resp struct {
		Answers []struct {
			Probability   *float64 `json:"probability"`
			Choice        *string  `json:"choice"`
			Probabilities []struct {
				Value       string  `json:"value"`
				Probability float64 `json:"probability"`
			} `json:"probabilities"`
		} `json:"answers"`
	}
	if err := post(ctx, strings.TrimRight(endpoint, "/")+"/decisions", key, body, &resp); err != nil {
		return nil, err
	}
	if len(resp.Answers) != len(qs) {
		return nil, fmt.Errorf("the server answered %d of %d questions", len(resp.Answers), len(qs))
	}
	out := make([]Answer, len(qs))
	for i, q := range qs {
		a, ans := resp.Answers[i], Answer{Name: q.Name}
		if q.Type == "yesno" {
			if a.Probability == nil || *a.Probability < 0 || *a.Probability > 1 {
				return nil, fmt.Errorf("question %s: no probability in the answer", q.Name)
			}
			ans.PTrue = *a.Probability
		} else {
			ans.Probabilities = map[string]float64{}
			for _, p := range a.Probabilities {
				ans.Probabilities[p.Value] = p.Probability
			}
			if a.Choice == nil {
				return nil, fmt.Errorf("question %s: no choice in the answer", q.Name)
			}
			ans.Choice = *a.Choice
			if err := checkChoice(q, ans); err != nil {
				return nil, err
			}
		}
		out[i] = ans
	}
	return out, nil
}

func askSystemOne(ctx context.Context, endpoint, key, model, input string, qs []Question) ([]Answer, error) {
	type question struct {
		Type         string `json:"type"`
		Instructions string `json:"instructions"`
		Criteria     any    `json:"criteria,omitempty"`
	}
	questions := map[string]question{}
	for _, q := range qs {
		sq := question{Instructions: q.Instructions}
		switch q.Type {
		case "choice":
			criteria := map[string]string{}
			for _, o := range q.Options {
				criteria[o.Name] = o.Description
			}
			sq.Type, sq.Criteria = "choice", criteria
		case "score":
			sq.Type, sq.Criteria = "score", q.Levels
		case "yesno":
			sq.Type = "noul"
		default:
			return nil, fmt.Errorf("question %s: type %q is not choice, score or yesno", q.Name, q.Type)
		}
		if _, dup := questions[q.Name]; dup {
			return nil, fmt.Errorf("two questions are named %s", q.Name)
		}
		questions[q.Name] = sq
	}
	body := map[string]any{"model": model, "state": input, "questions": questions}
	var resp struct {
		Answers map[string]struct {
			Choice        string             `json:"choice"`
			Score         *float64           `json:"score"`
			Noul          *float64           `json:"noul"`
			Probabilities map[string]float64 `json:"probabilities"`
		} `json:"answers"`
	}
	if err := post(ctx, strings.TrimRight(endpoint, "/")+"/systemone", key, body, &resp); err != nil {
		return nil, err
	}
	out := make([]Answer, len(qs))
	for i, q := range qs {
		a, ok := resp.Answers[q.Name]
		if !ok {
			return nil, fmt.Errorf("question %s: not answered", q.Name)
		}
		ans := Answer{Name: q.Name, Choice: a.Choice, Probabilities: a.Probabilities}
		switch q.Type {
		case "yesno":
			if a.Noul == nil || *a.Noul < 0 || *a.Noul > 1 {
				return nil, fmt.Errorf("question %s: no probability in the answer", q.Name)
			}
			ans.PTrue = *a.Noul
		case "score":
			if a.Score == nil {
				return nil, fmt.Errorf("question %s: no score in the answer", q.Name)
			}
			ans.Score = *a.Score
			for level := range ans.Probabilities {
				if n, err := strconv.Atoi(level); err != nil || n < 0 || n >= len(q.Levels) {
					return nil, fmt.Errorf("question %s: level %q was not offered", q.Name, level)
				}
			}
		case "choice":
			if err := checkChoice(q, ans); err != nil {
				return nil, err
			}
		}
		out[i] = ans
	}
	return out, nil
}

// checkChoice confirms an answer chose an offered option and gave each option a probability.
func checkChoice(q Question, a Answer) error {
	offered := false
	for _, o := range q.Options {
		if _, ok := a.Probabilities[o.Name]; !ok {
			return fmt.Errorf("question %s: no probability for %q", q.Name, o.Name)
		}
		offered = offered || o.Name == a.Choice
	}
	if !offered {
		return fmt.Errorf("question %s: chose %q, which was not offered", q.Name, a.Choice)
	}
	return nil
}
