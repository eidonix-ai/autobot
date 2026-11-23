package netguard

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestPublicOnly(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.5.123", "100.100.1.1", "169.254.169.254", "0.1.2.3",
		"192.0.0.8", "198.18.0.1", "240.0.0.1", "[::1]", "[fd00::1]", "[fe80::1]", "[fec0::1]", "[64:ff9b::c0a8:101]", "[2002:c0a8:101::]", "[::192.168.1.1]"} {
		if PublicOnly("tcp", s+":443", nil) == nil {
			t.Errorf("%s counted as public", s)
		}
	}
	for _, s := range []string{"1.1.1.1:443", "[2606:4700:4700::1111]:443"} {
		if err := PublicOnly("tcp", s, nil); err != nil {
			t.Errorf("%s refused: %v", s, err)
		}
	}
	own.Lock()
	if own.addrs == nil {
		own.names, own.addrs = map[string]bool{}, map[netip.Addr]bool{}
	}
	own.addrs[netip.MustParseAddr("203.0.113.9")] = true
	own.Unlock()
	if PublicOnly("tcp", "203.0.113.9:443", nil) == nil {
		t.Error("Autobot's own public address was allowed")
	}
}

// A request to the local network is refused, unless an admin trusts the host.
func TestTrustedHosts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "inside") }))
	defer srv.Close()
	c := &http.Client{Transport: Transport()}
	get := func() error {
		req, _ := http.NewRequestWithContext(context.Background(), "GET", srv.URL, nil)
		resp, err := c.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	SetTrusted(nil)
	if get() == nil {
		t.Fatal("reached a local address")
	}
	SetTrusted([]string{srv.Listener.Addr().String()})
	if err := get(); err != nil {
		t.Fatalf("a trusted host was refused: %v", err)
	}
	SetTrusted(nil)
}
