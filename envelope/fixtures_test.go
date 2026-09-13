package envelope_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jancona/pigeon/envelope"
)

// fixtures mirrors docs/pigeon-fixtures.json. Sections the envelope package
// does not implement are left out.
type fixtures struct {
	Version     int               `json:"pigeon_fixtures_version"`
	PacketTypes map[string]int    `json:"packet_types"`
	Flags       map[string]int    `json:"flags"`
	RcptStatus  map[string]int    `json:"rcpt_status"`
	RoomOps     map[string]int    `json:"room_ops"`
	Addresses   []addressFixture  `json:"addresses"`
	Special     map[string]string `json:"special_addresses"`
	Rooms       roomsFixture      `json:"rooms"`
	TestKey     keyFixture        `json:"test_key"`
	Envelopes   []packetFixture   `json:"envelopes"`
	RoomPackets []packetFixture   `json:"room_packets"`
	SMSWrap     smsFixture        `json:"sms_wrap"`
	Invalid     []invalidFixture  `json:"invalid_envelopes"`
}

type addressFixture struct {
	Text         string `json:"text"`
	Value        uint64 `json:"value"`
	Hex          string `json:"hex"`
	BaseCallsign string `json:"base_callsign"`
	DeviceSuffix string `json:"device_suffix"`
}

type roomsFixture struct {
	Valid []struct {
		Name      string  `json:"name"`
		Canonical string  `json:"canonical"`
		Value     uint64  `json:"value"`
		Hex       string  `json:"hex"`
		DecodesTo *string `json:"decodes_to"`
	} `json:"valid"`
	InvalidNames     []string `json:"invalid_names"`
	InvalidAddresses []struct {
		Hex       string  `json:"hex"`
		Reason    string  `json:"reason"`
		DecodesTo *string `json:"decodes_to"`
	} `json:"invalid_addresses"`
	Range map[string]string `json:"range"`
}

type keyFixture struct {
	Curve              string `json:"curve"`
	PrivateD           string `json:"private_d"`
	PublicUncompressed string `json:"public_uncompressed"`
}

// packetFixture covers MSG, RCPT, and ROOM; fields not present for a type
// are left at their zero values.
type packetFixture struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Type        string `json:"type"`
	Bytes       string `json:"bytes"`
	Length      int    `json:"length"`
	Fields      struct {
		Version     int      `json:"version"`
		Flags       int      `json:"flags"`
		Source      string   `json:"source"`
		Destination string   `json:"destination"`
		Timestamp   uint32   `json:"timestamp"`
		TTLMinutes  uint16   `json:"ttl_minutes"`
		Nonce       uint16   `json:"nonce"`
		Body        string   `json:"body"`
		MessageID   string   `json:"message_id"`
		Status      int      `json:"status"`
		LastHeard   uint32   `json:"last_heard"`
		Note        string   `json:"note"`
		Op          int      `json:"op"`
		Count       int      `json:"count"`
		Rooms       []string `json:"rooms"`
	} `json:"fields"`
	SigningInput string  `json:"signing_input"`
	MessageID    string  `json:"message_id"`
	Expiry       *uint32 `json:"expiry"`
	Signature    string  `json:"signature"`
	Digest       string  `json:"digest"`
}

type smsFixture struct {
	SMSPacket      string `json:"sms_packet"`
	LSFSource      string `json:"lsf_source"`
	LSFDestination string `json:"lsf_destination"`
	NodeTime       uint32 `json:"node_time"`
	NodeDefaultTTL uint16 `json:"node_default_ttl_minutes"`
	Nonce          uint16 `json:"nonce"`
	Envelope       string `json:"envelope"`
	MessageID      string `json:"message_id"`
	Unwrap         struct {
		SMSPacket string `json:"sms_packet"`
	} `json:"unwrap"`
}

type invalidFixture struct {
	Name   string `json:"name"`
	Bytes  string `json:"bytes"`
	Reason string `json:"reason"`
}

func loadFixtures(t *testing.T) *fixtures {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "docs", "pigeon-fixtures.json"))
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	var fx fixtures
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatalf("parse fixtures: %v", err)
	}
	if fx.Version != 2 {
		t.Fatalf("fixtures version %d, test written for 2", fx.Version)
	}
	return &fx
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

