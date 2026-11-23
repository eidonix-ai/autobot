package connector

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
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
		// exec: a job runs what it builds (build scripts, test binaries). Docker's default is noexec.
		"Tmpfs": map[string]string{
			"/work": fmt.Sprintf("rw,exec,nosuid,nodev,size=%dg,mode=0700,uid=1000,gid=1000", s.ScratchGB),
			"/tmp":  "rw,exec,nosuid,nodev,size=1g,mode=1777",
		},
		"Binds": binds,
	}
	if len(s.GPUs) > 0 {
		host["DeviceRequests"] = []map[string]any{{"Driver": "cdi", "DeviceIDs": s.GPUs}}
	}
	return map[string]any{
		"Image":           s.Image,
		"Entrypoint":      []string{}, // run Cmd itself, not the image's start-up script
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
	return demux(resp.Body, w)
}

// demux copies a multiplexed stream (a container's logs, or a command's output) to w. Without a
// TTY, Docker frames each write: one byte of stream, three of padding, four of length.
func demux(src io.Reader, w io.Writer) error {
	r := bufio.NewReader(src)
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

// exec runs a command in a running container as the container's own user, copying its output
// (stdout and stderr, in order) to w, and returns its exit code. The command gets no input.
func (d *docker) exec(ctx context.Context, id string, cmd []string, w io.Writer) (int, error) {
	var created struct{ Id string }
	err := d.call(ctx, "POST", "/containers/"+id+"/exec", map[string]any{
		"AttachStdout": true, "AttachStderr": true, "Tty": false, "WorkingDir": "/work", "Cmd": cmd}, &created)
	if err != nil {
		return -1, err
	}
	b, _ := json.Marshal(map[string]any{"Detach": false, "Tty": false})
	req, err := http.NewRequestWithContext(ctx, "POST", "http://docker/exec/"+created.Id+"/start", bytes.NewReader(b))
	if err != nil {
		return -1, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.http.Do(req)
	if err != nil {
		return -1, fmt.Errorf("docker: %w", err)
	}
	streamErr := demux(resp.Body, w)
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return -1, fmt.Errorf("docker: starting the command: %s", resp.Status)
	}
	if streamErr != nil {
		return -1, streamErr
	}
	// The stream ends when the command does, but Docker may take a moment to record the exit.
	for range 50 {
		var st struct {
			Running  bool
			ExitCode int
		}
		if err := d.call(ctx, "GET", "/exec/"+created.Id+"/json", nil, &st); err != nil {
			return -1, err
		}
		if !st.Running {
			return st.ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return -1, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return -1, errors.New("docker did not report the command's exit")
}
