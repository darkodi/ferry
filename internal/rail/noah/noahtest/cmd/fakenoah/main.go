// Command fakenoah serves the fake Noah API (internal/rail/noah/noahtest)
// on a local port, so Ferry can run end to end without a sandbox key.
//
//	FAKE_NOAH_ADDR            listen address (default 127.0.0.1:18090)
//	FAKE_NOAH_API_KEY         the API key it accepts (default fake-noah-key)
//	FAKE_NOAH_SIGNING_PUBKEY  optional PEM public key; if set, every request
//	                          must carry a valid Api-Signature
//	FAKE_NOAH_WEBHOOK_URL     optional; where to POST signed Transaction webhooks,
//	                          e.g. http://127.0.0.1:8080/v1/webhooks/noah
//	FAKE_NOAH_WEBHOOK_KEY     PEM P-384 private key that signs them; give Ferry the
//	                          public half as NOAH_WEBHOOK_PUBLIC_KEY
//
// Point Ferry at it with NOAH_BASE_URL=http://127.0.0.1:18090/v1. Drive
// transactions with:
//
//	curl -X POST localhost:18090/_control/transactions/<id>/settle   (or fail, review, reverse)
//	curl -X POST localhost:18090/_control/credit/1000                (a treasury top-up arriving)
//	curl localhost:18090/_control/transactions
package main

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/darkodi/ferry/internal/rail/noah"
	"github.com/darkodi/ferry/internal/rail/noah/noahtest"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("fakenoah stopped", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	addr := envOr("FAKE_NOAH_ADDR", "127.0.0.1:18090")
	var pub *ecdsa.PublicKey
	if path := os.Getenv("FAKE_NOAH_SIGNING_PUBKEY"); path != "" {
		var err error
		if pub, err = readPublicKey(path); err != nil {
			return err
		}
	}
	s := noahtest.NewUnstarted(envOr("FAKE_NOAH_API_KEY", "fake-noah-key"), pub, time.Now)
	if url := os.Getenv("FAKE_NOAH_WEBHOOK_URL"); url != "" {
		b, err := os.ReadFile(os.Getenv("FAKE_NOAH_WEBHOOK_KEY")) //nolint:gosec // an operator-chosen key file for a local dev tool
		if err != nil {
			return fmt.Errorf("FAKE_NOAH_WEBHOOK_KEY: %w", err)
		}
		key, err := noah.ParseSigningKey(b) // same PEM forms as a request-signing key
		if err != nil {
			return err
		}
		s.EnableWebhooks(url, key, true)
		log.Info("fake Noah sends webhooks", "url", url)
	}
	log.Info("fake Noah listening", "base_url", "http://"+addr+"/v1", "signing_required", pub != nil)
	srv := &http.Server{Addr: addr, Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

func readPublicKey(path string) (*ecdsa.PublicKey, error) {
	b, err := os.ReadFile(path) //nolint:gosec // an operator-chosen key file for a local dev tool
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("signing public key is not PEM")
	}
	k, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse public key: %w", err)
	}
	ek, ok := k.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("signing public key is not ECDSA")
	}
	return ek, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
