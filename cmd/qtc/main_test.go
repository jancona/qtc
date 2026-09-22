package main

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/jancona/qtc/envelope"
)

func TestFixturesChecker(t *testing.T) {
	b, err := os.ReadFile("../../docs/qtc-fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx fixtureFile
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatal(err)
	}
	pass, fail := checkFixtures(&fx, func(section, name, msg string) { t.Errorf("%s/%s: %s", section, name, msg) })
	if fail != 0 || pass < 100 {
		t.Errorf("pass=%d fail=%d", pass, fail)
	}
	// A corrupted fixture must be reported, not crash.
	fx.Envelopes[0].MessageID = "0000000000000000"
	fails := 0
	checkFixtures(&fx, func(string, string, string) { fails++ })
	if fails != 1 {
		t.Errorf("corrupted fixture produced %d failures, want 1", fails)
	}
}

func TestDecodeDescribe(t *testing.T) {
	raw, _ := hex.DecodeString("0800020000018a92ae0000001680b76aa3ed4005a03c7f4869204a696d2c2074657374696e6720746865206e657720656e76656c6f70652e")
	e, err := envelope.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	d := describe(e, nil)
	if d.Type != "MSG" || d.ID != "cba5c5c74eaebf72" || d.Body == nil || *d.Body != "Hi Jim, testing the new envelope." || d.Expiry == nil || *d.Expiry != 1789214400 {
		t.Errorf("describe = %+v", d)
	}
	for _, in := range []string{hex.EncodeToString(raw), "CAACAAABipKuAAAAFoC3aqPtQAWgPH9IaSBKaW0sIHRlc3RpbmcgdGhlIG5ldyBlbnZlbG9wZS4="} {
		if got, err := readEnvelopeArg(in); err != nil || hex.EncodeToString(got) != hex.EncodeToString(raw) {
			t.Errorf("readEnvelopeArg(%.20s) = %x, %v", in, got, err)
		}
	}
	if _, err := readEnvelopeArg("not*valid"); err == nil {
		t.Error("accepted junk")
	}
}
