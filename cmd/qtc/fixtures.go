package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	"github.com/jancona/qtc/envelope"
)

// fixtureFile is the subset of docs/qtc-fixtures.json the checker reads.
type fixtureFile struct {
	Version   int `json:"qtc_fixtures_version"`
	Addresses []struct {
		Text         string `json:"text"`
		Hex          string `json:"hex"`
		BaseCallsign string `json:"base_callsign"`
		Value        uint64 `json:"value"`
	} `json:"addresses"`
	Rooms struct {
		Valid []struct {
			Name      string  `json:"name"`
			Hex       string  `json:"hex"`
			DecodesTo *string `json:"decodes_to"`
		} `json:"valid"`
		InvalidNames     []string `json:"invalid_names"`
		InvalidAddresses []struct {
			Hex string `json:"hex"`
		} `json:"invalid_addresses"`
	} `json:"rooms"`
	TestKey struct {
		PublicUncompressed string `json:"public_uncompressed"`
	} `json:"test_key"`
	Envelopes []struct {
		Name         string         `json:"name"`
		Type         string         `json:"type"`
		Bytes        string         `json:"bytes"`
		SigningInput string         `json:"signing_input"`
		MessageID    string         `json:"message_id"`
		Signature    string         `json:"signature"`
		Fields       map[string]any `json:"fields"`
	} `json:"envelopes"`
	RoomPackets []struct {
		Name   string `json:"name"`
		Bytes  string `json:"bytes"`
		Fields struct {
			Op    int      `json:"op"`
			Rooms []string `json:"rooms"`
		} `json:"fields"`
	} `json:"room_packets"`
	SMSWrap struct {
		SMSPacket      string `json:"sms_packet"`
		LSFSource      string `json:"lsf_source"`
		LSFDestination string `json:"lsf_destination"`
		Envelope       string `json:"envelope"`
		NodeTime       uint32 `json:"node_time"`
		NodeDefaultTTL uint16 `json:"node_default_ttl_minutes"`
		Nonce          uint16 `json:"nonce"`
		Unwrap         struct {
			SMSPacket string `json:"sms_packet"`
		} `json:"unwrap"`
	} `json:"sms_wrap"`
	Invalid []struct {
		Name  string `json:"name"`
		Bytes string `json:"bytes"`
	} `json:"invalid_envelopes"`
}