func addr(t *testing.T, hexAddr string) envelope.Address {
	t.Helper()
	return envelope.AddressFromBytes(unhex(t, hexAddr))
}

func addrHex(a envelope.Address) string {
	b := a.Bytes()
	return hex.EncodeToString(b[:])
}

func testKey(t *testing.T, k keyFixture) *ecdsa.PrivateKey {
	t.Helper()
	if k.Curve != "secp256r1" {
		t.Fatalf("test key curve %q", k.Curve)
	}
	priv, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), unhex(t, k.PrivateD))
	if err != nil {
		t.Fatalf("ParseRawPrivateKey: %v", err)
	}
	pub, err := priv.PublicKey.Bytes()
	if err != nil {
		t.Fatalf("PublicKey.Bytes: %v", err)
	}
	if got := hex.EncodeToString(pub); got != k.PublicUncompressed {
		t.Fatalf("test key public = %s, fixture %s", got, k.PublicUncompressed)
	}
	return priv
}

func TestFixturesConstants(t *testing.T) {
	fx := loadFixtures(t)
	check := func(section, name string, got, want int) {
		t.Helper()
		if got != want {
			t.Errorf("%s.%s = %d, fixture %d", section, name, got, want)
		}
	}
	check("packet_types", "MSG", int(envelope.TypeMSG), fx.PacketTypes["MSG"])
	check("packet_types", "RCPT", int(envelope.TypeRCPT), fx.PacketTypes["RCPT"])
	check("packet_types", "ROOM", int(envelope.TypeROOM), fx.PacketTypes["ROOM"])
	check("flags", "SIGNED", int(envelope.FlagSigned), fx.Flags["SIGNED"])
	check("flags", "RCPT_REQ", int(envelope.FlagRcptReq), fx.Flags["RCPT_REQ"])
	check("rcpt_status", "QUEUED", int(envelope.StatusQueued), fx.RcptStatus["QUEUED"])
	check("rcpt_status", "TRANSMITTED", int(envelope.StatusTransmitted), fx.RcptStatus["TRANSMITTED"])
	check("rcpt_status", "DELIVERED", int(envelope.StatusDelivered), fx.RcptStatus["DELIVERED"])
	check("rcpt_status", "EXPIRED", int(envelope.StatusExpired), fx.RcptStatus["EXPIRED"])
	check("rcpt_status", "REJECTED", int(envelope.StatusRejected), fx.RcptStatus["REJECTED"])
	check("room_ops", "JOIN", int(envelope.OpJoin), fx.RoomOps["JOIN"])
	check("room_ops", "LEAVE", int(envelope.OpLeave), fx.RoomOps["LEAVE"])
	check("room_ops", "LIST", int(envelope.OpList), fx.RoomOps["LIST"])
	check("room_ops", "OK", int(envelope.OpOK), fx.RoomOps["OK"])
	check("room_ops", "REFUSED", int(envelope.OpRefused), fx.RoomOps["REFUSED"])
}

