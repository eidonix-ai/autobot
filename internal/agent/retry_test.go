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
	saved := retryWaits
	retryWaits = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond}
	defer func() { retryWaits = saved }()
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
