package awskms_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha512"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"errors"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"

	"github.com/darkodi/ferry/internal/platform/awskms"
	"github.com/darkodi/ferry/internal/rail/noah"
)

// fakeKMS answers like KMS with real keys, and insists on the request
// shapes KMS requires for each key spec.
type fakeKMS struct {
	p384    *ecdsa.PrivateKey
	ed      ed25519.PrivateKey
	other   *ecdsa.PrivateKey // forge: sign with a key that is not the one advertised
	forge   bool
	wrapped map[string][]byte
}

func newFake(t *testing.T) *fakeKMS {
	t.Helper()
	p, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	o, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	_, ed, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &fakeKMS{p384: p, ed: ed, other: o, wrapped: map[string][]byte{}}
}

func (f *fakeKMS) Sign(_ context.Context, in *kms.SignInput, _ ...func(*kms.Options)) (*kms.SignOutput, error) {
	switch *in.KeyId {
	case "p384":
		if in.MessageType != types.MessageTypeDigest || in.SigningAlgorithm != types.SigningAlgorithmSpecEcdsaSha384 || len(in.Message) != 48 {
			return nil, errors.New("fake KMS: P-384 wants a 48-byte DIGEST with ECDSA_SHA_384")
		}
		key := f.p384
		if f.forge {
			key = f.other
		}
		sig, err := ecdsa.SignASN1(rand.Reader, key, in.Message)
		return &kms.SignOutput{Signature: sig}, err
	case "ed":
		if in.MessageType != types.MessageTypeRaw || in.SigningAlgorithm != types.SigningAlgorithmSpecEd25519Sha512 {
			return nil, errors.New("fake KMS: Ed25519 wants a RAW message with ED25519_SHA_512")
		}
		sig := ed25519.Sign(f.ed, in.Message)
		if f.forge {
			sig[0] ^= 1
		}
		return &kms.SignOutput{Signature: sig}, nil
	}
	return nil, errors.New("fake KMS: no such key")
}