func TestFixturesAddresses(t *testing.T) {
	fx := loadFixtures(t)
	for _, f := range fx.Addresses {
		t.Run(f.Text, func(t *testing.T) {
			a, err := envelope.EncodeAddress(f.Text)
			if err != nil {
				t.Fatalf("EncodeAddress: %v", err)
			}
			if uint64(a) != f.Value {
				t.Errorf("value = %d, want %d", uint64(a), f.Value)
			}
			if got := addrHex(a); got != f.Hex {
				t.Errorf("hex = %s, want %s", got, f.Hex)
			}
			if got := addr(t, f.Hex); got != a {
				t.Errorf("AddressFromBytes = %d, want %d", uint64(got), uint64(a))
			}
			text, err := a.Text()
			if err != nil {
				t.Fatalf("Text: %v", err)
			}
			if text != f.Text {
				t.Errorf("Text = %q, want %q", text, f.Text)
			}
			if a.String() != f.Text {
				t.Errorf("String = %q, want %q", a.String(), f.Text)
			}
			// base_callsign is the mechanical rule only. K1XYZ  R is a node
			// callsign that callers must use whole; that policy is theirs,
			// not this package's (fixtures note, architecture §3).
			base, suffix := envelope.SplitCallsign(text)
			if base != f.BaseCallsign || suffix != f.DeviceSuffix {
				t.Errorf("SplitCallsign = %q, %q; want %q, %q", base, suffix, f.BaseCallsign, f.DeviceSuffix)
			}
			if a.BaseCallsign() != f.BaseCallsign {
				t.Errorf("BaseCallsign = %q, want %q", a.BaseCallsign(), f.BaseCallsign)
			}
			if !a.IsStandard() || a.IsRoom() || a.IsExtended() {
				t.Errorf("range predicates wrong for a callsign")
			}
		})
	}

	t.Run("special", func(t *testing.T) {
		want := map[string]envelope.Address{
			"reserved_zero": envelope.AddressZero,
			"standard_max":  envelope.StandardMax,
			"extended_base": envelope.ExtendedBase,
			"extended_end":  envelope.ExtendedEnd,
			"broadcast":     envelope.Broadcast,
		}
		for name, a := range want {
			if got := addrHex(a); got != fx.Special[name] {
				t.Errorf("%s = %s, fixture %s", name, got, fx.Special[name])
			}
		}
		if _, err := envelope.AddressZero.Text(); err == nil {
			t.Error("zero address decoded as a callsign")
		}
		if envelope.Broadcast.String() != "@ALL" {
			t.Errorf("Broadcast.String = %q", envelope.Broadcast.String())
		}
		// trailing_space_note: padding spaces do not change the value.
		abc, _ := envelope.EncodeAddress("ABC")
		for _, s := range []string{"ABC ", "ABC      "} {
			if a, err := envelope.EncodeAddress(s); err != nil || a != abc {
				t.Errorf("EncodeAddress(%q) = %d, %v; want %d", s, uint64(a), err, uint64(abc))
			}
		}
		if !strings.HasSuffix(fx.Special["trailing_space_note"], addrHex(abc)) {
			t.Errorf("trailing_space_note does not end with %s", addrHex(abc))
		}
	})
}

func TestFixturesRooms(t *testing.T) {
	fx := loadFixtures(t)
	for _, f := range fx.Rooms.Valid {
		t.Run(f.Name, func(t *testing.T) {
			a, err := envelope.RoomAddress(f.Name)
			if err != nil {
				t.Fatalf("RoomAddress: %v", err)
			}
			if uint64(a) != f.Value || addrHex(a) != f.Hex {
				t.Errorf("RoomAddress = %d/%s, want %d/%s", uint64(a), addrHex(a), f.Value, f.Hex)
			}
			name, ok := a.RoomName()
			if !ok || f.DecodesTo == nil || name != *f.DecodesTo || name != f.Canonical {
				t.Errorf("RoomName = %q, %v; want %q", name, ok, f.Canonical)
			}
			if !a.IsRoom() || !a.IsExtended() || a.IsStandard() {
				t.Error("range predicates wrong for a room")
			}
			if a.String() != "#"+f.Canonical {
				t.Errorf("String = %q", a.String())
			}
		})
	}
	for _, name := range fx.Rooms.InvalidNames {
		if a, err := envelope.RoomAddress(name); err == nil {
			t.Errorf("RoomAddress(%q) = %d, want error", name, uint64(a))
		}
	}
	for _, f := range fx.Rooms.InvalidAddresses {
		a := addr(t, f.Hex)
		if f.DecodesTo != nil {
			t.Fatalf("fixture %s (%s) has a non-null decodes_to", f.Hex, f.Reason)
		}
		if name, ok := a.RoomName(); ok || a.IsRoom() {
			t.Errorf("%s (%s): RoomName = %q, want invalid", f.Hex, f.Reason, name)
		}
	}
	rng := fx.Rooms.Range
	if got := addrHex(envelope.RoomFirst); got != rng["first_valid"] {
		t.Errorf("RoomFirst = %s, fixture %s", got, rng["first_valid"])
	}
	if got := addrHex(envelope.RoomLast); got != rng["last_valid"] {
		t.Errorf("RoomLast = %s, fixture %s", got, rng["last_valid"])
	}
	if got := addrHex(envelope.ReservedRooms); got != rng["first_reserved_by_rooms_spec"] {
		t.Errorf("ReservedRooms = %s, fixture %s", got, rng["first_reserved_by_rooms_spec"])
	}
}

