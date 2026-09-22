package main

import (
	"crypto/elliptic"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"

	"github.com/jancona/qtc/qtcd"
)

func ellipticP256() elliptic.Curve { return elliptic.P256() }

func runKeys(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: qtc keys gen <file> | qtc keys id <file>")
	}
	switch args[0] {
	case "gen":
		if _, err := os.Stat(args[1]); err == nil {
			return fmt.Errorf("%s already exists", args[1])
		}
		k, err := qtcd.LoadOrCreateKey(args[1])
		if err != nil {
			return err
		}
		id, err := qtcd.PeerIDFromKey(k)
		if err != nil {
			return err
		}
		fmt.Printf("wrote %s\npeer id  %s\n", args[1], id)
		return nil
	case "id":
		if _, err := os.Stat(args[1]); err != nil {
			return err
		}
		k, err := qtcd.LoadOrCreateKey(args[1])
		if err != nil {
			return err
		}
		id, err := qtcd.PeerIDFromKey(k)
		if err != nil {
			return err
		}
		pub, err := k.PublicKey.Bytes()
		if err != nil {
			return err
		}
		der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
		if err != nil {
			return err
		}
		fmt.Printf("peer id      %s\npublic hex   %s\n%s", id, hex.EncodeToString(pub), pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
		return nil
	}
	return fmt.Errorf("unknown keys subcommand %q", args[0])
}
