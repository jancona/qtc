package envelope

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"fmt"
	"math/big"
)

// Sign returns a signed copy of an unsigned MSG or RCPT (§4.6, §5.4). The
// signature is ECDSA on secp256r1 over SHA-256 of SigningInput ("QTC" ‖
// Kind ‖ the signed fields), encoded as raw r‖s. Nonces come from crypto/ecdsa, so two signatures of the same
// envelope differ; verify them, never compare them. The message ID is
// unchanged by signing.
func (e *Envelope) Sign(priv *ecdsa.PrivateKey) (*Envelope, error) {
	if !e.signable() {
		return nil, ErrNotSignable
	}
	if e.Signed() {
		return nil, ErrAlreadySigned
	}
	if priv.Curve != elliptic.P256() {
		return nil, fmt.Errorf("envelope: key is not secp256r1")
	}
	if len(e.raw)+SignatureLen > MaxPayload {
		return nil, fmt.Errorf("envelope: %d-byte payload leaves no room for a signature", len(e.raw))
	}
	digest := sha256.Sum256(e.SigningInput())
	r, s, err := ecdsa.Sign(nil, priv, digest[:])
	if err != nil {
		return nil, fmt.Errorf("envelope: sign: %w", err)
	}
	raw := make([]byte, 0, len(e.raw)+SignatureLen)
	raw = append(raw, e.raw...)
	raw[offFlags] |= FlagSigned
	raw = append(raw, make([]byte, SignatureLen)...)
	r.FillBytes(raw[len(e.raw) : len(e.raw)+32])
	s.FillBytes(raw[len(e.raw)+32:])
	return &Envelope{raw: raw, id: e.id}, nil
}

// Verify checks the signature of a signed MSG or RCPT against pub. It
// returns ErrUnsigned if there is no signature and ErrBadSignature if it does
// not verify. A caller that cannot verify a signature should treat the
// envelope as unsigned (StripSignature) rather than reject it, unless local
// policy says otherwise.
func (e *Envelope) Verify(pub *ecdsa.PublicKey) error {
	if !e.signable() {
		return ErrNotSignable
	}
	sig, ok := e.Signature()
	if !ok {
		return ErrUnsigned
	}
	if pub.Curve != elliptic.P256() {
		return fmt.Errorf("envelope: key is not secp256r1")
	}
	digest := sha256.Sum256(e.SigningInput())
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(pub, digest[:], r, s) {
		return ErrBadSignature
	}
	return nil
}
