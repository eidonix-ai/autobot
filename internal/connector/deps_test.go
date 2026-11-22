package connector

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Dependencies come only from the allowed hosts, and only when they match the lockfile's checksum.
func TestDownloadIsAllowlistedAndVerified(t *testing.T) {
	body := []byte("crate bytes")
	sum := sha256.Sum256(body)
	good := hex.EncodeToString(sum[:])
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			// The same server under a name that is not allowed (its certificate covers example.com).
			http.Redirect(w, r, "https://example.com:"+strings.Split(r.Host, ":")[1]+"/x.crate", http.StatusFound)
			return
		}
		w.Write(body)
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "https://")
	allowedHosts[host] = true
	defer delete(allowedHosts, host)
	saved := depClient
	defer func() { depClient = saved }()
	depClient = srv.Client()
	depClient.CheckRedirect = saved.CheckRedirect
	addr := srv.Listener.Addr().String()
	depClient.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr) // every name reaches the test server
	}

	d := &deps{cache: t.TempDir()}
	ctx := context.Background()
	if _, err := d.download(ctx, "https://not-allowed.example/x.crate", good); err == nil {
		t.Fatal("downloaded from a host that is not allowed")
	}
	if _, err := d.download(ctx, srv.URL+"/elsewhere", good); err == nil {
		t.Fatal("followed a redirect to a host that is not allowed")
	}
	wrong := strings.Repeat("0", 64)
	if _, err := d.download(ctx, srv.URL+"/x.crate", wrong); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("a file that does not match its checksum: %v", err)
	}
	if _, err := os.Stat(filepath.Join(d.cache, "files", wrong[:2], wrong)); err == nil {
		t.Fatal("a mismatched file was cached")
	}
	file, err := d.download(ctx, srv.URL+"/x.crate", good)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(file); !bytes.Equal(b, body) {
		t.Fatalf("cached %q", b)
	}
}

// A crate archive can only write inside its own name-version folder.
func TestUnpackCrateStaysInside(t *testing.T) {
	mk := func(names ...string) string {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		for _, n := range names {
			tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: 0o644, Size: 1})
			tw.Write([]byte("x"))
		}
		tw.Close()
		gz.Close()
		f := filepath.Join(t.TempDir(), "c.crate")
		os.WriteFile(f, buf.Bytes(), 0o644)
		return f
	}
	cache := t.TempDir()
	dir := filepath.Join(cache, "crates")
	os.MkdirAll(dir, 0o755)
	os.MkdirAll(filepath.Join(cache, "tmp"), 0o755)
	sum := strings.Repeat("a", 64)
	if err := unpackCrate(mk("serde-1.0.0/Cargo.toml", "serde-1.0.0/src/lib.rs"), dir, "serde-1.0.0", sum); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "serde-1.0.0", ".cargo-checksum.json")); !strings.Contains(string(b), sum) {
		t.Fatalf("checksum file %q", b)
	}
	for _, bad := range []string{"serde-1.0.0/../../escaped", "other-1.0.0/Cargo.toml"} {
		if err := unpackCrate(mk(bad), dir, "serde-1.0.0", sum); err == nil {
			t.Errorf("%s: unpacked", bad)
		}
	}
	if _, err := os.Stat(filepath.Join(cache, "escaped")); err == nil {
		t.Fatal("wrote outside the crate's folder")
	}
}

func TestPickWheel(t *testing.T) {
	w := func(file string) struct{ URL, Hash string } {
		return struct{ URL, Hash string }{"https://files.pythonhosted.org/p/" + url.PathEscape(file), "sha256:" + strings.Repeat("b", 64)}
	}
	wheels := []struct{ URL, Hash string }{
		w("torch-2.14.1+cu132-cp312-cp312-win_amd64.whl"),
		w("torch-2.14.1+cu132-cp312-cp312-manylinux_2_28_x86_64.whl"),
		w("torch-2.14.1+cu132-cp312-cp312-manylinux_2_28_aarch64.whl"),
	}
	if p, ok := pickWheel(wheels, "cp312", "aarch64"); !ok || p.file != "torch-2.14.1+cu132-cp312-cp312-manylinux_2_28_aarch64.whl" {
		t.Fatalf("picked %+v %v", p, ok)
	}
	if _, ok := pickWheel(wheels, "cp311", "aarch64"); ok {
		t.Fatal("picked a wheel for another Python")
	}
	if p, ok := pickWheel([]struct{ URL, Hash string }{w("six-1.17.0-py2.py3-none-any.whl")}, "cp312", "x86_64"); !ok || p.file != "six-1.17.0-py2.py3-none-any.whl" {
		t.Fatalf("pure-Python wheel: %+v %v", p, ok)
	}
}
