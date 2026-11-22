package decisions

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var qs = []Question{
	{Name: "verdict", Type: "choice", Instructions: "Approve?", Options: []Option{{"approve", "safe"}, {"request-changes", "needs work"}}},
	{Name: "risky", Type: "yesno", Instructions: "Risky?"},
}

// Each protocol's request and answer must match its published shape.
func TestNative(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model, Input string
			Questions    []struct {
				Type, Name string
				Choices    []struct{ Value string }
			}
		}
		json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path != "/v1/decisions" || r.Header.Get("Authorization") != "Bearer k" || body.Model != "m" || body.Input != "diff" ||
			len(body.Questions) != 2 || body.Questions[0].Type != "choice" || body.Questions[1].Type != "predicate" || len(body.Questions[0].Choices) != 2 {
			t.Errorf("request %s %+v", r.URL.Path, body)
		}
		io.WriteString(w, `{"answers":[{"choice":"request-changes","probabilities":[{"value":"approve","probability":0.1},{"value":"request-changes","probability":0.9}]},{"probability":0.8}]}`)
	}))
	defer srv.Close()
	a, err := Ask(context.Background(), Native, srv.URL+"/v1", "k", "m", "diff", qs)
	if err != nil {
		t.Fatal(err)
	}
	if a[0].Choice != "request-changes" || a[0].Probabilities["approve"] != 0.1 || a[1].PTrue != 0.8 {
		t.Fatalf("answers %+v", a)
	}
}

func TestSystemOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model     string
			State     string
			Questions map[string]struct {
				Type     string
				Criteria json.RawMessage
			}
		}
		json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path != "/v1/systemone" || body.State != "diff" || body.Questions["verdict"].Type != "choice" ||
			body.Questions["risky"].Type != "noul" || body.Questions["severity"].Type != "score" ||
			!strings.Contains(string(body.Questions["verdict"].Criteria), `"approve":"safe"`) {
			t.Errorf("request %s %+v", r.URL.Path, body)
		}
		io.WriteString(w, `{"model":"m","answers":{
			"verdict":{"type":"choice","choice":"approve","probabilities":{"approve":0.7,"request-changes":0.3},"confidence":0.7},
			"risky":{"type":"noul","noul":0.2},
			"severity":{"type":"score","score":0.4,"probabilities":{"0":0.6,"1":0.4},"confidence":0.6}}}`)
	}))
	defer srv.Close()
	all := append(append([]Question{}, qs...), Question{Name: "severity", Type: "score", Instructions: "How bad?", Levels: []string{"none", "minor"}})
	a, err := Ask(context.Background(), SystemOne, srv.URL+"/v1", "k", "m", "diff", all)
	if err != nil {
		t.Fatal(err)
	}
	if a[0].Choice != "approve" || a[1].PTrue != 0.2 || a[2].Score != 0.4 || a[2].Probabilities["0"] != 0.6 {
		t.Fatalf("answers %+v", a)
	}
	// A /v1/decisions server cannot take a score question.
	if _, err := Ask(context.Background(), Native, srv.URL+"/v1", "k", "m", "diff", all); err == nil {
		t.Fatal("sent a score question to a /v1/decisions server")
	}
}
