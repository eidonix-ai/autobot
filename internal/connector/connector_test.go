package connector

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The job container's settings are the isolation boundary on a donor's machine.
func TestJobContainerIsIsolated(t *testing.T) {
	s := spec{Name: "autobot-run-1", Image: "sha256:abc", Cmd: []string{"bash"}, Run: 1, CPUs: 4, MemoryGiB: 24, ScratchGB: 16,
		GPUs: []string{"nvidia.com/gpu=GPU-1"}, Binds: []string{"/models:/models:ro"}}
	body := s.createBody()
	host := body["HostConfig"].(map[string]any)
	want := map[string]any{
		"NetworkMode": "none", "ReadonlyRootfs": true, "CapDrop": []string{"ALL"},
		"SecurityOpt": []string{"no-new-privileges"}, "Privileged": false,
		"Memory": int64(24) << 30, "MemorySwap": int64(24) << 30, "NanoCpus": int64(4e9), "PidsLimit": 4096,
	}
	for k, v := range want {
		if !reflect.DeepEqual(host[k], v) {
			t.Errorf("HostConfig.%s = %v, want %v", k, host[k], v)
		}
	}
	if body["User"] != "1000:1000" || body["NetworkDisabled"] != true {
		t.Errorf("user %v, network disabled %v", body["User"], body["NetworkDisabled"])
	}
	for _, k := range []string{"Mounts", "Devices", "CapAdd", "PortBindings", "PidMode", "IpcMode", "UsernsMode", "VolumesFrom"} {
		if _, ok := host[k]; ok {
			t.Errorf("HostConfig sets %s", k)
		}
	}
	tmpfs := host["Tmpfs"].(map[string]string)
	if len(tmpfs) != 2 || tmpfs["/work"] != "rw,nosuid,nodev,size=16g,mode=0700,uid=1000,gid=1000" {
		t.Errorf("tmpfs %v", tmpfs)
	}
	for _, b := range host["Binds"].([]string) {
		if b[len(b)-3:] != ":ro" {
			t.Errorf("bind %q is writable", b)
		}
	}
	gpus := host["DeviceRequests"].([]map[string]any)
	if len(gpus) != 1 || !reflect.DeepEqual(gpus[0]["DeviceIDs"], []string{"nvidia.com/gpu=GPU-1"}) {
		t.Errorf("GPUs %v", gpus)
	}
}

func tarball(t *testing.T, entries []tar.Header) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, h := range entries {
		data := []byte("x")
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(data))
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write(data)
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// A hostile tarball must not write outside the folder it is unpacked into.
func TestUnpackStaysInside(t *testing.T) {
	good := tarball(t, []tar.Header{
		{Name: "o-r-abc/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "o-r-abc/src/main.go", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "o-r-abc/link", Typeflag: tar.TypeSymlink, Linkname: "src/main.go"},
	})
	dir := filepath.Join(t.TempDir(), "source")
	if err := unpack(bytes.NewReader(good), dir); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "src/main.go")); err != nil || string(b) != "x" {
		t.Fatalf("main.go: %q %v", b, err)
	}

	outside := t.TempDir()
	for name, entries := range map[string][]tar.Header{
		"dot-dot path": {{Name: "o-r-abc/../../escaped", Typeflag: tar.TypeReg, Mode: 0o644}},
		"through a symlink": {
			{Name: "o-r-abc/out", Typeflag: tar.TypeSymlink, Linkname: outside},
			{Name: "o-r-abc/out/escaped", Typeflag: tar.TypeReg, Mode: 0o644},
		},
	} {
		dir := filepath.Join(t.TempDir(), "source")
		err := unpack(bytes.NewReader(tarball(t, entries)), dir)
		if err == nil {
			t.Errorf("%s: unpacked without error", name)
		}
		if _, err := os.Stat(filepath.Join(outside, "escaped")); err == nil {
			t.Fatalf("%s: wrote outside the folder", name)
		}
	}
}
