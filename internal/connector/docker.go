package connector

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
)

// docker talks to the Docker Engine API over its Unix socket.
type docker struct{ http *http.Client }

func newDocker(socket string) *docker {
	return &docker{&http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}}
}

func (d *docker) call(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return fmt.Errorf("docker: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var e struct{ Message string }
		if json.Unmarshal(msg, &e) == nil && e.Message != "" {
			return fmt.Errorf("docker: %s", e.Message)
		}
		return fmt.Errorf("docker: %s %s: %s", method, path, resp.Status)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// spec is a job container's settings. Everything that isolates a job from the machine is here.
type spec struct {
	Name      string
	Image     string
	Cmd       []string
	Run       int64
	CPUs      int
	MemoryGiB int
	ScratchGB int
	GPUs      []string // CDI device names, such as nvidia.com/gpu=GPU-…
	Binds     []string // host:container:ro
}

// createBody is the Engine API request for a job container: no network, a read-only root, a
// non-root user with no capabilities and no privilege gain, capped CPU, memory (no swap) and
// processes, scratch only in a size-capped in-memory /work, and only the GPUs and read-only
// folders it is given.
func (s spec) createBody() map[string]any {
	mem := int64(s.MemoryGiB) << 30
	binds := append([]string{}, s.Binds...)
	host := map[string]any{
		"NetworkMode":    "none",
		"ReadonlyRootfs": true,
		"CapDrop":        []string{"ALL"},
		"SecurityOpt":    []string{"no-new-privileges"},
		"Privileged":     false,
		"NanoCpus":       int64(s.CPUs) * 1e9,
		"Memory":         mem,
		"MemorySwap":     mem,
		"PidsLimit":      4096,
		"Init":           true,
		"ShmSize":        int64(1) << 30,
		"Tmpfs": map[string]string{
			"/work": fmt.Sprintf("rw,nosuid,nodev,size=%dg,mode=0700,uid=1000,gid=1000", s.ScratchGB),
			"/tmp":  "rw,nosuid,nodev,size=1g,mode=1777",
		},
		"Binds": binds,
	}
	if len(s.GPUs) > 0 {
		host["DeviceRequests"] = []map[string]any{{"Driver": "cdi", "DeviceIDs": s.GPUs}}
	}
	return map[string]any{
		"Image":           s.Image,
		"Cmd":             s.Cmd,
		"User":            "1000:1000",
		"WorkingDir":      "/work",
		"Env":             []string{"HOME=/work", "TMPDIR=/work/tmp"},
		"Labels":          map[string]string{runLabel: strconv.FormatInt(s.Run, 10)},
		"NetworkDisabled": true,
		"Tty":             false,
		"HostConfig":      host,
	}
}

// runLabel marks every job container, so leftovers can be found and removed.
const runLabel = "org.eidonix.autobot.run"

func (d *docker) create(ctx context.Context, s spec) (string, error) {
	var out struct{ Id string }
	err := d.call(ctx, "POST", "/containers/create?name="+url.QueryEscape(s.Name), s.createBody(), &out)
	return out.Id, err
}

func (d *docker) start(ctx context.Context, id string) error {
	return d.call(ctx, "POST", "/containers/"+id+"/start", nil, nil)
}

func (d *docker) kill(ctx context.Context, id string) error {
	return d.call(ctx, "POST", "/containers/"+id+"/kill", nil, nil)
}

func (d *docker) remove(ctx context.Context, id string) error {
	return d.call(ctx, "DELETE", "/containers/"+id+"?force=1", nil, nil)
}

// wait blocks until the container exits and returns its exit code.
func (d *docker) wait(ctx context.Context, id string) (int, error) {
	var out struct{ StatusCode int }
	err := d.call(ctx, "POST", "/containers/"+id+"/wait", nil, &out)
	return out.StatusCode, err
}

func (d *docker) oomKilled(ctx context.Context, id string) bool {
	var out struct{ State struct{ OOMKilled bool } }
	return d.call(ctx, "GET", "/containers/"+id+"/json", nil, &out) == nil && out.State.OOMKilled
}

// imageID returns the id of a local image, which jobs then run by.
func (d *docker) imageID(ctx context.Context, name string) (string, error) {
	var out struct{ Id string }
	err := d.call(ctx, "GET", "/images/"+url.PathEscape(name)+"/json", nil, &out)
	return out.Id, err
}

// leftovers lists job containers from earlier runs.
func (d *docker) leftovers(ctx context.Context) ([]string, error) {
	filters, _ := json.Marshal(map[string][]string{"label": {runLabel}})
	var out []struct{ Id string }
	if err := d.call(ctx, "GET", "/containers/json?all=1&filters="+url.QueryEscape(string(filters)), nil, &out); err != nil {
		return nil, err
	}
	var ids []string
	for _, c := range out {
		ids = append(ids, c.Id)
	}
	return ids, nil
}

// logs copies the container's stdout and stderr, in order, to w until it exits.
func (d *docker) logs(ctx context.Context, id string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, "GET", "http://docker/containers/"+id+"/logs?follow=1&stdout=1&stderr=1", nil)
	if err != nil {
		return err
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Without a TTY, Docker frames each write: one byte of stream, three of padding, four of length.
	r := bufio.NewReader(resp.Body)
	var head [8]byte
	for {
		if _, err := io.ReadFull(r, head[:]); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if _, err := io.CopyN(w, r, int64(binary.BigEndian.Uint32(head[4:]))); err != nil {
			return err
		}
	}
}
