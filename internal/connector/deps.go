package connector

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
)

// Dependencies come in over this machine's own connection, before the job starts. The connector
// reads the lockfiles in the run's code as data (it runs nothing from them), downloads each listed
// file from a fixed list of public hosts, checks it against the lockfile's SHA-256, and keeps it in
// a cache shared by later runs. The job, which has no network, sees them read-only:
//
//	/deps/crates  every crates.io crate the Cargo.lock files name, as a Cargo directory source
//	/deps/wheels  the Python wheels uv.lock names that the work image does not already have,
//	              with requirements.txt listing them
//
// Packages that must be built from source (git dependencies, source-only Python packages) are not
// fetched: they come from the work image, and the run's output says which they are.

// allowedHosts are the only hosts dependencies are downloaded from.
var allowedHosts = map[string]bool{
	"static.crates.io":        true,
	"files.pythonhosted.org":  true,
	"download.pytorch.org":    true,
	"download-r2.pytorch.org": true,
}

const (
	maxDepFile  = 4 << 30  // one download
	maxDepTotal = 20 << 30 // all downloads for one run
)

var (
	crateNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	versionRE   = regexp.MustCompile(`^[0-9A-Za-z.+-]{1,64}$`)
	wheelNameRE = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,255}\.whl$`)
	sha256RE    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// imageInfo is what the work image already has, found once per image.
type imageInfo struct {
	pythonTag string          // such as cp312; "" when the image has no Python
	installed map[string]bool // normalized name==version of every installed Python package
}

// deps prepares the run's dependencies and returns the read-only mounts for the job.
type deps struct {
	c       *Connector
	out     io.Writer
	cache   string // DATA_DIR/cache
	runDir  string
	mu      sync.Mutex
	fetched int64 // bytes downloaded for this run
}

func (c *Connector) prepareDeps(ctx context.Context, source, runDir string, out io.Writer) ([]string, error) {
	d := &deps{c: c, out: out, cache: filepath.Join(c.cfg.DataDir, "cache"), runDir: runDir}
	var binds []string
	locks, err := findLockfiles(source)
	if err != nil {
		return nil, err
	}
	if len(locks["Cargo.lock"]) > 0 {
		if err := d.crates(ctx, locks["Cargo.lock"]); err != nil {
			return nil, err
		}
		binds = append(binds, filepath.Join(d.cache, "crates")+":/deps/crates:ro")
	}
	if len(locks["uv.lock"]) > 0 {
		dir, err := d.wheels(ctx, locks["uv.lock"])
		if err != nil {
			return nil, err
		}
		if dir != "" {
			binds = append(binds, dir+":/deps/wheels:ro")
		}
	}
	return binds, nil
}

// findLockfiles finds Cargo.lock and uv.lock files in the code, skipping build output.
func findLockfiles(root string) (map[string][]string, error) {
	found := map[string][]string{}
	err := filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() && (e.Name() == "target" || e.Name() == "node_modules" || strings.HasPrefix(e.Name(), ".")) && p != root {
			return fs.SkipDir
		}
		if e.Type().IsRegular() && (e.Name() == "Cargo.lock" || e.Name() == "uv.lock") {
			found[e.Name()] = append(found[e.Name()], p)
		}
		return nil
	})
	return found, err
}

// crates makes every crates.io crate in the lockfiles available in the cache's crates folder.
func (d *deps) crates(ctx context.Context, locks []string) error {
	type crate struct{ name, version, sum string }
	want := map[string]crate{}
	git := map[string]bool{}
	for _, lock := range locks {
		var f struct {
			Package []struct{ Name, Version, Source, Checksum string }
		}
		if _, err := toml.DecodeFile(lock, &f); err != nil {
			return fmt.Errorf("read %s: %w", lock, err)
		}
		for _, p := range f.Package {
			switch {
			case strings.HasPrefix(p.Source, "git+"):
				git[p.Name] = true
			case p.Source == "registry+https://github.com/rust-lang/crates.io-index" || p.Source == "sparse+https://index.crates.io/":
				if !crateNameRE.MatchString(p.Name) || !versionRE.MatchString(p.Version) || !sha256RE.MatchString(p.Checksum) {
					return fmt.Errorf("%s: bad entry for crate %q", lock, p.Name)
				}
				want[p.Name+"-"+p.Version] = crate{p.Name, p.Version, p.Checksum}
			}
		}
	}
	dir := filepath.Join(d.cache, "crates")
	for _, p := range []string{dir, filepath.Join(d.cache, "tmp")} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			return err
		}
	}
	var missing []crate
	for key, c := range want {
		if have, err := os.ReadFile(filepath.Join(dir, key, ".cargo-checksum.json")); err == nil && strings.Contains(string(have), `"`+c.sum+`"`) {
			continue
		}
		missing = append(missing, c)
	}
	fmt.Fprintf(d.out, "Rust: %d crates from %d Cargo.lock files; %d already cached, downloading %d\n", len(want), len(locks), len(want)-len(missing), len(missing))
	err := parallel(ctx, len(missing), func(i int) error {
		c := missing[i]
		file, err := d.download(ctx, fmt.Sprintf("https://static.crates.io/crates/%s/%s-%s.crate", c.name, c.name, c.version), c.sum)
		if err != nil {
			return fmt.Errorf("crate %s %s: %w", c.name, c.version, err)
		}
		return unpackCrate(file, dir, c.name+"-"+c.version, c.sum)
	})
	if err != nil {
		return err
	}
	if len(git) > 0 {
		fmt.Fprintf(d.out, "Rust: %d crates come from git (%s) and are not fetched; the work image must have them\n", len(git), strings.Join(sorted(git), ", "))
	}
	return nil
}

// unpackCrate extracts a downloaded .crate into dir/key as a Cargo directory-source entry.
func unpackCrate(file, dir, key, sum string) error {
	tmp, err := os.MkdirTemp(filepath.Join(filepath.Dir(dir), "tmp"), "crate-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(tmp)
	if err != nil {
		return err
	}
	defer root.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := path.Clean(h.Name)
		if !fs.ValidPath(name) || (name != key && !strings.HasPrefix(name, key+"/")) {
			return fmt.Errorf("crate %s: unexpected path %q", key, h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			err = root.MkdirAll(name, 0o755)
		case tar.TypeReg:
			if err = root.MkdirAll(path.Dir(name), 0o755); err == nil {
				var w *os.File
				if w, err = root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644); err == nil {
					_, err = io.Copy(w, tr)
					err = errors.Join(err, w.Close())
				}
			}
		}
		if err != nil {
			return err
		}
	}
	// Cargo checks the package checksum against Cargo.lock; an empty file list skips per-file checks.
	if err := os.WriteFile(filepath.Join(tmp, key, ".cargo-checksum.json"), []byte(`{"files":{},"package":"`+sum+`"}`), 0o644); err != nil {
		return err
	}
	os.RemoveAll(filepath.Join(dir, key))
	return os.Rename(filepath.Join(tmp, key), filepath.Join(dir, key))
}

// wheels downloads the Python wheels uv.lock names for this machine that the work image lacks,
// links them into a folder for the run with a requirements.txt, and returns that folder ("" when
// nothing is missing).
func (d *deps) wheels(ctx context.Context, locks []string) (string, error) {
	img, err := d.c.image(ctx)
	if err != nil || img.pythonTag == "" {
		fmt.Fprintf(d.out, "Python: skipped, the work image has no usable Python (%v)\n", err)
		return "", nil
	}
	type wheel struct{ name, version, file, url, sum string }
	var todo []wheel
	unavailable := map[string]bool{}
	total := 0
	for _, lock := range locks {
		var f struct {
			Package []struct {
				Name, Version string
				Source        map[string]string
				Wheels        []struct{ URL, Hash string }
			}
		}
		if _, err := toml.DecodeFile(lock, &f); err != nil {
			return "", fmt.Errorf("read %s: %w", lock, err)
		}
		for _, p := range f.Package {
			if p.Source["registry"] == "" {
				continue // the project itself, local folders, git: not downloadable wheels
			}
			total++
			if img.installed[normalize(p.Name)+"=="+p.Version] {
				continue
			}
			w, ok := pickWheel(p.Wheels, img.pythonTag, d.c.arch())
			if !ok {
				unavailable[p.Name] = true
				continue
			}
			todo = append(todo, wheel{p.Name, p.Version, w.file, w.url, w.sum})
		}
	}
	fmt.Fprintf(d.out, "Python: %d packages in uv.lock; the work image has all but %d", total, len(todo)+len(unavailable))
	if len(todo) == 0 {
		fmt.Fprintln(d.out)
	} else {
		fmt.Fprintf(d.out, "; fetching %d wheels\n", len(todo))
	}
	if len(unavailable) > 0 {
		fmt.Fprintf(d.out, "Python: no wheel for this machine, so not fetched (other platforms, or needs building): %s\n", strings.Join(sorted(unavailable), ", "))
	}
	if len(todo) == 0 {
		return "", nil
	}
	dir := filepath.Join(d.runDir, "wheels")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	err = parallel(ctx, len(todo), func(i int) error {
		w := todo[i]
		file, err := d.download(ctx, w.url, w.sum)
		if err != nil {
			return fmt.Errorf("wheel %s %s: %w", w.name, w.version, err)
		}
		return os.Link(file, filepath.Join(dir, w.file))
	})
	if err != nil {
		return "", err
	}
	var req strings.Builder
	for _, w := range todo {
		fmt.Fprintf(&req, "%s==%s\n", w.name, w.version)
	}
	return dir, os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte(req.String()), 0o644)
}

type pickedWheel struct{ file, url, sum string }

// pickWheel chooses the wheel that installs on this machine: Linux on its CPU (or any platform),
// for its Python version (or any Python 3).
func pickWheel(wheels []struct{ URL, Hash string }, pyTag, arch string) (pickedWheel, bool) {
	for _, w := range wheels {
		u, err := url.Parse(w.URL)
		if err != nil || u.Scheme != "https" || !allowedHosts[u.Host] {
			continue
		}
		file, err := url.PathUnescape(path.Base(u.Path))
		if err != nil || !wheelNameRE.MatchString(file) {
			continue
		}
		sum, ok := strings.CutPrefix(w.Hash, "sha256:")
		if !ok || !sha256RE.MatchString(sum) {
			continue
		}
		// name-version(-build)?-python-abi-platform.whl
		parts := strings.Split(strings.TrimSuffix(file, ".whl"), "-")
		if len(parts) < 5 {
			continue
		}
		py, abi, plat := parts[len(parts)-3], parts[len(parts)-2], parts[len(parts)-1]
		platOK := plat == "any" || (strings.Contains(plat, "linux") && strings.HasSuffix(plat, arch))
		pyOK := false
		for _, t := range strings.Split(py, ".") {
			if t == pyTag || t == "py3" || (abi == "abi3" && strings.HasPrefix(t, "cp3") && t <= pyTag) {
				pyOK = true
			}
		}
		if platOK && pyOK {
			return pickedWheel{file, w.URL, sum}, true
		}
	}
	return pickedWheel{}, false
}

// normalize is a Python package name in its comparable form (PEP 503).
func normalize(name string) string {
	return strings.ToLower(regexp.MustCompile(`[-_.]+`).ReplaceAllString(name, "-"))
}

// download fetches a file from an allowed host into the cache, keyed by its SHA-256, and returns
// its path. A file already cached is not fetched again; a file that does not match is discarded.
func (d *deps) download(ctx context.Context, rawURL, sum string) (string, error) {
	dest := filepath.Join(d.cache, "files", sum[:2], sum)
	if _, err := os.Stat(dest); err == nil {
		return dest, nil
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || !allowedHosts[u.Host] {
		return "", fmt.Errorf("refusing to download from %q: not an allowed host", rawURL)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "autobot-connector/"+Version)
	resp, err := depClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s answered %s", u.Host, resp.Status)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".download-")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, maxDepFile+1))
	if err := errors.Join(err, tmp.Close()); err != nil {
		return "", err
	}
	if n > maxDepFile {
		return "", errors.New("larger than 4 GB")
	}
	d.mu.Lock()
	d.fetched += n
	over := d.fetched > maxDepTotal
	d.mu.Unlock()
	if over {
		return "", errors.New("the run's dependencies are over 20 GB")
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sum {
		return "", fmt.Errorf("checksum mismatch: the lockfile says %s, the download is %s", sum[:12], got[:12])
	}
	return dest, os.Rename(tmp.Name(), dest)
}

// depClient follows redirects only to allowed hosts.
var depClient = &http.Client{
	Timeout: 30 * time.Minute,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 5 || req.URL.Scheme != "https" || !allowedHosts[req.URL.Host] {
			return fmt.Errorf("refusing a redirect to %s", req.URL.Host)
		}
		return nil
	},
}

// parallel runs fn for 0..n-1 with eight workers and returns the first error.
func parallel(ctx context.Context, n int, fn func(int) error) error {
	jobs := make(chan int)
	var mu sync.Mutex
	var first error
	var wg sync.WaitGroup
	for range min(8, n) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				if err := fn(i); err != nil {
					mu.Lock()
					if first == nil {
						first = err
					}
					mu.Unlock()
				}
			}
		}()
	}
	for i := 0; i < n; i++ {
		mu.Lock()
		stop := first != nil
		mu.Unlock()
		if stop || ctx.Err() != nil {
			break
		}
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	if first == nil {
		first = ctx.Err()
	}
	return first
}

func sorted(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