func TestFixturesEnvelopes(t *testing.T) {
	fx := loadFixtures(t)
	priv := testKey(t, fx.TestKey)
	for _, f := range fx.Envelopes {
		t.Run(f.Name, func(t *testing.T) {
			raw := unhex(t, f.Bytes)
			if len(raw) != f.Length {
				t.Fatalf("fixture length %d but %d bytes", f.Length, len(raw))
			}
			e, err := envelope.Parse(raw)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			checkCommon(t, e, f, raw)
			switch f.Type {
			case "MSG":
				checkMsg(t, e, f)
			case "RCPT":
				checkRcpt(t, e, f)
			default:
				t.Fatalf("unexpected type %q in envelopes", f.Type)
			}
			checkSignature(t, e, f, priv)
			checkJSON(t, e, raw)
			if s := e.String(); !strings.HasPrefix(s, f.Type+" ") {
				t.Errorf("String = %q", s)
			}
		})
	}
}

func checkCommon(t *testing.T, e *envelope.Envelope, f packetFixture, raw []byte) {
	t.Helper()
	if e.Type().String() != f.Type {
		t.Errorf("Type = %s, want %s", e.Type(), f.Type)
	}
	if e.Len() != len(raw) || !bytes.Equal(e.Bytes(), raw) {
		t.Error("Bytes differs from input")
	}
	// Bytes is a copy; mutating it must not touch the envelope.
	b := e.Bytes()
	b[0] ^= 0xFF
	if !bytes.Equal(e.Bytes(), raw) {
		t.Error("Bytes shares storage with the envelope")
	}
	if int(e.Version()) != f.Fields.Version {
		t.Errorf("Version = %d, want %d", e.Version(), f.Fields.Version)
	}
	if int(e.Flags()) != f.Fields.Flags {
		t.Errorf("Flags = 0x%02X, want 0x%02X", e.Flags(), f.Fields.Flags)
	}
	if e.Timestamp() != f.Fields.Timestamp {
		t.Errorf("Timestamp = %d, want %d", e.Timestamp(), f.Fields.Timestamp)
	}
	if f.Type != "ROOM" {
		if got := addrHex(e.Source()); got != f.Fields.Source {
			t.Errorf("Source = %s, want %s", got, f.Fields.Source)
		}
		if got := addrHex(e.Destination()); got != f.Fields.Destination {
			t.Errorf("Destination = %s, want %s", got, f.Fields.Destination)
		}
	}
	if f.SigningInput != "" {
		if got := hex.EncodeToString(e.SigningInput()); got != f.SigningInput {
			t.Errorf("SigningInput = %s, want %s", got, f.SigningInput)
		}
	}
}

