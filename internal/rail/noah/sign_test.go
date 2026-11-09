package noah_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/darkodi/ferry/internal/rail/noah"
)

func p384(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

var signTime = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

// The signature carries every claim the docs list, binds the exact body
// bytes, and verifies with the matching public key only.
func TestSignClaims(t *testing.T) {
	key := p384(t)
	s, err := noah.NewSigner(key, func() time.Time { return signTime })
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"Nonce":"n-1","FiatAmount":"100.00"}`)
	q := url.Values{"Country": {"US"}, "FiatCurrency": {"USD"}}
	tok, err := s.Sign("POST", "/v1/transactions/sell", q, body)
	if err != nil {
		t.Fatal(err)
	}
	c, err := noah.VerifyJWT(tok, &key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	switch {
	case c.Audience != "https://api.noah.com":
		t.Errorf("aud %q", c.Audience)
	case c.IssuedAt != signTime.Unix() || c.Expires-c.IssuedAt != 300:
		t.Errorf("iat %d exp %d, want a 5 minute window from %d", c.IssuedAt, c.Expires, signTime.Unix())
	case c.Method != "POST" || c.Path != "/v1/transactions/sell":
		t.Errorf("method %q path %q", c.Method, c.Path)
	case c.BodyHash != hex.EncodeToString(sum[:]):
		t.Errorf("bodyHash %q is not the SHA-256 of the body", c.BodyHash)
	case c.QueryParams["Country"] != "US" || c.QueryParams["FiatCurrency"] != "USD" || len(c.QueryParams) != 2:
		t.Errorf("queryParams %v", c.QueryParams)
	}

	if _, err := noah.VerifyJWT(tok, &p384(t).PublicKey); err == nil {
		t.Error("verified with another key")
	}
	parts := strings.Split(tok, ".")
	tampered := parts[0] + "." + parts[1] + "x." + parts[2]
	if _, err := noah.VerifyJWT(tampered, &key.PublicKey); err == nil {
		t.Error("verified a tampered payload")
	}
}

// A request without a body or query omits those claims, as the docs ask.
func TestSignOmitsEmptyClaims(t *testing.T) {
	key := p384(t)
	s, _ := noah.NewSigner(key, func() time.Time { return signTime })
	tok, err := s.Sign("GET", "/v1/transactions/abc", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := noah.VerifyJWT(tok, &key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if c.BodyHash != "" || c.QueryParams != nil {
		t.Fatalf("claims %+v, want no bodyHash or queryParams", c)
	}
	if _, err := s.Sign("GET", "/v1/x", url.Values{"a": {"1", "2"}}, nil); err == nil {
		t.Fatal("signed a repeated query parameter the claim cannot express")
	}
}

func TestSigningKeyMustBeP384(t *testing.T) {
	p256, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := noah.NewSigner(p256, time.Now); err == nil {
		t.Fatal("accepted a P-256 key for ES384")
	}
}

// Keys load from both PEM forms openssl produces.
func TestParseSigningKey(t *testing.T) {
	key := p384(t)
	sec1, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string][]byte{
		"SEC 1":   pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: sec1}),
		"PKCS #8": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}),
	} {
		got, err := noah.ParseSigningKey(p)
		if err != nil || !got.Equal(key) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, p := range map[string][]byte{
		"not PEM":    []byte("hello"),
		"public key": pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte{1}}),
	} {
		if _, err := noah.ParseSigningKey(p); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func ecdsaKey() (*ecdsa.PrivateKey, error) { return ecdsa.GenerateKey(elliptic.P384(), rand.Reader) }
