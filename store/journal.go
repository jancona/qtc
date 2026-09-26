package store

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Journal is an append-only JSON Lines file. Each Append is written and
// fsynced before it returns; Rewrite replaces the whole file atomically, which
// is how callers drop entries that no longer matter. A Journal is not safe for
// concurrent use; callers serialize access.
type Journal struct {
	path  string
	f     *os.File
	lines int
}

// OpenJournal opens or creates the journal at path, calling each for every
// line already in it, in order. A line each rejects (or that is not
// complete, such as a write torn by a crash) is skipped and counted in bad;
// it never stops the replay.
func OpenJournal(path string, each func(line []byte) error) (j *Journal, bad int, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, 0, fmt.Errorf("store: journal dir: %w", err)
	}
	lines := 0
	b, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
	case err != nil:
		return nil, 0, fmt.Errorf("store: read journal: %w", err)
	default:
		sc := bufio.NewScanner(bytes.NewReader(b))
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			line := sc.Bytes()
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			lines++
			if !json.Valid(line) || each(line) != nil {
				bad++
			}
		}
		if err := sc.Err(); err != nil {
			return nil, bad, fmt.Errorf("store: scan journal %s: %w", path, err)
		}
		if len(b) > 0 && b[len(b)-1] != '\n' {
			// A torn final line: make sure the next append starts a new one.
			if err := appendNewline(path); err != nil {
				return nil, bad, err
			}
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, bad, fmt.Errorf("store: open journal: %w", err)
	}
	return &Journal{path: path, f: f, lines: lines}, bad, nil
}

func appendNewline(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("store: repair journal: %w", err)
	}
	_, err = f.Write([]byte{'\n'})
	return errors.Join(err, f.Close())
}

// Append writes v as one JSON line and fsyncs it.
func (j *Journal) Append(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("store: journal encode: %w", err)
	}
	if _, err := j.f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("store: journal write: %w", err)
	}
	if err := j.f.Sync(); err != nil {
		return fmt.Errorf("store: journal sync: %w", err)
	}
	j.lines++
	return nil
}

// Rewrite replaces the journal with the lines write emits, via a temporary
// file that is fsynced and renamed into place, so a crash leaves either the
// old journal or the new one.
func (j *Journal) Rewrite(write func(emit func(v any) error) error) error {
	tmp := j.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("store: journal rewrite: %w", err)
	}
	w := bufio.NewWriter(f)
	n := 0
	emit := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("store: journal encode: %w", err)
		}
		n++
		_, err = w.Write(append(b, '\n'))
		return err
	}
	err = write(emit)
	if err == nil {
		err = w.Flush()
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, j.path)
	}
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("store: journal rewrite: %w", err)
	}
	syncDir(filepath.Dir(j.path))
	nf, err := os.OpenFile(j.path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("store: reopen journal: %w", err)
	}
	j.f.Close()
	j.f = nf
	j.lines = n
	return nil
}

// syncDir makes a rename durable. Best effort: not every platform can fsync
// a directory.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}

// Lines is the number of lines in the journal, live or not.
func (j *Journal) Lines() int { return j.lines }

// Close closes the journal file.
func (j *Journal) Close() error { return j.f.Close() }
