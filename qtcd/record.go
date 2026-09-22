package qtcd

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"

	"github.com/jancona/qtc/envelope"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

// MailboxRecord names a callsign's home station and mailbox members (node
// protocol §4). It lives in the DHT under RecordKey and is announced on
// MailboxRecordsTopic when written.
type MailboxRecord struct {
	Callsign    envelope.Address
	Version     uint64
	HomeStation peer.ID
	Members     []peer.ID
	K           uint8
	Policy      uint16
	Updated     uint32
	Writer      peer.ID
	WriterKey   []byte // SubjectPublicKeyInfo DER
	Sig         []byte // raw r‖s
}

// Policy bits.
const (
	PolicySplitBySuffix uint16 = 1
	PolicyProvisional   uint16 = 2
)

// DHT key prefix and announcement topic.
const (
	RecordKeyPrefix     = "/qtc/0/mailbox/"
	MailboxRecordsTopic = "/qtc/0/mailbox-records"
)

// RecordKey is the DHT key for a base callsign.
func RecordKey(base envelope.Address) string { return RecordKeyPrefix + base.Base().String() }

// Errors from record handling.
var (
	ErrRecordSignature = errors.New("qtcd: mailbox record signature does not verify")
	ErrRecordWriter    = errors.New("qtcd: mailbox record writer_key does not match writer")
	ErrRecordKey       = errors.New("qtcd: mailbox record does not belong to this key")
	ErrRecordStale     = errors.New("qtcd: mailbox record version is not newer")
)

// Provisional reports whether the record was created on a sender's behalf
// (node protocol §7.3).
func (r *MailboxRecord) Provisional() bool { return r.Policy&PolicyProvisional != 0 }

// SigningInput is the canonical byte string the signature covers.
func (r *MailboxRecord) SigningInput() []byte {
	var b []byte
	cs := r.Callsign.Base().Bytes()
	b = append(b, cs[:]...)
	b = binary.BigEndian.AppendUint64(b, r.Version)
	b = appendID(b, r.HomeStation)
	b = append(b, byte(len(r.Members)))
	for _, m := range r.Members {
		b = appendID(b, m)
	}
	b = append(b, r.K)
	b = binary.BigEndian.AppendUint16(b, r.Policy)
	b = binary.BigEndian.AppendUint32(b, r.Updated)
	return appendID(b, r.Writer)
}

func appendID(b []byte, id peer.ID) []byte {
	raw := []byte(id)
	b = append(b, byte(len(raw)))
	return append(b, raw...)
}

// Sign sets Writer and WriterKey from key and signs the record.
func (r *MailboxRecord) Sign(key *ecdsa.PrivateKey) error {
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return fmt.Errorf("qtcd: record writer key: %w", err)
	}
	id, err := peerIDFromSPKI(der)
	if err != nil {
		return err
	}
	r.Writer, r.WriterKey = id, der
	digest := sha256.Sum256(r.SigningInput())
	rr, ss, err := ecdsa.Sign(nil, key, digest[:])
	if err != nil {
		return fmt.Errorf("qtcd: record sign: %w", err)
	}
	sig := make([]byte, 64)
	rr.FillBytes(sig[:32])
	ss.FillBytes(sig[32:])
	r.Sig = sig
	return nil
}

// Verify checks that WriterKey belongs to Writer and that Sig verifies.
func (r *MailboxRecord) Verify() error {
	id, err := peerIDFromSPKI(r.WriterKey)
	if err != nil {
		return err
	}
	if id != r.Writer {
		return ErrRecordWriter
	}
	pubAny, err := x509.ParsePKIXPublicKey(r.WriterKey)
	if err != nil {
		return fmt.Errorf("qtcd: record writer key: %w", err)
	}
	pub, ok := pubAny.(*ecdsa.PublicKey)
	if !ok || len(r.Sig) != 64 {
		return ErrRecordSignature
	}
	digest := sha256.Sum256(r.SigningInput())
	if !ecdsa.Verify(pub, digest[:], new(big.Int).SetBytes(r.Sig[:32]), new(big.Int).SetBytes(r.Sig[32:])) {
		return ErrRecordSignature
	}
	return nil
}

// peerIDFromSPKI derives the libp2p peer ID for an ECDSA public key in
// SubjectPublicKeyInfo DER, which is also libp2p's raw form for ECDSA keys.
func peerIDFromSPKI(der []byte) (peer.ID, error) {
	pk, err := crypto.UnmarshalECDSAPublicKey(der)
	if err != nil {
		return "", fmt.Errorf("qtcd: record writer key: %w", err)
	}
	return peer.IDFromPublicKey(pk)
}