func runFixtures(args []string) error {
	path := "docs/qtc-fixtures.json"
	if len(args) > 0 {
		path = args[0]
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var fx fixtureFile
	if err := json.Unmarshal(b, &fx); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	pass, fail := checkFixtures(&fx, func(section, name, msg string) {
		fmt.Printf("FAIL %s/%s: %s\n", section, name, msg)
	})
	fmt.Printf("fixtures version %d: %d checks passed, %d failed\n", fx.Version, pass, fail)
	if fail > 0 {
		return fmt.Errorf("%d fixture checks failed", fail)
	}
	return nil
}

// checkFixtures runs every section and reports failures through report.
func checkFixtures(fx *fixtureFile, report func(section, name, msg string)) (pass, fail int) {
	check := func(section, name string, ok bool, msg string) {
		if ok {
			pass++
		} else {
			fail++
			report(section, name, msg)
		}
	}
	unhex := func(s string) []byte { b, _ := hex.DecodeString(s); return b }
	addrHex := func(a envelope.Address) string { b := a.Bytes(); return hex.EncodeToString(b[:]) }

	for _, f := range fx.Addresses {
		a, err := envelope.EncodeAddress(f.Text)
		check("addresses", f.Text, err == nil && uint64(a) == f.Value && addrHex(a) == f.Hex, fmt.Sprintf("encode = %d/%s, %v", uint64(a), addrHex(a), err))
		text, _ := a.Text()
		check("addresses", f.Text+" decode", text == f.Text && a.BaseCallsign() == f.BaseCallsign, fmt.Sprintf("decode %q base %q", text, a.BaseCallsign()))
	}
	for _, f := range fx.Rooms.Valid {
		a, err := envelope.RoomAddress(f.Name)
		name, ok := a.RoomName()
		check("rooms", f.Name, err == nil && addrHex(a) == f.Hex && ok && f.DecodesTo != nil && name == *f.DecodesTo, fmt.Sprintf("%s %q %v", addrHex(a), name, err))
	}
	for _, n := range fx.Rooms.InvalidNames {
		_, err := envelope.RoomAddress(n)
		check("rooms", fmt.Sprintf("invalid %q", n), err != nil, "accepted")
	}
	for _, f := range fx.Rooms.InvalidAddresses {
		check("rooms", "invalid "+f.Hex, !envelope.AddressFromBytes(unhex(f.Hex)).IsRoom(), "decoded as a room")
	}

	var pub *ecdsa.PublicKey
	if k, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), unhex(fx.TestKey.PublicUncompressed)); err == nil {
		pub = k
	} else {
		check("test_key", "parse", false, err.Error())
	}
	for _, f := range fx.Envelopes {
		e, err := envelope.Parse(unhex(f.Bytes))
		if !checkOK(check, "envelopes", f.Name, err == nil, fmt.Sprint(err)) {
			continue
		}
		check("envelopes", f.Name+" type", e.Type().String() == f.Type, e.Type().String())
		if f.SigningInput != "" {
			check("envelopes", f.Name+" signing input", hex.EncodeToString(e.SigningInput()) == f.SigningInput, "mismatch")
		}
		if f.Type == "MSG" {
			check("envelopes", f.Name+" id", e.ID().String() == f.MessageID, e.ID().String())
			m, _ := e.Msg()
			check("envelopes", f.Name+" body", m.Body() == f.Fields["body"], fmt.Sprintf("%q", m.Body()))
		}
		if f.Signature != "" && pub != nil {
			sig, _ := e.Signature()
			check("envelopes", f.Name+" signature", hex.EncodeToString(sig) == f.Signature && e.Verify(pub) == nil, fmt.Sprint(e.Verify(pub)))
		}
		if !e.Signed() {
			check("envelopes", f.Name+" json", jsonRoundTrip(e), "JSON round trip changed bytes")
		}
	}
	for _, f := range fx.RoomPackets {
		e, err := envelope.Parse(unhex(f.Bytes))
		if !checkOK(check, "room_packets", f.Name, err == nil, fmt.Sprint(err)) {
			continue
		}
		r, ok := e.Room()
		rooms := r.Rooms()
		good := ok && int(r.Op()) == f.Fields.Op && len(rooms) == len(f.Fields.Rooms)
		for i := range rooms {
			good = good && addrHex(rooms[i]) == f.Fields.Rooms[i]
		}
		check("room_packets", f.Name, good, e.String())
	}
	s := fx.SMSWrap
	e, err := envelope.FromSMS(unhex(s.SMSPacket), envelope.AddressFromBytes(unhex(s.LSFSource)), envelope.AddressFromBytes(unhex(s.LSFDestination)), s.NodeTime, s.NodeDefaultTTL, s.Nonce)
	check("sms_wrap", "wrap", err == nil && hex.EncodeToString(e.Bytes()) == s.Envelope, fmt.Sprint(err))
	if basic, err := envelope.Parse(unhex(fx.Envelopes[0].Bytes)); err == nil {
		sms, err := envelope.ToSMS(basic)
		check("sms_wrap", "unwrap", err == nil && hex.EncodeToString(sms) == s.Unwrap.SMSPacket, fmt.Sprint(err))
	}
	for _, f := range fx.Invalid {
		_, err := envelope.Parse(unhex(f.Bytes))
		check("invalid_envelopes", f.Name, err != nil, "accepted")
	}
	return pass, fail
}

func checkOK(check func(string, string, bool, string), section, name string, ok bool, msg string) bool {
	check(section, name, ok, msg)
	return ok
}

func jsonRoundTrip(e *envelope.Envelope) bool {
	j, err := json.Marshal(e)
	if err != nil {
		return false
	}
	var back envelope.Envelope
	if err := json.Unmarshal(j, &back); err != nil {
		return false
	}
	return bytes.Equal(back.Bytes(), e.Bytes())
}
