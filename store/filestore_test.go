package store

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// testStore is what the shared store tests need.
type testStore interface {
	Store
	Len() int
}

// eachStore runs f against a MemStore and a FileStore, each capped at
// maxPer records per callsign (0 is unlimited).
func eachStore(t *testing.T, maxPer int, f func(*testing.T, testStore)) {
	t.Run("mem", func(t *testing.T) {
		m := NewMemStore()
		m.MaxPerCallsign = maxPer
		f(t, m)
	})
	t.Run("file", func(t *testing.T) {
		m := NewMemStore()
		m.MaxPerCallsign = maxPer
		fs, err := OpenFileStore(filepath.Join(t.TempDir(), "mailbox.jsonl"), m, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { fs.Close() })
		f(t, fs)
	})
}

func openFS(t *testing.T, path string, now uint32) *FileStore {
	t.Helper()
	fs, err := OpenFileStore(path, NewMemStore(), now, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fs.Close() })
	return fs
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "\n")
}

func TestFileStoreReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mailbox.jsonl")
	fs := openFS(t, path, t0)
	room := mustRoomAddr("NET")
	recs := []Record{
		{Callsign: w1aw, Env: msg(t, n1adjH, w1aw, t0, 60, 1, "one"), ReceivedAt: t0, Expiry: t0 + 100},
		{Callsign: w1aw, Env: msg(t, n1adjH, w1aw, t0, 60, 2, "two"), ReceivedAt: t0 + 1, Expiry: t0 + 1000},
		{Callsign: room, Env: msg(t, n1adjH, room, t0, 60, 3, "net"), ReceivedAt: t0 + 2, Expiry: t0 + 1000},
	}
	for _, r := range recs {
		if ok, err := fs.Put(r); !ok || err != nil {
			t.Fatalf("Put: %v %v", ok, err)
		}
	}
	fs.Close()

	// Reopened before anything expires: everything is back, and a repeat PUT
	// is still a duplicate.
	fs = openFS(t, path, t0+50)
	if fs.Len() != 3 {
		t.Fatalf("replayed %d, want 3", fs.Len())
	}
	if ok, err := fs.Put(recs[1]); ok || err != nil {
		t.Errorf("repeat Put after replay = %v, %v; want duplicate", ok, err)
	}
	got, _, _, _ := fs.Query(w1aw, 0, 10, nil)
	if b := bodiesRec(got); !reflect.DeepEqual(b, []string{"one", "two"}) {
		t.Errorf("W1AW after replay = %v", b)
	}
	if got, _, _, _ := fs.Query(room, 0, 10, nil); len(got) != 1 {
		t.Errorf("room after replay: %d records", len(got))
	}
	fs.Close()

	// Reopened after the first expires: it is dropped and compacted away.
	fs = openFS(t, path, t0+500)
	if fs.Len() != 2 {
		t.Errorf("after expiry replayed %d, want 2", fs.Len())
	}
	if n := countLines(t, path); n != 2 {
		t.Errorf("journal has %d lines after compaction, want 2", n)
	}
}

func TestFileStoreTornLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mailbox.jsonl")
	fs := openFS(t, path, t0)
	if _, err := fs.Put(Record{Callsign: w1aw, Env: msg(t, n1adj, w1aw, t0, 60, 1, "kept"), ReceivedAt: t0, Expiry: t0 + 100}); err != nil {
		t.Fatal(err)
	}
	fs.Close()
	// A crash mid-append leaves a partial line with no newline.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"callsign":"W1AW","received_at":`)
	f.Close()

	fs = openFS(t, path, t0)
	if fs.Len() != 1 {
		t.Fatalf("replayed %d, want 1", fs.Len())
	}
	if _, err := fs.Put(Record{Callsign: w1aw, Env: msg(t, n1adj, w1aw, t0, 60, 2, "after"), ReceivedAt: t0 + 1, Expiry: t0 + 100}); err != nil {
		t.Fatal(err)
	}
	fs.Close()
	if fs = openFS(t, path, t0); fs.Len() != 2 {
		t.Errorf("after torn line and a new put: %d records, want 2", fs.Len())
	}
}

func TestFileStoreExpireCompacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mailbox.jsonl")
	fs := openFS(t, path, t0)
	n := compactSlack + 10
	for i := 0; i < n; i++ {
		exp := t0 + 10
		if i == 0 {
			exp = t0 + 1000
		}
		if _, err := fs.Put(Record{Callsign: w1aw, Env: msg(t, n1adj, w1aw, t0, 60, uint16(i), "m"), ReceivedAt: t0, Expiry: exp}); err != nil {
			t.Fatal(err)
		}
	}
	if got := countLines(t, path); got != n {
		t.Fatalf("journal has %d lines, want %d", got, n)
	}
	if got := fs.Expire(t0 + 10); got != n-1 {
		t.Errorf("Expire = %d, want %d", got, n-1)
	}
	if got := countLines(t, path); got != 1 {
		t.Errorf("journal has %d lines after Expire, want 1", got)
	}
}

func TestFileStoreRefusesUnjournaled(t *testing.T) {
	fs := openFS(t, filepath.Join(t.TempDir(), "mailbox.jsonl"), t0)
	fs.j.f.Close() // every append now fails
	ok, err := fs.Put(Record{Callsign: w1aw, Env: msg(t, n1adj, w1aw, t0, 60, 1, "x"), ReceivedAt: t0, Expiry: t0 + 100})
	if ok || err == nil {
		t.Fatalf("Put with a broken journal = %v, %v; want an error", ok, err)
	}
	if fs.Len() != 0 {
		t.Error("record visible although it was never journaled")
	}
}
