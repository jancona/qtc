// Package store implements the QTC mailbox storage protocol (node protocol
// §5): JSON Lines PUT, QUERY, and WATCH over a byte stream, with a Server
// that serves one stream against a Store, a Client for the other end, and
// an in-memory Store for the spike. It is standard library plus envelope;
// the libp2p stream is handed in by station as an io.ReadWriter.
package store

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jancona/qtc/envelope"
)

// ProtocolID is the libp2p stream protocol ID.
const ProtocolID = "/qtc/0/store"

// Limits from node protocol §5.
const (
	MaxMessage   = 1 << 20 // bytes per JSON line
	DefaultLimit = 100
	MaxLimit     = 1000
)

// Message type names.
const (
	TypePut     = "PUT"
	TypePutOK   = "PUT_OK"
	TypePutErr  = "PUT_ERR"
	TypeQuery   = "QUERY"
	TypeResult  = "RESULT"
	TypeWatch   = "WATCH"
	TypeEvent   = "EVENT"
	TypeUnwatch = "UNWATCH"
)

// Code is a PUT_ERR code.
type Code int

const (
	CodeRefused Code = 1 // including live-only MSG (TTL 0)
	CodeQuota   Code = 2
	CodeExpired Code = 3
	CodeInvalid Code = 4
)

func (c Code) String() string {
	switch c {
	case CodeRefused:
		return "refused"
	case CodeQuota:
		return "quota"
	case CodeExpired:
		return "expired"
	case CodeInvalid:
		return "invalid"
	}
	return fmt.Sprintf("code %d", int(c))
}

// PutError is a PUT_ERR reply.
type PutError struct {
	ID     envelope.ID
	Code   Code
	Reason string
}

func (e *PutError) Error() string {
	return fmt.Sprintf("store: put %s: %s: %s", e.ID, e.Code, e.Reason)
}

// ErrQuota is returned by a Store when a per-callsign or per-writer quota
// is exceeded; the server maps it to CodeQuota.
var ErrQuota = errors.New("store: quota exceeded")

// message is the decoded form of any protocol message: every field is
// optional and unknown fields are ignored. Envelopes stay as base64 strings
// here so an undecodable one can be answered with PUT_ERR rather than a
// stream error.
type message struct {
	Type      string   `json:"type"`
	Callsign  string   `json:"callsign,omitempty"`
	Callsigns []string `json:"callsigns,omitempty"`
	Env       string   `json:"env,omitempty"`
	Envs      []string `json:"envs"`
	ID        string   `json:"id,omitempty"`
	Code      Code     `json:"code,omitempty"`
	Reason    string   `json:"reason,omitempty"`
	Since     *uint32  `json:"since,omitempty"`
	Limit     *int     `json:"limit,omitempty"`
	Types     []int    `json:"types,omitempty"`
	Next      *uint32  `json:"next"`
}

// MarshalJSON emits only the fields that belong to the message's type, so
// RESULT always carries "envs" and "next" (null when there is no more) and
// nothing else carries them.
func (m message) MarshalJSON() ([]byte, error) {
	type common struct {
		Type      string   `json:"type"`
		Callsign  string   `json:"callsign,omitempty"`
		Callsigns []string `json:"callsigns,omitempty"`
		Env       string   `json:"env,omitempty"`
		ID        string   `json:"id,omitempty"`
		Code      Code     `json:"code,omitempty"`
		Reason    string   `json:"reason,omitempty"`
		Since     *uint32  `json:"since,omitempty"`
		Limit     *int     `json:"limit,omitempty"`
		Types     []int    `json:"types,omitempty"`
	}
	c := common{m.Type, m.Callsign, m.Callsigns, m.Env, m.ID, m.Code, m.Reason, m.Since, m.Limit, m.Types}
	if m.Type != TypeResult {
		return json.Marshal(c)
	}
	envs := m.Envs
	if envs == nil {
		envs = []string{}
	}
	return json.Marshal(struct {
		common
		Envs []string `json:"envs"`
		Next *uint32  `json:"next"`
	}{c, envs, m.Next})
}

func encodeEnv(e *envelope.Envelope) string {
	return base64.StdEncoding.EncodeToString(e.Bytes())
}

func decodeEnv(s string) (*envelope.Envelope, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("bad base64: %w", err)
	}
	return envelope.Parse(raw)
}

// parseCallsign parses an address field and normalizes callsigns to their
// base; rooms (archives) pass through.
func parseCallsign(s string) (envelope.Address, error) {
	a, err := envelope.ParseAddress(s)
	if err != nil {
		return 0, err
	}
	if !a.IsStandard() && !a.IsRoom() {
		return 0, fmt.Errorf("address %q is neither a callsign nor a room", s)
	}
	return a.Base(), nil
}
