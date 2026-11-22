package connector

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
)

const (
	maxSourceBytes = 4 << 30 // unpacked
	maxSourceFiles = 500_000
)

// fetchSource downloads a run's code from Autobot, which fetches it from GitHub at the run's
// commit, and unpacks it into dir, which must not exist yet.
func (c *Connector) fetchSource(ctx context.Context, run int64, dir string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/connector/v1/runs/%d/source", strings.TrimSuffix(c.cfg.URL, "/"), run), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	req.Header.Set("User-Agent", "autobot-connector/"+Version)
	resp, err := (&http.Client{Timeout: 15 * time.Minute}).Do(req)
	if err != nil {
		return fmt.Errorf("download the code: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("download the code: %s", strings.TrimSpace(string(msg)))
	}
	return unpack(resp.Body, dir)
}

// unpack extracts a GitHub tarball into dir, dropping its top-level folder. Every write goes
// through os.Root, so no entry, link or path can land outside dir.
func unpack(r io.Reader, dir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("unpack source: %w", err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	tr := tar.NewReader(gz)
	var total int64
	for files := 0; ; files++ {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("unpack source: %w", err)
		}
		if files > maxSourceFiles {
			return errors.New("unpack source: too many files")
		}
		_, name, _ := strings.Cut(h.Name, "/") // GitHub puts everything under owner-repo-commit/
		name = path.Clean(name)
		if name == "." || name == "" || h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		if !fs.ValidPath(name) {
			return fmt.Errorf("unpack source: bad path %q", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if total += h.Size; total > maxSourceBytes {
				return errors.New("unpack source: larger than 4 GB")
			}
			if err := root.MkdirAll(path.Dir(name), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, io.LimitReader(tr, h.Size))
			if err := errors.Join(err, f.Close()); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := root.MkdirAll(path.Dir(name), 0o755); err != nil {
				return err
			}
			if err := root.Symlink(h.Linkname, name); err != nil {
				return err
			}
		default:
			// Hard links, devices and the like have no place in a source tree.
		}
	}
}