func checkMsg(t *testing.T, e *envelope.Envelope, f packetFixture) {
	t.Helper()
	m, ok := e.Msg()
	if !ok {
		t.Fatal("Msg view not available")
	}
	if _, ok := e.Rcpt(); ok {
		t.Error("Rcpt view available on a MSG")
	}
	if m.TTL() != f.Fields.TTLMinutes {
		t.Errorf("TTL = %d, want %d", m.TTL(), f.Fields.TTLMinutes)
	}
	if m.Nonce() != f.Fields.Nonce {
		t.Errorf("Nonce = %d, want %d", m.Nonce(), f.Fields.Nonce)
	}
	if m.Body() != f.Fields.Body {
		t.Errorf("Body = %q, want %q", m.Body(), f.Fields.Body)
	}
	if m.RcptReq() != (f.Fields.Flags&int(envelope.FlagRcptReq) != 0) {
		t.Error("RcptReq wrong")
	}
	if got := e.ID().String(); got != f.MessageID {
		t.Errorf("ID = %s, want %s", got, f.MessageID)
	}
	exp, ok := m.Expiry()
	switch {
	case f.Expiry == nil && ok:
		t.Errorf("Expiry = %d, fixture says the envelope does not determine it", exp)
	case f.Expiry != nil && (!ok || exp != *f.Expiry):
		t.Errorf("Expiry = %d, %v; want %d", exp, ok, *f.Expiry)
	}

	// The Build path must reproduce the unsigned bytes exactly, and the
	// signed fixtures once their signature is stripped.
	built, err := envelope.BuildMsg(addr(t, f.Fields.Source), addr(t, f.Fields.Destination),
		f.Fields.Timestamp, f.Fields.TTLMinutes, f.Fields.Nonce, byte(f.Fields.Flags)&^envelope.FlagSigned, f.Fields.Body)
	if err != nil {
		t.Fatalf("BuildMsg: %v", err)
	}
	stripped := e.StripSignature()
	if !bytes.Equal(built.Bytes(), stripped.Bytes()) {
		t.Errorf("BuildMsg bytes differ from the fixture (unsigned form):\n got %x\nwant %x", built.Bytes(), stripped.Bytes())
	}
	if built.ID() != e.ID() || stripped.ID() != e.ID() {
		t.Error("ID differs between built, stripped, and parsed forms")
	}
	if stripped.Signed() {
		t.Error("StripSignature left SIGNED set")
	}
}

func checkRcpt(t *testing.T, e *envelope.Envelope, f packetFixture) {
	t.Helper()
	r, ok := e.Rcpt()
	if !ok {
		t.Fatal("Rcpt view not available")
	}
	if _, ok := e.Msg(); ok {
		t.Error("Msg view available on a RCPT")
	}
	if got := r.MessageID().String(); got != f.Fields.MessageID {
		t.Errorf("MessageID = %s, want %s", got, f.Fields.MessageID)
	}
	if int(r.Status()) != f.Fields.Status {
		t.Errorf("Status = %d, want %d", r.Status(), f.Fields.Status)
	}
	if r.LastHeard() != f.Fields.LastHeard {
		t.Errorf("LastHeard = %d, want %d", r.LastHeard(), f.Fields.LastHeard)
	}
	if r.Note() != f.Fields.Note {
		t.Errorf("Note = %q, want %q", r.Note(), f.Fields.Note)
	}
	if e.ID() != (envelope.ID{}) {
		t.Error("a RCPT has no ID of its own")
	}
	built, err := envelope.BuildRcpt(addr(t, f.Fields.Source), addr(t, f.Fields.Destination),
		envelope.IDFromBytes(unhex(t, f.Fields.MessageID)), envelope.Status(f.Fields.Status),
		f.Fields.Timestamp, f.Fields.LastHeard, f.Fields.Note)
	if err != nil {
		t.Fatalf("BuildRcpt: %v", err)
	}
	if !bytes.Equal(built.Bytes(), e.StripSignature().Bytes()) {
		t.Errorf("BuildRcpt bytes differ from the fixture (unsigned form):\n got %x\nwant %x", built.Bytes(), e.StripSignature().Bytes())
	}
}

