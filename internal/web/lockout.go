package web

import (
	"strings"
	"sync"
	"time"
)

// An account that fails to sign in maxFails times within lockFor refuses sign-ins until lockFor has
// passed since the earliest of those failures, wherever they came from: nginx limits each address,
// this limits each account, so guessing from many addresses gets no further (Derek, 2026-11-23).
// It counts any name typed, so it says nothing about which accounts exist. The counts are in
// memory: a restart clears them.
const (
	maxFails = 10
	lockFor  = 15 * time.Minute
)

type loginFails struct {
	mu sync.Mutex
	at map[string][]time.Time
}

func (f *loginFails) recent(name string, now time.Time) []time.Time {
	var keep []time.Time
	for _, t := range f.at[name] {
		if now.Sub(t) < lockFor {
			keep = append(keep, t)
		}
	}
	if keep == nil {
		delete(f.at, name)
	} else {
		f.at[name] = keep
	}
	return keep
}

// locked reports how long the account must still wait, or 0.
func (f *loginFails) locked(name string) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.at == nil {
		return 0
	}
	now := time.Now()
	if fs := f.recent(strings.ToLower(name), now); len(fs) >= maxFails {
		return lockFor - now.Sub(fs[len(fs)-maxFails])
	}
	return 0
}

// failed records a failure and reports whether it just locked the account.
func (f *loginFails) failed(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.at == nil {
		f.at = map[string][]time.Time{}
	}
	name = strings.ToLower(name)
	now := time.Now()
	fs := append(f.recent(name, now), now)
	f.at[name] = fs
	return len(fs) == maxFails
}

func (f *loginFails) clear(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.at, strings.ToLower(name))
}
