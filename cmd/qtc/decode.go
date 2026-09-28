package main

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jancona/qtc/envelope"
)

// decoded is the JSON shape of a decoded envelope.
type decoded struct {
	Type        string   `json:"type"`
	Length      int      `json:"length"`
	Version     int      `json:"version"`
	Flags       string   `json:"flags"`
	Source      string   `json:"source,omitempty"`
	Destination string   `json:"destination,omitempty"`
	Timestamp   uint32   `json:"timestamp"`
	ID          string   `json:"id,omitempty"`
	StoreID     string   `json:"store_id"`
	TTL         *uint16  `json:"ttl_minutes,omitempty"`
	Nonce       *uint16  `json:"nonce,omitempty"`
	Body        *string  `json:"body,omitempty"`
	Expiry      *uint32  `json:"expiry,omitempty"`
	MessageID   string   `json:"message_id,omitempty"`
	Status      string   `json:"status,omitempty"`
	LastHeard   *uint32  `json:"last_heard,omitempty"`
	Note        *string  `json:"note,omitempty"`
	Op          string   `json:"op,omitempty"`
	Rooms       []string `json:"rooms,omitempty"`
	Signed      bool     `json:"signed"`
	Signature   string   `json:"signature,omitempty"`
	Verified    *bool    `json:"verified,omitempty"`
	VerifyError string   `json:"verify_error,omitempty"`
}

func runDecode(args []string) error {
	fs := flag.NewFlagSet("decode", flag.ContinueOnError)
	keyFile := fs.String("key", "", "PEM file with the signer's public key (SPKI) or private key (PKCS #8) to verify against")
	pubHex := fs.String("pubhex", "", "signer's uncompressed public key as hex (04 || X || Y)")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("need one envelope argument: hex, base64, or - for stdin")
	}
	raw, err := readEnvelopeArg(fs.Arg(0))
	if err != nil {
		return err
	}
	e, err := envelope.Parse(raw)
	if err != nil {
		return err
	}
	var pub *ecdsa.PublicKey
	switch {
	case *keyFile != "":
		if pub, err = loadPublicKey(*keyFile); err != nil {
			return err
		}
	case *pubHex != "":
		b, err := hex.DecodeString(*pubHex)
		if err != nil {
			return fmt.Errorf("pubhex: %w", err)
		}
		if pub, err = ecdsa.ParseUncompressedPublicKey(ellipticP256(), b); err != nil {
			return fmt.Errorf("pubhex: %w", err)
		}
	}
	d := describe(e, pub)
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(d)
	}
	printDecoded(d)
	return nil
}

// readEnvelopeArg accepts hex, standard base64, or "-" for stdin holding
// either.
func readEnvelopeArg(arg string) ([]byte, error) {
	if arg == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, err
		}
		arg = strings.TrimSpace(string(b))
	}
	arg = strings.Join(strings.Fields(arg), "")
	if b, err := hex.DecodeString(arg); err == nil {
		return b, nil
	}
	if b, err := base64.StdEncoding.DecodeString(arg); err == nil {
		return b, nil
	}
	return nil, errors.New("argument is neither hex nor base64")
}

func describe(e *envelope.Envelope, pub *ecdsa.PublicKey) decoded {
	d := decoded{
		Type:      e.Kind().String(),
		Length:    e.Len(),
		Version:   int(e.Version()),
		Flags:     fmt.Sprintf("0x%02x", e.Flags()),
		Timestamp: e.Timestamp(),
		StoreID:   e.StoreID().String(),
		Signed:    e.Signed(),
	}
	if e.Kind() != envelope.KindROOM {
		d.Source, d.Destination = e.Source().String(), e.Destination().String()
	}
	if m, ok := e.Msg(); ok {
		ttl, nonce, body := m.TTL(), m.Nonce(), m.Body()
		d.TTL, d.Nonce, d.Body = &ttl, &nonce, &body
		d.ID = e.ID().String()
		if exp, ok := m.Expiry(); ok {
			d.Expiry = &exp
		}
	}
	if r, ok := e.Rcpt(); ok {
		lh, note := r.LastHeard(), r.Note()
		d.MessageID, d.Status, d.LastHeard, d.Note = r.MessageID().String(), r.Status().String(), &lh, &note
	}
	if r, ok := e.Room(); ok {
		d.Op = r.Op().String()
		for _, a := range r.Rooms() {
			d.Rooms = append(d.Rooms, a.String())
		}
		note := r.Note()
		d.Note = &note
	}
	if sig, ok := e.Signature(); ok {
		d.Signature = hex.EncodeToString(sig)
		if pub != nil {
			v := e.Verify(pub) == nil
			d.Verified = &v
			if !v {
				d.VerifyError = e.Verify(pub).Error()
			}
		}
	}
	return d
}

func printDecoded(d decoded) {
	p := func(k string, v any) { fmt.Printf("%-12s %v\n", k, v) }
	p("type", d.Type)
	p("length", d.Length)
	p("version", d.Version)
	p("flags", d.Flags)
	if d.Source != "" {
		p("source", d.Source)
		p("destination", d.Destination)
	}
	p("timestamp", d.Timestamp)
	if d.ID != "" {
		p("id", d.ID)
	}
	p("store_id", d.StoreID)
	if d.TTL != nil {
		p("ttl_minutes", *d.TTL)
		p("nonce", fmt.Sprintf("0x%04x", *d.Nonce))
		p("body", fmt.Sprintf("%q", *d.Body))
		if d.Expiry != nil {
			p("expiry", *d.Expiry)
		} else {
			p("expiry", "node decides")
		}
	}
	if d.MessageID != "" {
		p("message_id", d.MessageID)
		p("status", d.Status)
		p("last_heard", *d.LastHeard)
	}
	if d.Op != "" {
		p("op", d.Op)
		p("rooms", strings.Join(d.Rooms, " "))
	}
	if d.Note != nil && *d.Note != "" {
		p("note", fmt.Sprintf("%q", *d.Note))
	}
	if d.Signed {
		p("signature", d.Signature)
		switch {
		case d.Verified == nil:
			p("verified", "no key given")
		case *d.Verified:
			p("verified", true)
		default:
			p("verified", "false: "+d.VerifyError)
		}
	}
}

// loadPublicKey reads an SPKI public key or a PKCS #8 private key from PEM.
func loadPublicKey(path string) (*ecdsa.PublicKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, fmt.Errorf("%s: no PEM block", path)
	}
	switch block.Type {
	case "PUBLIC KEY":
		k, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		pub, ok := k.(*ecdsa.PublicKey)
		if !ok {
			return nil, fmt.Errorf("%s: not an ECDSA key", path)
		}
		return pub, nil
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		priv, ok := k.(*ecdsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("%s: not an ECDSA key", path)
		}
		return &priv.PublicKey, nil
	}
	return nil, fmt.Errorf("%s: unexpected PEM type %q", path, block.Type)
}
