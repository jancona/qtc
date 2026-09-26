package qtcd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jancona/qtc/envelope"
)

func TestDeliveredTableJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "delivered.jsonl")
	now := uint32(1789128000)
	old := deliveryKey{id: envelope.ID{1}, device: mustAddr(t, "N1ADJ  H")}
	recent := deliveryKey{id: envelope.ID{2}, device: mustAddr(t, "W1AW")}

	d, err := openDeliveredTable(path, now, testLog(t, "d"))
	if err != nil {
		t.Fatal(err)
	}
	retention := uint32(deliveredRetention / time.Second)
	if !d.mark(old, now-retention+10) || !d.mark(recent, now) {
		t.Fatal("first mark refused")
	}
	if d.mark(recent, now) {
		t.Error("second mark accepted")
	}
	d.close()

	// Reopened within retention: both remembered.
	d, err = openDeliveredTable(path, now+5, testLog(t, "d"))
	if err != nil {
		t.Fatal(err)
	}
	if d.mark(old, now+5) || d.mark(recent, now+5) {
		t.Error("delivery forgotten across reopen")
	}
	d.close()

	// Reopened after the old one's retention: it is dropped and compacted away.
	d, err = openDeliveredTable(path, now+20, testLog(t, "d"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	if d.mark(recent, now+20) {
		t.Error("recent delivery forgotten")
	}
	b, _ := os.ReadFile(path)
	if n := strings.Count(string(b), "\n"); n != 1 {
		t.Errorf("journal has %d lines after compaction, want 1:\n%s", n, b)
	}
	if !d.mark(old, now+20) {
		t.Error("expired delivery still remembered")
	}
}

// TestStationRestart stops a mailbox station and starts another on the same
// DataDir: same identity, same mailbox, and no repeat delivery.
func TestStationRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state") // not yet created, as on first run
	cfg := Config{
		Callsign:    "N1ADJ  Z",
		DataDir:     dir,
		Caps:        CapMailbox,
		ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"},
		Log:         testLog(t, "restart"),
	}
	start := func() *Station {
		t.Helper()
		r, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		return r
	}
	ht, w1aw := mustAddr(t, "N1ADJ  H"), mustAddr(t, "W1AW")
	e := mustMsg(t, w1aw, ht, unixNow(), 60, 1, 0, "survives")

	r := start()
	id := r.ID()
	if stored, err := r.server.PutLocal(ht, e); !stored || err != nil {
		t.Fatalf("PutLocal = %v, %v", stored, err)
	}
	if !r.markDelivered(e, ht) {
		t.Fatal("first delivery refused")
	}
	if err := r.Stop(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"node.key", "mailbox.jsonl", "delivered.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("state file: %v", err)
		}
	}

	r = start()
	defer r.Stop()
	if r.ID() != id {
		t.Errorf("peer ID changed across restart: %s -> %s", id, r.ID())
	}
	recs, _, _, err := r.mem.Query(ht.Base(), 0, 10, nil)
	if err != nil || len(recs) != 1 || recs[0].ID() != e.StoreID() {
		t.Errorf("mailbox after restart = %v, %v", recs, err)
	}
	if r.markDelivered(e, ht) {
		t.Error("delivery repeated after restart")
	}
}

// TestDeliveredTableRoomsAndForget: room entries (the room address standing
// in for a device) and forgotten deliveries survive a reload correctly.
func TestDeliveredTableRoomsAndForget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "delivered.jsonl")
	now := uint32(1789128000)
	room := deliveryKey{id: envelope.ID{1}, device: mustRoom(t, "NET")}
	gone := deliveryKey{id: envelope.ID{2}, device: mustAddr(t, "N1ADJ  H")}
	skipped := deliveryKey{id: envelope.ID{3}, device: mustAddr(t, "N1ADJ  H")}

	d, err := openDeliveredTable(path, now, testLog(t, "d"))
	if err != nil {
		t.Fatal(err)
	}
	d.mark(room, now)
	d.mark(gone, now)
	d.forget(gone)
	d.skip(skipped, now)
	d.close()
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), `"device":"#NET"`) || !strings.Contains(string(b), `"skipped":true`) {
		t.Errorf("journal:\n%s", b)
	}

	d, err = openDeliveredTable(path, now, testLog(t, "d"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	if !d.has(room) {
		t.Error("room entry lost on reload")
	}
	if d.has(gone) {
		t.Error("forgotten delivery came back on reload")
	}
	if !d.has(skipped) {
		t.Error("skipped message lost on reload")
	}
}
