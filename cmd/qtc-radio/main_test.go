package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLoadRadioConfig: qtc-radio reads the [Radio] and [Modem] settings of
// an m17-gateway config, with m17-gateway's defaults.
func TestLoadRadioConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "gw.ini")
	ini := "[General]\nCallsign=N1ADJ\n[Radio]\nRXFrequency=433475000\nTXFrequency=433475000\nPower=10\nAFC=true\nFrequencyCorr=17\n" +
		"[Modem]\nType=CC1200\nPort=/dev/ttyAMA0\n"
	if err := os.WriteFile(p, []byte(ini), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := loadRadioConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.rxFrequency != 433475000 || c.txFrequency != 433475000 || c.frequencyCorr != 17 || !c.afc || c.power != 10 {
		t.Errorf("radio %+v", c)
	}
	if c.rxHoldoff != 500*time.Millisecond || c.packetGap != 250*time.Millisecond || c.modemType != "cc1200" {
		t.Errorf("defaults: holdoff %v gap %v type %q", c.rxHoldoff, c.packetGap, c.modemType)
	}
	c.modemType = "hackrf"
	if _, err := openModem(c); err == nil {
		t.Error("opened an unknown modem type")
	}
	if err := os.WriteFile(p, []byte("[Radio]\nRXFrequency=lots\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRadioConfig(p); err == nil {
		t.Error("accepted a bad frequency")
	}
}