// checkSignature verifies the fixture signature against the test key (the
// bytes are not reproduced: signing is randomized) and round-trips a fresh
// signature over the same envelope.
func checkSignature(t *testing.T, e *envelope.Envelope, f packetFixture, priv *ecdsa.PrivateKey) {
	t.Helper()
	if f.Signature == "" {
		if _, ok := e.Signature(); ok || e.Signed() {
			t.Error("unsigned fixture reports a signature")
		}
		if err := e.Verify(&priv.PublicKey); !errors.Is(err, envelope.ErrUnsigned) {
			t.Errorf("Verify on unsigned = %v, want ErrUnsigned", err)
		}
	} else {
		sig, ok := e.Signature()
		if !ok || hex.EncodeToString(sig) != f.Signature {
			t.Errorf("Signature = %x, %v; want %s", sig, ok, f.Signature)
		}
		if d := sha256.Sum256(e.SigningInput()); hex.EncodeToString(d[:]) != f.Digest {
			t.Errorf("digest = %x, want %s", d, f.Digest)
		}
		if err := e.Verify(&priv.PublicKey); err != nil {
			t.Errorf("fixture signature does not verify: %v", err)
		}
		if _, err := e.Sign(priv); !errors.Is(err, envelope.ErrAlreadySigned) {
			t.Errorf("Sign on signed = %v, want ErrAlreadySigned", err)
		}
		if err := e.StripSignature().Verify(&priv.PublicKey); !errors.Is(err, envelope.ErrUnsigned) {
			t.Errorf("Verify after strip = %v, want ErrUnsigned", err)
		}
	}

	// Fresh signature round trip. A body over MaxBodySigned cannot be signed.
	unsigned := e.StripSignature()
	signed, err := unsigned.Sign(priv)
	if unsigned.Len()+envelope.SignatureLen > envelope.MaxPayload {
		if err == nil {
			t.Error("Sign accepted a payload too long for a signature")
		}
		return
	}
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if !signed.Signed() || signed.Len() != unsigned.Len()+envelope.SignatureLen {
		t.Error("Sign did not append a 64-byte signature")
	}
	if signed.ID() != unsigned.ID() {
		t.Error("signing changed the ID")
	}
	if !bytes.Equal(signed.SigningInput(), unsigned.SigningInput()) {
		t.Error("signing changed the signing input")
	}
	if !bytes.Equal(unsigned.Bytes(), e.StripSignature().Bytes()) {
		t.Error("Sign mutated its receiver")
	}
	if err := signed.Verify(&priv.PublicKey); err != nil {
		t.Errorf("fresh signature does not verify: %v", err)
	}
	if !bytes.Equal(signed.StripSignature().Bytes(), unsigned.Bytes()) {
		t.Error("strip after sign is not the original")
	}
	other, err := ecdsa.GenerateKey(elliptic.P256(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := signed.Verify(&other.PublicKey); !errors.Is(err, envelope.ErrBadSignature) {
		t.Errorf("Verify with wrong key = %v, want ErrBadSignature", err)
	}
	tampered := signed.Bytes()
	tampered[len(tampered)-envelope.SignatureLen-1] ^= 0x01 // last byte of body/note, or the header
	if te, err := envelope.Parse(tampered); err != nil {
		t.Fatalf("Parse tampered: %v", err)
	} else if err := te.Verify(&priv.PublicKey); !errors.Is(err, envelope.ErrBadSignature) {
		t.Errorf("Verify tampered = %v, want ErrBadSignature", err)
	}
}

func checkJSON(t *testing.T, e *envelope.Envelope, raw []byte) {
	t.Helper()
	j, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	var back envelope.Envelope
	if err := json.Unmarshal(j, &back); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if !bytes.Equal(back.Bytes(), raw) {
		t.Error("JSON round trip changed the bytes")
	}
	if back.ID() != e.ID() {
		t.Error("JSON round trip changed the ID")
	}
	// A struct field works the same way.
	var wrapped struct {
		E *envelope.Envelope `json:"envelope"`
	}
	if err := json.Unmarshal([]byte(`{"envelope":`+string(j)+`}`), &wrapped); err != nil || !bytes.Equal(wrapped.E.Bytes(), raw) {
		t.Errorf("wrapped JSON round trip: %v", err)
	}
}

func TestFixturesRoomPackets(t *testing.T) {
	fx := loadFixtures(t)
	for _, f := range fx.RoomPackets {
		t.Run(f.Name, func(t *testing.T) {
			raw := unhex(t, f.Bytes)
			if len(raw) != f.Length {
				t.Fatalf("fixture length %d but %d bytes", f.Length, len(raw))
			}
			e, err := envelope.Parse(raw)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			checkCommon(t, e, f, raw)
			r, ok := e.Room()
			if !ok {
				t.Fatal("Room view not available")
			}
			if int(r.Op()) != f.Fields.Op {
				t.Errorf("Op = %d, want %d", r.Op(), f.Fields.Op)
			}
			if r.Count() != f.Fields.Count || len(r.Rooms()) != f.Fields.Count {
				t.Errorf("Count = %d, want %d", r.Count(), f.Fields.Count)
			}
			rooms := r.Rooms()
			for i, want := range f.Fields.Rooms {
				if i < len(rooms) && addrHex(rooms[i]) != want {
					t.Errorf("Rooms[%d] = %s, want %s", i, addrHex(rooms[i]), want)
				}
			}
			if r.Note() != f.Fields.Note {
				t.Errorf("Note = %q, want %q", r.Note(), f.Fields.Note)
			}
			if e.Source() != envelope.AddressZero || e.Destination() != envelope.AddressZero || e.ID() != (envelope.ID{}) {
				t.Error("ROOM has no addresses or ID")
			}
			if e.SigningInput() != nil {
				t.Error("ROOM has no signing input")
			}
			if _, err := e.Sign(nil); !errors.Is(err, envelope.ErrNotSignable) {
				t.Errorf("Sign on ROOM = %v, want ErrNotSignable", err)
			}
			built, err := envelope.BuildRoom(envelope.RoomOp(f.Fields.Op), f.Fields.Timestamp, rooms, f.Fields.Note)
			if err != nil {
				t.Fatalf("BuildRoom: %v", err)
			}
			if !bytes.Equal(built.Bytes(), raw) {
				t.Errorf("BuildRoom bytes differ:\n got %x\nwant %x", built.Bytes(), raw)
			}
			checkJSON(t, e, raw)
			if s := e.String(); !strings.HasPrefix(s, "ROOM "+r.Op().String()) {
				t.Errorf("String = %q", s)
			}
		})
	}
}

func TestFixturesSMSWrap(t *testing.T) {
	fx := loadFixtures(t)
	f := fx.SMSWrap
	e, err := envelope.FromSMS(unhex(t, f.SMSPacket), addr(t, f.LSFSource), addr(t, f.LSFDestination),
		f.NodeTime, f.NodeDefaultTTL, f.Nonce)
	if err != nil {
		t.Fatalf("FromSMS: %v", err)
	}
	if got := hex.EncodeToString(e.Bytes()); got != f.Envelope {
		t.Errorf("FromSMS = %s, want %s", got, f.Envelope)
	}
	if got := e.ID().String(); got != f.MessageID {
		t.Errorf("ID = %s, want %s", got, f.MessageID)
	}

	// Egress: the unwrap fixture is msg_basic rendered as SMS.
	var basic *envelope.Envelope
	for _, p := range fx.Envelopes {
		if p.Name == "msg_basic" {
			basic, err = envelope.Parse(unhex(t, p.Bytes))
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if basic == nil {
		t.Fatal("msg_basic fixture missing")
	}
	sms, err := envelope.ToSMS(basic)
	if err != nil {
		t.Fatalf("ToSMS: %v", err)
	}
	if got := hex.EncodeToString(sms); got != f.Unwrap.SMSPacket {
		t.Errorf("ToSMS = %s, want %s", got, f.Unwrap.SMSPacket)
	}
	if _, err := envelope.ToSMS(e.StripSignature()); err != nil {
		t.Errorf("ToSMS of an unsigned MSG: %v", err)
	}
	if _, err := envelope.FromSMS([]byte{0x08}, 1, 2, 0, 0, 0); err == nil {
		t.Error("FromSMS accepted a non-SMS type byte")
	}
}

func TestFixturesInvalidEnvelopes(t *testing.T) {
	fx := loadFixtures(t)
	for _, f := range fx.Invalid {
		t.Run(f.Name, func(t *testing.T) {
			raw := unhex(t, f.Bytes)
			e, err := envelope.Parse(raw)
			if err == nil {
				t.Fatalf("Parse accepted %s (%s): %s", f.Name, f.Reason, e)
			}
			if !errors.Is(err, envelope.ErrInvalid) && !errors.Is(err, envelope.ErrUnknownVersion) {
				t.Errorf("error %v does not wrap ErrInvalid or ErrUnknownVersion", err)
			}
			if f.Name == "unknown_version" && !errors.Is(err, envelope.ErrUnknownVersion) {
				t.Errorf("unknown_version error = %v, want ErrUnknownVersion", err)
			}
			var viaJSON envelope.Envelope
			j, _ := json.Marshal(base64.StdEncoding.EncodeToString(raw))
			if err := json.Unmarshal(j, &viaJSON); err == nil {
				t.Error("UnmarshalJSON accepted an invalid envelope")
			}
		})
	}
}