func (f *fakeKMS) GetPublicKey(_ context.Context, in *kms.GetPublicKeyInput, _ ...func(*kms.Options)) (*kms.GetPublicKeyOutput, error) {
	var pub any
	usage := types.KeyUsageTypeSignVerify
	switch *in.KeyId {
	case "p384":
		pub = &f.p384.PublicKey
	case "ed":
		pub = f.ed.Public()
	case "p256":
		k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		pub = &k.PublicKey
	case "p384-encrypt":
		pub, usage = &f.p384.PublicKey, types.KeyUsageTypeEncryptDecrypt
	default:
		return nil, errors.New("fake KMS: no such key")
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	return &kms.GetPublicKeyOutput{PublicKey: der, KeyUsage: usage}, err
}

func (f *fakeKMS) Decrypt(_ context.Context, in *kms.DecryptInput, _ ...func(*kms.Options)) (*kms.DecryptOutput, error) {
	pt, ok := f.wrapped[string(in.CiphertextBlob)]
	if !ok {
		return nil, errors.New("fake KMS: InvalidCiphertextException")
	}
	return &kms.DecryptOutput{Plaintext: pt}, nil
}

// A Noah request signed through KMS verifies as an ES384 JWT with the
// KMS key's public half: the DER signature is converted to r||s.
func TestNoahSignsThroughKMS(t *testing.T) {
	f := newFake(t)
	ks, err := awskms.NewECDSASigner(t.Context(), f, "p384")
	if err != nil {
		t.Fatal(err)
	}
	signer, err := noah.NewDigestSigner(ks, ks.Public(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	for range 20 { // r and s of varying length, some with leading zero bytes
		tok, err := signer.Sign(t.Context(), "POST", "/v1/transactions/sell", nil, []byte(`{"Nonce":"n"}`))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := noah.VerifyJWT(tok, &f.p384.PublicKey); err != nil {
			t.Fatalf("KMS-signed JWT does not verify: %v", err)
		}
	}
}

// A Solana-style message signed through KMS verifies with ed25519.
func TestEd25519SignsThroughKMS(t *testing.T) {
	f := newFake(t)
	s, err := awskms.NewEd25519Signer(t.Context(), f, "ed")
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("a serialized Solana message")
	sig, err := s.Sign(t.Context(), msg)
	if err != nil || !ed25519.Verify(f.ed.Public().(ed25519.PublicKey), msg, sig) {
		t.Fatalf("signature %x, %v", sig, err)
	}
	if !bytes.Equal(s.Public(), f.ed.Public().(ed25519.PublicKey)) {
		t.Fatal("public key differs")
	}
}

// A wrong key id, spec or usage fails when the signer is created.
func TestKeySpecChecked(t *testing.T) {
	f := newFake(t)
	ctx := t.Context()
	for name, err := range map[string]error{
		"ed25519 as ECDSA": second(awskms.NewECDSASigner(ctx, f, "ed")),
		"P-256 as P-384":   second(awskms.NewECDSASigner(ctx, f, "p256")),
		"P-384 as Ed25519": second(awskms.NewEd25519Signer(ctx, f, "p384")),
		"encryption key":   second(awskms.NewECDSASigner(ctx, f, "p384-encrypt")),
		"unknown key":      second(awskms.NewECDSASigner(ctx, f, "nope")),
		"empty key id":     second(awskms.NewEd25519Signer(ctx, f, "")),
	} {
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func second[T any](_ T, err error) error { return err }

// A signature that does not verify against the advertised public key is
// refused, whatever KMS (or something pretending to be it) returns.
func TestForgedSignaturesRefused(t *testing.T) {
	f := newFake(t)
	ks, _ := awskms.NewECDSASigner(t.Context(), f, "p384")
	es, _ := awskms.NewEd25519Signer(t.Context(), f, "ed")
	f.forge = true
	digest := sha512.Sum384([]byte("x"))
	if _, _, err := ks.SignDigest(t.Context(), digest[:]); err == nil {
		t.Error("an ECDSA signature by another key was accepted")
	}
	if _, err := es.Sign(t.Context(), []byte("x")); err == nil {
		t.Error("a corrupted Ed25519 signature was accepted")
	}
	if _, _, err := ks.SignDigest(t.Context(), []byte("short")); err == nil {
		t.Error("a non-SHA-384 digest was sent to KMS")
	}
}

func TestDecryptDataKey(t *testing.T) {
	f := newFake(t)
	key := bytes.Repeat([]byte{7}, 32)
	f.wrapped["blob-ok"] = key
	f.wrapped["blob-short"] = key[:16]
	got, err := awskms.DecryptDataKey(t.Context(), f, base64.StdEncoding.EncodeToString([]byte("blob-ok")))
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("decrypt: %x, %v", got, err)
	}
	for name, wrapped := range map[string]string{
		"not base64":   "%%%",
		"16-byte key":  base64.StdEncoding.EncodeToString([]byte("blob-short")),
		"unknown blob": base64.StdEncoding.EncodeToString([]byte("tampered")),
	} {
		if _, err := awskms.DecryptDataKey(t.Context(), f, wrapped); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseDER(t *testing.T) {
	good, _ := asn1.Marshal(struct{ R, S *big.Int }{big.NewInt(5), big.NewInt(7)})
	r, s, err := awskms.ParseDER(good)
	if err != nil || r.Int64() != 5 || s.Int64() != 7 {
		t.Fatalf("good: %v %v %v", r, s, err)
	}
	zero, _ := asn1.Marshal(struct{ R, S *big.Int }{big.NewInt(0), big.NewInt(7)})
	neg, _ := asn1.Marshal(struct{ R, S *big.Int }{big.NewInt(-5), big.NewInt(7)})
	for name, der := range map[string][]byte{
		"empty": nil, "garbage": []byte("x"), "trailing": append(append([]byte{}, good...), 0),
		"zero r": zero, "negative r": neg,
	} {
		if _, _, err := awskms.ParseDER(der); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Whatever bytes come back as a signature, ParseDER never panics, and
// what it accepts is canonical DER of positive integers.
func FuzzParseDER(f *testing.F) {
	good, _ := asn1.Marshal(struct{ R, S *big.Int }{big.NewInt(5), big.NewInt(7)})
	f.Add(good)
	f.Add([]byte{0x30, 0x06, 0x02, 0x01, 0x00, 0x02, 0x01, 0x01})
	f.Add([]byte{0x30, 0x80})
	f.Fuzz(func(t *testing.T, der []byte) {
		r, s, err := awskms.ParseDER(der)
		if err != nil {
			return
		}
		if r.Sign() <= 0 || s.Sign() <= 0 {
			t.Fatalf("accepted non-positive r or s from %x", der)
		}
		again, _ := asn1.Marshal(struct{ R, S *big.Int }{r, s})
		if !bytes.Equal(again, der) {
			t.Fatalf("accepted non-canonical DER %x", der)
		}
	})
}

// Against real AWS KMS, when keys are provided:
//
//	FERRY_TEST_KMS_P384_KEY_ID=... FERRY_TEST_KMS_ED25519_KEY_ID=... go test ./internal/platform/awskms -run RealKMS
func TestRealKMS(t *testing.T) {
	p384, ed := os.Getenv("FERRY_TEST_KMS_P384_KEY_ID"), os.Getenv("FERRY_TEST_KMS_ED25519_KEY_ID")
	if p384 == "" && ed == "" {
		t.Skip("set FERRY_TEST_KMS_P384_KEY_ID and/or FERRY_TEST_KMS_ED25519_KEY_ID to test against AWS KMS")
	}
	api, err := awskms.New(t.Context(), os.Getenv("FERRY_KMS_ENDPOINT"))
	if err != nil {
		t.Fatal(err)
	}
	if p384 != "" {
		ks, err := awskms.NewECDSASigner(t.Context(), api, p384)
		if err != nil {
			t.Fatal(err)
		}
		signer, _ := noah.NewDigestSigner(ks, ks.Public(), time.Now)
		tok, err := signer.Sign(t.Context(), "GET", "/v1/balances", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := noah.VerifyJWT(tok, ks.Public()); err != nil {
			t.Fatal(err)
		}
	}
	if ed != "" {
		es, err := awskms.NewEd25519Signer(t.Context(), api, ed)
		if err != nil {
			t.Fatal(err)
		}
		msg := []byte(strings.Repeat("m", 300))
		sig, err := es.Sign(t.Context(), msg)
		if err != nil || !ed25519.Verify(es.Public(), msg, sig) {
			t.Fatalf("%x %v", sig, err)
		}
	}
}