type recordJSON struct {
	Callsign    string   `json:"callsign"`
	Version     uint64   `json:"version"`
	HomeStation string   `json:"home_station"`
	Members     []string `json:"members"`
	K           uint8    `json:"k"`
	Policy      uint16   `json:"policy"`
	Updated     uint32   `json:"updated"`
	Writer      string   `json:"writer"`
	WriterKey   string   `json:"writer_key"`
	Sig         string   `json:"sig"`
}

// MarshalJSON encodes the record in the node protocol's JSON form.
func (r *MailboxRecord) MarshalJSON() ([]byte, error) {
	j := recordJSON{
		Callsign: r.Callsign.Base().String(), Version: r.Version, HomeStation: r.HomeStation.String(),
		Members: make([]string, 0, len(r.Members)), K: r.K, Policy: r.Policy, Updated: r.Updated,
		Writer: r.Writer.String(), WriterKey: base64.StdEncoding.EncodeToString(r.WriterKey),
		Sig: base64.StdEncoding.EncodeToString(r.Sig),
	}
	for _, m := range r.Members {
		j.Members = append(j.Members, m.String())
	}
	return json.Marshal(j)
}

// UnmarshalJSON decodes the JSON form. It does not verify; call Verify.
func (r *MailboxRecord) UnmarshalJSON(b []byte) error {
	var j recordJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	cs, err := envelope.ParseAddress(j.Callsign)
	if err != nil {
		return fmt.Errorf("qtcd: record callsign: %w", err)
	}
	home, err := peer.Decode(j.HomeStation)
	if err != nil {
		return fmt.Errorf("qtcd: record home_station: %w", err)
	}
	writer, err := peer.Decode(j.Writer)
	if err != nil {
		return fmt.Errorf("qtcd: record writer: %w", err)
	}
	members := make([]peer.ID, 0, len(j.Members))
	for _, m := range j.Members {
		id, err := peer.Decode(m)
		if err != nil {
			return fmt.Errorf("qtcd: record member: %w", err)
		}
		members = append(members, id)
	}
	wk, err := base64.StdEncoding.DecodeString(j.WriterKey)
	if err != nil {
		return fmt.Errorf("qtcd: record writer_key: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(j.Sig)
	if err != nil {
		return fmt.Errorf("qtcd: record sig: %w", err)
	}
	*r = MailboxRecord{Callsign: cs, Version: j.Version, HomeStation: home, Members: members, K: j.K,
		Policy: j.Policy, Updated: j.Updated, Writer: writer, WriterKey: wk, Sig: sig}
	return nil
}

// parseRecord decodes and verifies a record and checks it belongs to key.
func parseRecord(key string, value []byte) (*MailboxRecord, error) {
	var r MailboxRecord
	if err := json.Unmarshal(value, &r); err != nil {
		return nil, fmt.Errorf("qtcd: mailbox record: %w", err)
	}
	if err := r.Verify(); err != nil {
		return nil, err
	}
	if RecordKey(r.Callsign) != key {
		return nil, ErrRecordKey
	}
	return &r, nil
}

// recordValidator is the DHT validator for the "qtc" namespace (node
// protocol §4). It checks one record at a time and selects the highest
// version, preferring the record written by its own home station on a tie.
type recordValidator struct{}

func (recordValidator) Validate(key string, value []byte) error {
	_, err := parseRecord(key, value)
	return err
}

func (recordValidator) Select(key string, values [][]byte) (int, error) {
	best, bestRec := -1, (*MailboxRecord)(nil)
	for i, v := range values {
		r, err := parseRecord(key, v)
		if err != nil {
			continue
		}
		if bestRec == nil || r.Version > bestRec.Version ||
			(r.Version == bestRec.Version && r.Writer == r.HomeStation && bestRec.Writer != bestRec.HomeStation) {
			best, bestRec = i, r
		}
	}
	if best < 0 {
		return 0, errors.New("qtcd: no valid mailbox record")
	}
	return best, nil
}

// hasMember reports whether id is in the member list.
func (r *MailboxRecord) hasMember(id peer.ID) bool {
	for _, m := range r.Members {
		if m == id {
			return true
		}
	}
	return false
}

// equalMembers reports whether two member lists are the same set.
func equalMembers(a, b []peer.ID) bool {
	if len(a) != len(b) {
		return false
	}
	for _, id := range a {
		found := false
		for _, o := range b {
			if id == o {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
