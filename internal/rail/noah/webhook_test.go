package noah_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"testing"

	"github.com/darkodi/ferry/internal/rail"
	"github.com/darkodi/ferry/internal/rail/noah"
	"github.com/darkodi/ferry/internal/rail/noah/noahtest"
)

func webhooksFor(t *testing.T, key *ecdsa.PrivateKey) *noah.Webhooks {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	w, err := noah.NewWebhooks(string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})))
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func signed(t *testing.T, key *ecdsa.PrivateKey, body []byte) http.Header {
	t.Helper()
	sig, err := noahtest.SignWebhook(key, body)
	if err != nil {
		t.Fatal(err)
	}
	return http.Header{noah.SignatureHeader: {sig}}
}

const txEvent = `{"Data":{"ID":"33ab3f32-2ab7-5921-b581-ba43938e31a7","Status":"Settled","Direction":"Out","ExternalID":"p1",` +
	`"CryptoCurrency":"USDC_TEST","NewFieldNoahAdded":{"x":1}},"EventType":"Transaction","EventVersion":1730205262722,` +
	`"Occurred":"2024-10-29T12:34:23Z","UserID":"u"}`

func TestWebhookParse(t *testing.T) {
	key := p384(t)
	w := webhooksFor(t, key)
	body := []byte(txEvent)
	ev, err := w.Parse(body, signed(t, key, body))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Rail != "noah" || ev.RailTxID != "33ab3f32-2ab7-5921-b581-ba43938e31a7" || ev.Version != 1730205262722 ||
		ev.Reverses != "" || ev.Occurred.IsZero() || !bytes.Equal(ev.Payload, body) {
		t.Fatalf("event %+v", ev)
	}

	// The docs' reversal example uses a lower-case event type.
	rev := []byte(`{"Data":{"ID":"r1","Reverses":"33ab3f32","Direction":"In","Status":"Settled"},"EventType":"transaction","EventVersion":2}`)
	ev, err = w.Parse(rev, signed(t, key, rev))
	if err != nil || ev.Reverses != "33ab3f32" || ev.RailTxID != "r1" {
		t.Fatalf("reversal: %+v, %v", ev, err)
	}
}

// Nothing is believed before the signature verifies over the exact bytes.
func TestWebhookSignatureRequired(t *testing.T) {
	key := p384(t)
	w := webhooksFor(t, key)
	body := []byte(txEvent)
	good := signed(t, key, body)

	tampered := bytes.Replace(body, []byte(`"Settled"`), []byte(`"Failed"`), 1)
	for name, tc := range map[string]struct {
		body   []byte
		header http.Header
	}{
		"tampered body":    {tampered, good},
		"another key":      {body, signed(t, p384(t), body)},
		"no signature":     {body, http.Header{}},
		"not base64":       {body, http.Header{noah.SignatureHeader: {"%%%"}}},
		"signature of nil": {body, signed(t, key, nil)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := w.Parse(tc.body, tc.header); !errors.Is(err, rail.ErrBadSignature) {
				t.Fatalf("got %v, want ErrBadSignature", err)
			}
		})
	}
}

func TestWebhookRejectsUnusableEvents(t *testing.T) {
	key := p384(t)
	w := webhooksFor(t, key)
	for name, tc := range map[string]struct {
		body    string
		ignored bool
	}{
		"customer event":   {`{"Data":{"CustomerID":"c"},"EventType":"Customer","EventVersion":1}`, true},
		"fiat deposit":     {`{"Data":{"ID":"d"},"EventType":"FiatDeposit","EventVersion":1}`, true},
		"no version":       {`{"Data":{"ID":"x"},"EventType":"Transaction"}`, false},
		"no id":            {`{"Data":{"Status":"Settled"},"EventType":"Transaction","EventVersion":1}`, false},
		"not JSON":         {`{"Data":`, false},
		"data not object":  {`{"Data":"x","EventType":"Transaction","EventVersion":1}`, false},
		"negative version": {`{"Data":{"ID":"x"},"EventType":"Transaction","EventVersion":-5}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			body := []byte(tc.body)
			_, err := w.Parse(body, signed(t, key, body))
			if err == nil {
				t.Fatal("accepted")
			}
			if errors.Is(err, rail.ErrIgnoredEvent) != tc.ignored {
				t.Fatalf("got %v, ignored=%v", err, tc.ignored)
			}
			if errors.Is(err, rail.ErrBadSignature) {
				t.Fatal("a signed body reported as a bad signature")
			}
		})
	}
}

// Noah's published keys load, and the right one is picked per environment.
func TestPublishedWebhookKeys(t *testing.T) {
	for _, k := range []string{noah.SandboxWebhookKey, noah.ProductionWebhookKey} {
		if _, err := noah.NewWebhooks(k); err != nil {
			t.Fatal(err)
		}
	}
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if _, err := noah.WebhooksFromEnv(env(map[string]string{"NOAH_BASE_URL": "https://api.noah.com/v1"})); err != nil {
		t.Fatal(err)
	}
	if _, err := noah.WebhooksFromEnv(env(map[string]string{"NOAH_WEBHOOK_PUBLIC_KEY": "/nonexistent.pem"})); err == nil {
		t.Fatal("a missing override key file was accepted")
	}
}

// Whatever a signed body holds, Parse never panics, and an accepted event
// has what the intake relies on.
func FuzzWebhookEnvelope(f *testing.F) {
	f.Add([]byte(txEvent))
	f.Add([]byte(`{"Data":{"ID":"r","Reverses":"o"},"EventType":"TRANSACTION","EventVersion":9}`))
	f.Add([]byte(`{"Data":null,"EventType":"Transaction","EventVersion":1}`))
	f.Add([]byte(`[]`))
	key, err := ecdsaKey()
	if err != nil {
		f.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	w, err := noah.NewWebhooks(string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})))
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		sig, err := noahtest.SignWebhook(key, body)
		if err != nil {
			t.Fatal(err)
		}
		ev, err := w.Parse(body, http.Header{noah.SignatureHeader: {sig}})
		if err != nil {
			if errors.Is(err, rail.ErrBadSignature) {
				t.Fatal("a correctly signed body failed verification")
			}
			return
		}
		if ev.RailTxID == "" || len(ev.RailTxID) > 100 || len(ev.Reverses) > 100 || ev.Version <= 0 || !bytes.Equal(ev.Payload, body) {
			t.Fatalf("accepted a bad event: %+v", ev)
		}
	})
}

// A bank return arrives as a new transaction; Get on the original finds
// it and reports it, one version above settled.
func TestGetFindsReturn(t *testing.T) {
	r, fake := setup(t)
	_, tx, err := submit(t, r, "n-return")
	if err != nil {
		t.Fatal(err)
	}
	if err := fake.Settle(tx.ID); err != nil {
		t.Fatal(err)
	}
	got, err := r.Get(t.Context(), tx.ID)
	if err != nil || got.Return != nil || got.Version != 2 {
		t.Fatalf("settled, no return yet: %+v, %v", got, err)
	}
	retID, err := fake.Reverse(tx.ID, "98.25")
	if err != nil {
		t.Fatal(err)
	}
	got, err = r.Get(t.Context(), tx.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Return == nil || got.Return.RailTxID != retID || got.Return.Amount != usdc("98.25") || got.Version != 3 || got.Status != rail.StatusSettled {
		t.Fatalf("after the return: %+v", got)
	}
}
