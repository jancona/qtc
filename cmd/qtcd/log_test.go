package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestJournalLogger(t *testing.T) {
	var buf bytes.Buffer
	log := newLogger(&buf, slog.LevelDebug, true)
	log.Debug("d")
	log.Info("i", "k", "v")
	log.Warn("w")
	log.Error("e", "err", "line1\nline2")
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	want := []string{
		`<7>level=DEBUG msg=d`,
		`<6>level=INFO msg=i k=v`,
		`<4>level=WARN msg=w`,
		`<3>level=ERROR msg=e err="line1\nline2"`,
	}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%s", len(lines), len(want), buf.String())
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, lines[i], want[i])
		}
	}
}

func TestTerminalLoggerKeepsTime(t *testing.T) {
	var buf bytes.Buffer
	newLogger(&buf, slog.LevelInfo, false).Info("hello")
	if !strings.HasPrefix(buf.String(), "time=") {
		t.Errorf("terminal log line lost its time: %q", buf.String())
	}
}

func TestUnderJournal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no journal")
	}
	f, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	orig := os.Stderr
	os.Stderr = f
	defer func() { os.Stderr = orig }()

	t.Setenv("JOURNAL_STREAM", "")
	if underJournal() {
		t.Error("journal detected with JOURNAL_STREAM unset")
	}
	t.Setenv("JOURNAL_STREAM", fileID(f))
	if !underJournal() {
		t.Errorf("journal not detected for matching stream %s", fileID(f))
	}
	t.Setenv("JOURNAL_STREAM", "1:2")
	if underJournal() {
		t.Error("journal detected for an inherited stream that is not stderr")
	}
}
