package qtcd

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"testing"

	"github.com/libp2p/go-libp2p/core/peer"
)

func testKeyAndID(t *testing.T) (*ecdsa.PrivateKey, peer.ID) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id, err := PeerIDFromKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return k, id
}

func TestMailboxRecordSignVerifyJSON(t *testing.T) {
	homeKey, home := testKeyAndID(t)
	_, m1 := testKeyAndID(t)
	_, m2 := testKeyAndID(t)
	rec := &MailboxRecord{Callsign: mustAddr(t, "N1ADJ  H"), Version: 3, HomeStation: home, Members: []peer.ID{m1, m2}, K: 2, Policy: PolicyProvisional, Updated: t0}
	if err := rec.Sign(homeKey); err != nil {
		t.Fatal(err)
	}
	if rec.Writer != home || len(rec.Sig) != 64 || !rec.Provisional() {
		t.Errorf("signed record = %+v", rec)
	}
	if err := rec.Verify(); err != nil {
		t.Fatal(err)
	}
	// JSON round trip keeps everything, including the base callsign.
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	var back MailboxRecord
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Callsign != mustAddr(t, "N1ADJ") || back.Version != 3 || back.HomeStation != home || !equalMembers(back.Members, rec.Members) || back.K != 2 || back.Policy != PolicyProvisional || back.Updated != t0 {
		t.Errorf("round trip = %+v", back)
	}
	if err := back.Verify(); err != nil {
		t.Errorf("round-tripped record does not verify: %v", err)
	}
	if RecordKey(back.Callsign) != "/qtc/0/mailbox/N1ADJ" {
		t.Errorf("key = %s", RecordKey(back.Callsign))
	}

	// Tampering with any signed field breaks verification.
	tampered := back
	tampered.Members = []peer.ID{m2, m1}
	if err := tampered.Verify(); !errors.Is(err, ErrRecordSignature) {
		t.Errorf("reordered members verified: %v", err)
	}
	tampered = back
	tampered.Version = 4
	if err := tampered.Verify(); !errors.Is(err, ErrRecordSignature) {
		t.Errorf("bumped version verified: %v", err)
	}
	// A writer_key that is not the writer's is rejected.
	otherKey, _ := testKeyAndID(t)
	forged := back
	if err := forged.Sign(otherKey); err != nil {
		t.Fatal(err)
	}
	forged.Writer = home
	if err := forged.Verify(); !errors.Is(err, ErrRecordWriter) {
		t.Errorf("forged writer verified: %v", err)
	}
}

func TestRecordValidator(t *testing.T) {
	homeKey, home := testKeyAndID(t)
	otherKey, other := testKeyAndID(t)
	_, m := testKeyAndID(t)
	cs := mustAddr(t, "W1AW")
	key := RecordKey(cs)
	mk := func(version uint64, k *ecdsa.PrivateKey) []byte {
		r := &MailboxRecord{Callsign: cs, Version: version, HomeStation: home, Members: []peer.ID{m}, K: 1, Updated: t0}
		if err := r.Sign(k); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(r)
		return b
	}
	v := recordValidator{}
	v1, v2home, v2other := mk(1, homeKey), mk(2, homeKey), mk(2, otherKey)
	if err := v.Validate(key, v1); err != nil {
		t.Errorf("Validate: %v", err)
	}
	if err := v.Validate(RecordKey(mustAddr(t, "N1ADJ")), v1); !errors.Is(err, ErrRecordKey) {
		t.Errorf("record under the wrong key accepted: %v", err)
	}
	if err := v.Validate(key, []byte("junk")); err == nil {
		t.Error("junk accepted")
	}
	if i, err := v.Select(key, [][]byte{v1, v2other, v2home}); err != nil || i != 2 {
		t.Errorf("Select = %d, %v; want 2 (highest version, written by its home station)", i, err)
	}
	if i, err := v.Select(key, [][]byte{v2other, v1}); err != nil || i != 0 {
		t.Errorf("Select = %d, %v; want 0", i, err)
	}
	if _, err := v.Select(key, [][]byte{[]byte("junk")}); err == nil {
		t.Error("Select accepted junk only")
	}
	_ = other
}
