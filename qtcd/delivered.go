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
// so a restart never repeats a delivery.
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

// deliveryLine is one line of the delivered journal.
type deliveryLine struct {
	ID     string `json:"id"`
	Device string `json:"device"`
	At     uint32 `json:"at"`
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
		if dl.At >= cutoff {
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
	dev, err := envelope.EncodeAddress(dl.Device)
	if err != nil {
		return deliveryKey{}, err
	}
	return deliveryKey{id: envelope.IDFromBytes(b), device: dev}, nil
}

// mark records a delivery at now, returning false if it already happened.
// A journal failure is logged, not fatal: the cost is at most one repeat
// delivery after a restart.
func (d *deliveredTable) mark(k deliveryKey, now uint32) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, done := d.done[k]; done {
		return false
	}
	d.done[k] = now
	if d.j != nil {
		dev, _ := k.device.Text()
		if err := d.j.Append(deliveryLine{ID: k.id.String(), Device: dev, At: now}); err != nil {
			d.log.Warn("delivered journal append failed", "err", err)
		}
	}
	return true
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
			dev, _ := k.device.Text()
			if err := emit(deliveryLine{ID: k.id.String(), Device: dev, At: at}); err != nil {
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
