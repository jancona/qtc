package qtcd

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

// loadOrCreateKey reads an ECDSA P-256 key from a PKCS #8 PEM file, creating
// it with mode 0600 if absent. An empty path yields an ephemeral key.
func loadOrCreateKey(path string) (*ecdsa.PrivateKey, error) {
	if path == "" {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("qtcd: generate key: %w", err)
		}
		return k, nil
	}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("qtcd: generate key: %w", err)
		}
		der, err := x509.MarshalPKCS8PrivateKey(k)
		if err != nil {
			return nil, fmt.Errorf("qtcd: encode key: %w", err)
		}
		pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("qtcd: key dir: %w", err)
		}
		if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
			return nil, fmt.Errorf("qtcd: write key %s: %w", path, err)
		}
		return k, nil
	case err != nil:
		return nil, fmt.Errorf("qtcd: read key %s: %w", path, err)
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, fmt.Errorf("qtcd: key %s: no PEM block", path)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("qtcd: key %s: %w", path, err)
	}
	k, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || k.Curve != elliptic.P256() {
		return nil, fmt.Errorf("qtcd: key %s is not ECDSA P-256", path)
	}
	return k, nil
}

// libp2pKey converts the node key into a libp2p identity.
func libp2pKey(k *ecdsa.PrivateKey) (crypto.PrivKey, error) {
	priv, _, err := crypto.ECDSAKeyPairFromKey(k)
	if err != nil {
		return nil, fmt.Errorf("qtcd: libp2p identity: %w", err)
	}
	return priv, nil
}

// LoadOrCreateKey reads the node key at path, creating it if absent. It is
// exported for the qtc CLI's key commands.
func LoadOrCreateKey(path string) (*ecdsa.PrivateKey, error) { return loadOrCreateKey(path) }

// PeerIDFromKey derives the libp2p peer ID for a node key.
func PeerIDFromKey(k *ecdsa.PrivateKey) (peer.ID, error) {
	lk, err := libp2pKey(k)
	if err != nil {
		return "", err
	}
	return peer.IDFromPrivateKey(lk)
}
