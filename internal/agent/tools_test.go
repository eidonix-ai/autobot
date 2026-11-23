package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eidonix-ai/autobot/internal/netguard"
)

// fetch_url reaches only public addresses: not this machine, the LAN, or a redirect to either.
func TestFetchRefusesPrivateAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "secret") }))
	defer srv.Close()
	for _, u := range []string{srv.URL, "http://localhost:" + strings.TrimPrefix(srv.URL, "http://127.0.0.1:"),
		"http://192.168.5.123:8080/", "http://10.0.0.1/", "http://100.64.0.1/", "http://169.254.169.254/latest/meta-data/", "http://[::1]/", "file:///etc/passwd"} {
		text, err := Fetch(context.Background(), u)
		if err == nil || strings.Contains(text, "secret") {
			t.Errorf("%s was fetched: %q", u, text)
		}
	}
	for _, s := range []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.0.1", "100.100.0.1", "169.254.1.1", "0.0.0.0", "::1", "fd00::1", "fe80::1"} {
		if netguard.PublicOnly("tcp", s+":80", nil) == nil && netguard.PublicOnly("tcp", "["+s+"]:80", nil) == nil {
			t.Errorf("%s counted as public", s)
		}
	}
	for _, s := range []string{"1.1.1.1:443", "[2606:4700:4700::1111]:443"} {
		if err := netguard.PublicOnly("tcp", s, nil); err != nil {
			t.Errorf("%s refused: %v", s, err)
		}
	}
}

func TestNumberedAndMatch(t *testing.T) {
	if got := numbered("a\nb\nc", 2, 2); !strings.Contains(got, "     2  b") || strings.Contains(got, "  a") {
		t.Fatalf("%q", got)
	}
	if !match("*_test.go", "internal/x/a_test.go") || match("*_test.go", "a.go") || !match("internal/*", "internal/x/a.go") {
		t.Fatal("match")
	}
}
