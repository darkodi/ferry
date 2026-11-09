package noah

import (
	"crypto/ecdsa"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/darkodi/ferry/internal/rail"
)

// Noah's webhook signing public keys, as published at
// https://docs.noah.com/api-concepts/webhooks/configuration. Public keys:
// safe to commit, and pinning them here means a compromised config cannot
// quietly swap in an attacker's key for the real Noah environments.
const (
	SandboxWebhookKey = `-----BEGIN PUBLIC KEY-----
MHYwEAYHKoZIzj0CAQYFK4EEACIDYgAEm8yBiD+kmVJ1Xc9sfRkDx0yo9+u8yiAD
PngI20KoEswz0gflp8o/z66Abqz/m9A1CBecixWdeT72pA8NZBJI6L6Osd8RV+yx
QArxeGKEVX/2QNrfPqeAKODHT5LdStGT
-----END PUBLIC KEY-----`
	ProductionWebhookKey = `-----BEGIN PUBLIC KEY-----
MHYwEAYHKoZIzj0CAQYFK4EEACIDYgAELKJhxcUGJr3XgRrf+laSAVHvp31wFhE2
XdicXvF0DAdKzSPN8bkSdjrsUA6nnVUq3M47Y7RUYugMfkagaYjUExQZVjpMFg0P
DnXWl9y0dXYDq+pzYhAgL+MNpnY0eJ78
-----END PUBLIC KEY-----`
)

// SignatureHeader carries the base64 ECDSA signature of the raw body.
const SignatureHeader = "Webhook-Signature"

// Webhooks verifies and parses Noah webhooks.
type Webhooks struct {
	key *ecdsa.PublicKey
}

// NewWebhooks returns a verifier for the given PEM public key.
func NewWebhooks(pemKey string) (*Webhooks, error) {
	block, _ := pem.Decode([]byte(pemKey))
	if block == nil {
		return nil, errors.New("noah webhook: public key is not PEM")
	}
	k, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("noah webhook: parse public key: %w", err)
	}
	ek, ok := k.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("noah webhook: public key is not ECDSA")
	}
	return &Webhooks{key: ek}, nil
}

// WebhooksFromEnv picks the verification key: NOAH_WEBHOOK_PUBLIC_KEY (a
// PEM file, for the fake or a key rotation) if set, else Noah's
// production key for the production base URL and the sandbox key
// otherwise.
func WebhooksFromEnv(getenv func(string) string) (*Webhooks, error) {
	if path := getenv("NOAH_WEBHOOK_PUBLIC_KEY"); path != "" {
		b, err := os.ReadFile(path) //nolint:gosec // the operator names the key file
		if err != nil {
			return nil, fmt.Errorf("read NOAH_WEBHOOK_PUBLIC_KEY %s: %w", path, err)
		}
		return NewWebhooks(string(b))
	}
	if strings.HasPrefix(getenv("NOAH_BASE_URL"), "https://api.noah.com") {
		return NewWebhooks(ProductionWebhookKey)
	}
	return NewWebhooks(SandboxWebhookKey)
}

// Name is the rail name events are stored under.
func (*Webhooks) Name() string { return "noah" }

// Envelope is Noah's webhook body.
type Envelope struct {
	EventType    string          `json:"EventType"`
	EventVersion int64           `json:"EventVersion"`
	Occurred     time.Time       `json:"Occurred"`
	Data         json.RawMessage `json:"Data"`
}

// Parse verifies a webhook and turns a Transaction event into a
// rail.Event. The signature (Webhook-Signature header) is checked over the
// exact bytes received, before any parsing: a bad one is
// rail.ErrBadSignature. Verified events of other types are
// rail.ErrIgnoredEvent. Unknown fields are allowed (Noah adds fields over
// time); the ones Ferry uses are required.
func (w *Webhooks) Parse(body []byte, header http.Header) (rail.Event, error) {
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(header.Get(SignatureHeader)))
	if err != nil || len(sig) == 0 {
		return rail.Event{}, rail.ErrBadSignature
	}
	digest := sha512.Sum384(body)
	if !ecdsa.VerifyASN1(w.key, digest[:], sig) {
		return rail.Event{}, rail.ErrBadSignature
	}
	var env Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return rail.Event{}, fmt.Errorf("noah webhook: body: %w", err)
	}
	// The docs show both "Transaction" and "transaction".
	if !strings.EqualFold(env.EventType, "Transaction") {
		return rail.Event{}, fmt.Errorf("%w: %q", rail.ErrIgnoredEvent, env.EventType)
	}
	if env.EventVersion <= 0 {
		return rail.Event{}, errors.New("noah webhook: missing EventVersion")
	}
	var t Transaction
	if err := json.Unmarshal(env.Data, &t); err != nil {
		return rail.Event{}, fmt.Errorf("noah webhook: Data: %w", err)
	}
	if t.ID == "" || len(t.ID) > 100 {
		return rail.Event{}, errors.New("noah webhook: Data.ID missing or too long")
	}
	if len(t.Reverses) > 100 {
		return rail.Event{}, errors.New("noah webhook: Data.Reverses too long")
	}
	return rail.Event{
		Rail:     "noah",
		Type:     "Transaction",
		RailTxID: t.ID,
		Reverses: t.Reverses,
		Version:  env.EventVersion,
		Occurred: env.Occurred,
		Payload:  body,
	}, nil
}
