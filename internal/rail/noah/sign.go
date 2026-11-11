package noah

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"time"
)

// Audience is the aud claim Noah requires in every request signature, in
// sandbox and production alike.
const Audience = "https://api.noah.com"

// signatureTTL is how long a request signature is valid. Noah accepts up
// to 15 minutes; a short window limits replay of a captured request.
const signatureTTL = 5 * time.Minute

// Signer makes the Api-Signature header: a JWT signed with ES384 (ECDSA on
// P-384 with SHA-384) that binds the API key's request to its method,
// path, query and exact body bytes.
//
// It is written against the standard library rather than a JWT package:
// the token has one fixed header and a handful of claims, and owning the
// encoding makes the bytes that are signed obvious.
type Signer struct {
	ds  DigestSigner
	now func() time.Time
}

// DigestSigner signs a SHA-384 digest with a P-384 key: a local key, or
// one held in AWS KMS (awskms.ECDSASigner) that Ferry never sees.
type DigestSigner interface {
	SignDigest(ctx context.Context, digest []byte) (r, s *big.Int, err error)
}

// localKey signs with a private key in memory (local development).
type localKey struct{ k *ecdsa.PrivateKey }

func (l localKey) SignDigest(_ context.Context, digest []byte) (r, s *big.Int, err error) {
	return ecdsa.Sign(rand.Reader, l.k, digest) //nolint:wrapcheck // the caller wraps it
}

// NewSigner returns a signer for a P-384 private key in memory.
func NewSigner(key *ecdsa.PrivateKey, now func() time.Time) (*Signer, error) {
	if key == nil || key.Curve != elliptic.P384() {
		return nil, errors.New("noah: request signing key must be an ECDSA P-384 key (ES384)")
	}
	return &Signer{ds: localKey{key}, now: now}, nil
}

// NewDigestSigner returns a signer that signs through ds, whose public
// key is pub (checked to be P-384), e.g. a KMS key.
func NewDigestSigner(ds DigestSigner, pub *ecdsa.PublicKey, now func() time.Time) (*Signer, error) {
	if ds == nil || pub == nil || pub.Curve != elliptic.P384() {
		return nil, errors.New("noah: request signing key must be an ECDSA P-384 key (ES384)")
	}
	return &Signer{ds: ds, now: now}, nil
}

// ParseSigningKey reads a PEM private key as produced by
// `openssl ecparam -name secp384r1 -genkey` (SEC 1, "EC PRIVATE KEY") or
// a PKCS #8 "PRIVATE KEY".
func ParseSigningKey(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("noah: signing key is not PEM")
	}
	switch block.Type {
	case "EC PRIVATE KEY":
		k, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("noah: parse EC private key: %w", err)
		}
		return k, nil
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("noah: parse PKCS #8 private key: %w", err)
		}
		ek, ok := k.(*ecdsa.PrivateKey)
		if !ok {
			return nil, errors.New("noah: signing key is not an ECDSA key")
		}
		return ek, nil
	default:
		return nil, fmt.Errorf("noah: unexpected PEM block %q for a signing key", block.Type)
	}
}

// Claims are the JWT claims Noah verifies.
type Claims struct {
	Audience    string            `json:"aud"`
	IssuedAt    int64             `json:"iat"`
	Expires     int64             `json:"exp"`
	Method      string            `json:"method"`
	Path        string            `json:"path"`
	QueryParams map[string]string `json:"queryParams,omitempty"`
	BodyHash    string            `json:"bodyHash,omitempty"`
}

// jwtHeader is fixed: base64url of {"alg":"ES384","typ":"JWT"}.
var jwtHeader = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"ES384","typ":"JWT"}`))

// Sign returns the Api-Signature value for one request. path is the URL
// path including the /v1 prefix; body is exactly the bytes that will be
// sent (nil for none). A query parameter given twice is refused: the
// claim is a flat map, so it could not describe the request.
func (s *Signer) Sign(ctx context.Context, method, path string, query url.Values, body []byte) (string, error) {
	now := s.now()
	c := Claims{
		Audience: Audience,
		IssuedAt: now.Unix(),
		Expires:  now.Add(signatureTTL).Unix(),
		Method:   method,
		Path:     path,
	}
	if len(query) > 0 {
		c.QueryParams = make(map[string]string, len(query))
		for k, vs := range query {
			if len(vs) != 1 {
				return "", fmt.Errorf("noah: query parameter %q must have exactly one value to be signed", k)
			}
			c.QueryParams[k] = vs[0]
		}
	}
	if len(body) > 0 {
		sum := sha256.Sum256(body)
		c.BodyHash = hex.EncodeToString(sum[:])
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("noah: encode claims: %w", err)
	}
	signingInput := jwtHeader + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha512.Sum384([]byte(signingInput))
	r, ss, err := s.ds.SignDigest(ctx, digest[:])
	if err != nil {
		return "", fmt.Errorf("noah: sign request: %w", err)
	}
	// r and s are below the curve order, so they fit 48 bytes; a signer
	// returning more is broken (FillBytes would panic).
	if r.BitLen() > 384 || ss.BitLen() > 384 || r.Sign() <= 0 || ss.Sign() <= 0 {
		return "", errors.New("noah: signer returned an out-of-range signature")
	}
	// JWS (RFC 7518 §3.4) uses the raw r||s, each left-padded to the
	// curve size (48 bytes for P-384), not the ASN.1 DER form.
	sig := make([]byte, 96)
	r.FillBytes(sig[:48])
	ss.FillBytes(sig[48:])
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// VerifyJWT checks an ES384 JWT against pub and returns its claims. It is
// what Noah does with Api-Signature; the fake Noah server and the tests
// use it to prove the signer's output.
func VerifyJWT(token string, pub *ecdsa.PublicKey) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, errors.New("jwt: want three segments")
	}
	if parts[0] != jwtHeader {
		return Claims{}, errors.New("jwt: unexpected header (want ES384 JWT)")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 96 {
		return Claims{}, errors.New("jwt: bad signature encoding")
	}
	digest := sha512.Sum384([]byte(parts[0] + "." + parts[1]))
	r := new(big.Int).SetBytes(sig[:48])
	s := new(big.Int).SetBytes(sig[48:])
	if !ecdsa.Verify(pub, digest[:], r, s) {
		return Claims{}, errors.New("jwt: signature does not verify")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, errors.New("jwt: bad payload encoding")
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return Claims{}, fmt.Errorf("jwt: payload: %w", err)
	}
	return c, nil
}
