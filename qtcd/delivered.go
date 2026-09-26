package qtcd

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jancona/qtc/envelope"
	"github.com/jancona/qtc/store"
)

// deliveredRetention is how long a (message, device) delivery is remembered.
// A message can be offered again for as long as some mailbox still holds it
// (a repair copies history to a recruit, whose EVENT reaches every watcher),
// so this outlasts the longest TTL an envelope can carry, plus the future
// timestamp tolerance.
const deliveredRetention = 0xFFFF*time.Minute + time.Hour

// deliveredTable is the delivered-once table (node protocol §7.4): which message
// has gone to which device, and when. With a journal it survives a restart,
// so a restart never repeats a delivery. A message skipped by the replay cap
// is recorded the same way, so it never comes back; the room address stands
// in for a device to dedup messages arriving on a room topic.
type deliveredTable struct {
	mu   sync.Mutex
	done map[deliveryKey]uint32 // delivery time
	j    *store.Journal         // nil keeps the table in memory only
	log  *slog.Logger
}

type deliveryKey struct {
	id     envelope.ID
	device envelope.Address
}

// deliveryLine is one line of the delivered journal. Device is a callsign,
// or "#NAME" for a room (envelope.Address.String). Skipped marks a message
// the replay cap left out (for reading the journal; it loads as delivered).
// Forget undoes an earlier line: a delivery that could not be handed off.
type deliveryLine struct {
	ID      string `json:"id"`
	Device  string `json:"device"`
	At      uint32 `json:"at"`
	Skipped bool   `json:"skipped,omitempty"`
	Forget  bool   `json:"forget,omitempty"`
}

func newDeliveredTable() *deliveredTable {
	return &deliveredTable{done: map[deliveryKey]uint32{}}
}

// openDeliveredTable loads the journal at path, dropping deliveries older than
// the retention period, and compacts it.
func openDeliveredTable(path string, now uint32, log *slog.Logger) (*deliveredTable, error) {
	d := &deliveredTable{done: map[deliveryKey]uint32{}, log: log}
	cutoff := now - min(now, uint32(deliveredRetention/time.Second))
	j, bad, err := store.OpenJournal(path, func(line []byte) error {
		var dl deliveryLine
		if err := json.Unmarshal(line, &dl); err != nil {
			return err
		}
		k, err := dl.key()
		if err != nil {
			return err
		}
		switch {
		case dl.Forget:
			delete(d.done, k)
		case dl.At >= cutoff:
			d.done[k] = dl.At
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if bad > 0 {
		log.Warn("delivered journal: skipped unreadable lines", "path", path, "n", bad)
	}
	d.j = j
	if err := d.compact(); err != nil {
		j.Close()
		return nil, err
	}
	return d, nil
}

func (dl deliveryLine) key() (deliveryKey, error) {
	b, err := hex.DecodeString(dl.ID)
	if err != nil || len(b) != len(envelope.ID{}) {
		return deliveryKey{}, fmt.Errorf("qtcd: delivered journal: bad id %q", dl.ID)
	}
	dev, err := envelope.ParseAddress(dl.Device)
	if err != nil {
		return deliveryKey{}, err
	}
	return deliveryKey{id: envelope.IDFromBytes(b), device: dev}, nil
}

// mark records a delivery at now, returning false if it already happened.
// A journal failure is logged, not fatal: the cost is at most one repeat
// delivery after a restart.
func (d *deliveredTable) mark(k deliveryKey, now uint32) bool {
	return d.record(k, now, false)
}

// skip records a message the replay cap left out, so that it is never
// offered to the device again. It reports false if it was already recorded.
func (d *deliveredTable) skip(k deliveryKey, now uint32) bool {
	return d.record(k, now, true)
}

func (d *deliveredTable) record(k deliveryKey, now uint32, skipped bool) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, done := d.done[k]; done {
		return false
	}
	d.done[k] = now
	d.append(deliveryLine{ID: k.id.String(), Device: k.device.String(), At: now, Skipped: skipped})
	return true
}

// has reports whether k is recorded.
func (d *deliveredTable) has(k deliveryKey) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, done := d.done[k]
	return done
}

// forget removes k after a delivery that was marked but could not be handed
// off, so the message can be delivered later.
func (d *deliveredTable) forget(k deliveryKey) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, done := d.done[k]; !done {
		return
	}
	delete(d.done, k)
	d.append(deliveryLine{ID: k.id.String(), Device: k.device.String(), Forget: true})
}

// append journals one line. Callers hold d.mu.
func (d *deliveredTable) append(l deliveryLine) {
	if d.j == nil {
		return
	}
	if err := d.j.Append(l); err != nil {
		d.log.Warn("delivered journal append failed", "err", err)
	}
}

// expire forgets deliveries older than the retention period and compacts
// the journal once dead lines outnumber live ones.
func (d *deliveredTable) expire(now uint32) {
	cutoff := now - min(now, uint32(deliveredRetention/time.Second))
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, at := range d.done {
		if at < cutoff {
			delete(d.done, k)
		}
	}
	if d.j != nil && d.j.Lines() > 2*len(d.done)+256 {
		if err := d.compact(); err != nil {
			d.log.Warn("delivered journal compaction failed", "err", err)
		}
	}
}

// compact rewrites the journal with the live table. Callers hold d.mu or
// have exclusive access.
func (d *deliveredTable) compact() error {
	return d.j.Rewrite(func(emit func(any) error) error {
		for k, at := range d.done {
			if err := emit(deliveryLine{ID: k.id.String(), Device: k.device.String(), At: at}); err != nil {
				return err
			}
		}
		return nil
	})
}

func (d *deliveredTable) close() error {
	if d.j == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.j.Close()
}
