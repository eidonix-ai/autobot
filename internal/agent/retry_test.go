package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

// flaky fails its first call with err, then answers.
type flaky struct {
	err   error
	calls int
}

func (f *flaky) Complete(ctx context.Context, msgs []Message, tools []ToolSpec) (Message, Usage, error) {
	if f.calls++; f.calls == 1 {
		return Message{}, Usage{}, f.err
	}
	return Message{Role: "assistant", Content: "ok"}, Usage{In: 1}, nil
}

// An answer cut off on the way (seen live: a sub-agent died on it) is asked for again, like a
// server error; a refusal is not.
func TestBrokenAnswerIsRetried(t *testing.T) {
	f := &flaky{err: errors.New("reading the answer from api.example.com: unexpected EOF")}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if reply, _, err := complete(ctx, f, nil, nil); err != nil || reply.Content != "ok" || f.calls != 2 {
		t.Fatalf("%v %q after %d calls", err, reply.Content, f.calls)
	}
	f = &flaky{err: errors.New("api.example.com answered 400 Bad Request: no such model")}
	if _, _, err := complete(ctx, f, nil, nil); err == nil || f.calls != 1 {
		t.Fatalf("a refusal was retried: %v after %d calls", err, f.calls)
	}
}

// flakyN fails its first n calls with err, then answers.
type flakyN struct {
	err   error
	n     int
	calls int
}

func (f *flakyN) Complete(ctx context.Context, msgs []Message, tools []ToolSpec) (Message, Usage, error) {
	if f.calls++; f.calls <= f.n {
		return Message{}, Usage{}, f.err
	}
	return Message{Role: "assistant", Content: "ok"}, Usage{In: 1}, nil
}

// A provider that drops connections for a while is ridden out: five failures in a row still end in an
// answer (seen live: two of five lenses lost to DeepSeek after the old four attempts).
func TestLongerRetries(t *testing.T) {
	saved := RetryWaits
	RetryWaits = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond}
	defer func() { RetryWaits = saved }()
	f := &flakyN{err: errors.New("could not reach api.example.com: unexpected EOF"), n: 5}
	if reply, _, err := complete(context.Background(), f, nil, nil); err != nil || reply.Content != "ok" || f.calls != 6 {
		t.Fatalf("%v %q after %d calls", err, reply.Content, f.calls)
	}
	var total time.Duration
	for _, w := range saved {
		total += w
	}
	if total < 2*time.Minute || total > 5*time.Minute {
		t.Fatalf("the waits add up to %s", total)
	}
}

func TestIsTransientMessage(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{`llm.eidonix.com answered 503 Service Unavailable: {"error":{"code":"server_is_overloaded"}}`, true},
		{"api.example.com answered 429 Too Many Requests: slow down", true},
		{"api.example.com answered 502 Bad Gateway: ", true},
		{"could not reach llm.eidonix.com: dial tcp: connection refused", true},
		{"reading the answer from api.deepseek.com: unexpected EOF", true},
		{"api.example.com answered 400 Bad Request: no such model", false},
		{"api.example.com answered 401 Unauthorized: bad key", false},
		{"stopped after 60 turns without finishing", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := IsTransientMessage(tc.msg); got != tc.want {
			t.Errorf("IsTransientMessage(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}
