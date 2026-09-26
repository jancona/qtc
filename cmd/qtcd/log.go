package main

import (
	"bytes"
	"io"
	"log/slog"
	"os"
)

// newLogger logs to w in slog's text format. With journal set (w is the
// systemd journal stream), it leaves out the time (the journal
// stamps every line) and prefixes each line with its syslog priority, so
// journalctl -p filters and highlights by level.
func newLogger(w io.Writer, level slog.Level, journal bool) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	if journal {
		opts.ReplaceAttr = func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		}
		w = journalWriter{w}
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// underJournal reports whether stderr is the journal stream systemd set up
// for this service: JOURNAL_STREAM holds its device and inode. A process
// that inherited the variable but writes elsewhere does not match.
func underJournal() bool {
	env := os.Getenv("JOURNAL_STREAM")
	return env != "" && fileID(os.Stderr) == env
}

// journalWriter adds the sd-daemon priority prefix ("<3>" and so on) that
// journald reads from the start of a line. slog handlers write each record
// in one Write, and without the time a text record starts with its level.
type journalWriter struct{ w io.Writer }

var journalPriorities = []struct {
	level  []byte
	prefix []byte
}{
	{[]byte("level=ERROR"), []byte("<3>")},
	{[]byte("level=WARN"), []byte("<4>")},
	{[]byte("level=INFO"), []byte("<6>")},
	{[]byte("level=DEBUG"), []byte("<7>")},
}

func (j journalWriter) Write(p []byte) (int, error) {
	for _, jp := range journalPriorities {
		if bytes.HasPrefix(p, jp.level) {
			if _, err := j.w.Write(append(append([]byte(nil), jp.prefix...), p...)); err != nil {
				return 0, err
			}
			return len(p), nil
		}
	}
	return j.w.Write(p)
}
