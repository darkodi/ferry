package noah

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/darkodi/ferry/internal/platform/awskms"
)

// FromEnv builds the adapter from the environment (see .env.example):
//
//	NOAH_BASE_URL          API base (default https://api.sandbox.noah.com/v1)
//	NOAH_API_KEY           API key (required)
//	NOAH_SIGNING_KEY_PATH  PEM P-384 private key for Api-Signature (local);
//	NOAH_SIGNING_KMS_KEY_ID  or an AWS KMS ECC_NIST_P384 key that signs instead
//	                       (the private key never leaves KMS); one of the two
//	                       is optional in sandbox, required in production
//	FERRY_KMS_ENDPOINT     KMS endpoint override (e.g. LocalStack); empty for AWS
//	NOAH_CRYPTO_CURRENCY   what Ferry sells (default USDC_TEST)
//	NOAH_QUOTE_TTL         how long a prepared price is honoured (default 2m)
func FromEnv(ctx context.Context, getenv func(string) string, now func() time.Time) (*Rail, error) {
	base := getenv("NOAH_BASE_URL")
	if base == "" {
		base = "https://api.sandbox.noah.com/v1"
	}
	key := getenv("NOAH_API_KEY")
	if key == "" {
		return nil, errors.New("NOAH_API_KEY is required for the noah rail")
	}
	var signer *Signer
	path, kmsKey := getenv("NOAH_SIGNING_KEY_PATH"), getenv("NOAH_SIGNING_KMS_KEY_ID")
	switch {
	case path != "" && kmsKey != "":
		return nil, errors.New("set NOAH_SIGNING_KEY_PATH or NOAH_SIGNING_KMS_KEY_ID, not both")
	case kmsKey != "":
		api, err := awskms.New(ctx, getenv("FERRY_KMS_ENDPOINT"))
		if err != nil {
			return nil, err
		}
		ks, err := awskms.NewECDSASigner(ctx, api, kmsKey)
		if err != nil {
			return nil, err
		}
		if signer, err = NewDigestSigner(ks, ks.Public(), now); err != nil {
			return nil, err
		}
	case path != "":
		b, err := os.ReadFile(path) //nolint:gosec // the operator names the key file
		if err != nil {
			// Name the file, never its contents.
			return nil, fmt.Errorf("read NOAH_SIGNING_KEY_PATH %s: %w", path, err)
		}
		pk, err := ParseSigningKey(b)
		if err != nil {
			return nil, err
		}
		if signer, err = NewSigner(pk, now); err != nil {
			return nil, err
		}
	}
	client, err := NewClient(base, key, signer)
	if err != nil {
		return nil, err
	}
	cfg := Config{CryptoCurrency: getenv("NOAH_CRYPTO_CURRENCY")}
	if cfg.CryptoCurrency == "" {
		cfg.CryptoCurrency = "USDC_TEST"
	}
	if v := getenv("NOAH_QUOTE_TTL"); v != "" {
		if cfg.QuoteTTL, err = time.ParseDuration(v); err != nil || cfg.QuoteTTL <= 0 {
			return nil, fmt.Errorf("NOAH_QUOTE_TTL %q: want a positive duration such as 2m", v)
		}
	}
	return New(client, cfg, now)
}

// Signed reports whether the adapter signs its requests.
func (r *Rail) Signed() bool { return r.c.signer != nil }
