package store

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"github.com/jancona/qtc/envelope"
)

// FileStore is a MemStore whose contents survive a restart: every stored
// record is appended to a JSON Lines journal before Put reports success, and
// the journal is replayed when the store opens. Expired records stay in the
// file until it is compacted, which happens on open and from Expire once dead
// lines outnumber live ones.
type FileStore struct {
	mem *MemStore
	log *slog.Logger

	mu sync.Mutex // serializes writes so a record is journaled before it is visible
	j  *Journal
}

// journalRecord is one line of a FileStore journal.
type journalRecord struct {
	Callsign   string `json:"callsign"`
	ReceivedAt uint32 `json:"received_at"`
	Expiry     uint32 `json:"expiry"`
	Env        string `json:"env"` // base64, as on the wire
}

// compactSlack keeps small journals from being rewritten over a handful of
// expired lines.
const compactSlack = 256

// OpenFileStore opens the journal at path, replays every record that has not
// expired at now into mem, and compacts the file. mem supplies the in-memory
// settings (MaxPerCallsign) and should be empty. log may be nil.
func OpenFileStore(path string, mem *MemStore, now uint32, log *slog.Logger) (*FileStore, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &FileStore{mem: mem, log: log}
	replayed, expired := 0, 0
	j, bad, err := OpenJournal(path, func(line []byte) error {
		rec, err := decodeRecord(line)
		if err != nil {
			return err
		}
		if rec.Expiry <= now {
			expired++
			return nil
		}
		if _, err := mem.Put(rec); err != nil {
			return err
		}
		replayed++
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.j = j
	if bad > 0 {
		log.Warn("mailbox journal: skipped unreadable lines", "path", path, "n", bad)
	}
	log.Info("mailbox journal replayed", "path", path, "records", replayed, "expired", expired)
	if err := s.compact(); err != nil {
		j.Close()
		return nil, err
	}
	return s, nil
}

// Put implements Store. A record is visible to Query only once it is in the
// journal; a journal failure refuses the PUT rather than acknowledge a
// record a restart would lose.
func (s *FileStore) Put(rec Record) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if dup, err := s.mem.check(rec); dup || err != nil {
		return false, err
	}
	if err := s.j.Append(encodeRecord(rec)); err != nil {
		return false, err
	}
	return s.mem.Put(rec)
}

// Query implements Store.
func (s *FileStore) Query(callsign envelope.Address, since uint32, limit int, types []envelope.PacketType) ([]Record, uint32, bool, error) {
	return s.mem.Query(callsign, since, limit, types)
}

// Expire implements Store, compacting the journal when it has grown to more
// than twice the live records.
func (s *FileStore) Expire(now uint32) int {
	n := s.mem.Expire(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.j.Lines() > 2*s.mem.Len()+compactSlack {
		if err := s.compact(); err != nil {
			s.log.Warn("mailbox journal compaction failed", "err", err)
		}
	}
	return n
}

// Len reports the number of stored records.
func (s *FileStore) Len() int { return s.mem.Len() }

// Close closes the journal.
func (s *FileStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.j.Close()
}

// compact rewrites the journal with only the live records. Callers hold s.mu
// or have exclusive access.
func (s *FileStore) compact() error {
	return s.j.Rewrite(func(emit func(any) error) error {
		for _, rec := range s.mem.all() {
			if err := emit(encodeRecord(rec)); err != nil {
				return err
			}
		}
		return nil
	})
}

func encodeRecord(rec Record) journalRecord {
	return journalRecord{
		Callsign:   addrText(rec.Callsign),
		ReceivedAt: rec.ReceivedAt,
		Expiry:     rec.Expiry,
		Env:        base64.StdEncoding.EncodeToString(rec.Env.Bytes()),
	}
}

func decodeRecord(line []byte) (Record, error) {
	var jr journalRecord
	if err := json.Unmarshal(line, &jr); err != nil {
		return Record{}, err
	}
	call, err := parseAddrText(jr.Callsign)
	if err != nil {
		return Record{}, err
	}
	b, err := base64.StdEncoding.DecodeString(jr.Env)
	if err != nil {
		return Record{}, fmt.Errorf("store: journal envelope: %w", err)
	}
	e, err := envelope.Parse(b)
	if err != nil {
		return Record{}, err
	}
	return Record{Callsign: call, Env: e, ReceivedAt: jr.ReceivedAt, Expiry: jr.Expiry}, nil
}

// addrText writes a mailbox key readably: callsign text, "#NAME" for a room,
// or hex for anything else.
func addrText(a envelope.Address) string {
	if name, ok := a.RoomName(); ok {
		return "#" + name
	}
	if t, err := a.Text(); err == nil {
		return t
	}
	return fmt.Sprintf("0x%012X", uint64(a))
}

func parseAddrText(s string) (envelope.Address, error) {
	switch {
	case strings.HasPrefix(s, "#"):
		return envelope.RoomAddress(s[1:])
	case strings.HasPrefix(s, "0x"):
		v, err := strconv.ParseUint(s[2:], 16, 48)
		return envelope.Address(v), err
	default:
		return envelope.EncodeAddress(s)
	}
}
