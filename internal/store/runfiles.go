package store

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

// What a run produced is kept in files, not in the database: test-run output and agent
// conversations grow without bound, and SQLite keeps only the summary rows.
//
// The files live in a folder named runs, next to the database file, and are gzip-compressed. A
// file grows by appending: each write is one complete gzip member, and a reader (Go's gzip.Reader
// reads concatenated members as one stream) sees them joined. A crash can leave a cut-off last
// member, which readers ignore.
//
//	runs/test-<id>.log.gz     a test run's output
//	runs/conv-<id>.jsonl.gz   an agent conversation (see conversations.go)

// RunsDir is the folder the run files are kept in.
func (s *Store) RunsDir() string { return filepath.Join(filepath.Dir(s.path), "runs") }

func (s *Store) testLog(id int64) string {
	return filepath.Join(s.RunsDir(), "test-"+strconv.FormatInt(id, 10)+".log.gz")
}

func (s *Store) convFile(id int64) string {
	return filepath.Join(s.RunsDir(), "conv-"+strconv.FormatInt(id, 10)+".jsonl.gz")
}

// member compresses data as one gzip member.
func member(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(data)
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// appendGz adds data to a file as one gzip member, creating the file (and the folder) if needed.
func (s *Store) appendGz(path string, data []byte) error {
	z, err := member(data)
	if err != nil {
		return err
	}
	s.files.Lock()
	defer s.files.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	if _, err := f.Write(z); err != nil { // one write, so a member is never interleaved with another
		f.Close()
		return err
	}
	return f.Close()
}

// readGz reads a whole file. A cut-off last member (a crash while writing) ends the data there.
func readGz(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		if errors.Is(err, io.EOF) { // an empty file
			return nil, nil
		}
		return nil, err
	}
	out, err := io.ReadAll(zr)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		err = nil
	}
	return out, err
}

// writeGz replaces a file with data, through a temporary file so a crash never leaves half of it.
func (s *Store) writeGz(path string, data []byte) error {
	z, err := member(data)
	if err != nil {
		return err
	}
	s.files.Lock()
	defer s.files.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, z, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func removeFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// TailKept is how much of a test run's output its database row keeps, for summaries.
const TailKept = 4 << 10

// tail is the last n bytes of s, starting on a character boundary.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s
}

// oneLine is the first line of a text, cut to 200 characters and marked when anything was left
// out: what a step's output is shortened to once it is old. Shortening it again changes nothing.
func oneLine(s string) string {
	line, rest, _ := strings.Cut(strings.TrimSpace(s), "\n")
	cut := strings.TrimSpace(rest) != ""
	if !cut && len(line) <= 205 && strings.HasSuffix(line, " […]") {
		return line // already shortened
	}
	if len(line) > 200 {
		line, cut = strings.ToValidUTF8(line[:200], ""), true
	}
	if cut {
		line += " […]"
	}
	return line
}

// stepOutputOld is the SQL condition for a step output that oneLine would change.
const stepOutputOld = `(instr(output, char(10)) > 0 OR length(output) > 205)`
