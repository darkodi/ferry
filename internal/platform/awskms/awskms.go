// Package awskms keeps Ferry's private keys inside AWS KMS. Ferry asks KMS
// to sign or decrypt and never holds the key material:
//
//   - ECDSA (ECC_NIST_P384, ECDSA_SHA_384): Noah request signing;
//   - Ed25519 (ECC_NIST_EDWARDS25519, ED25519_SHA_512): the Solana
//     authority that creates deposit accounts;
//   - symmetric: the key-encryption key that wraps FERRY_DATA_KEY.
//
// Each signer checks its key's public key when it is created, so a
// wrong key id or key spec fails at startup, not at the first payout.
package awskms

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"
)

// API is the part of the KMS client Ferry uses (*kms.Client satisfies it).
type API interface {
	Sign(ctx context.Context, in *kms.SignInput, optFns ...func(*kms.Options)) (*kms.SignOutput, error)
	GetPublicKey(ctx context.Context, in *kms.GetPublicKeyInput, optFns ...func(*kms.Options)) (*kms.GetPublicKeyOutput, error)
	Decrypt(ctx context.Context, in *kms.DecryptInput, optFns ...func(*kms.Options)) (*kms.DecryptOutput, error)
}

// New returns a KMS client from the standard AWS environment (region,
// credentials; on ECS the task role). endpoint overrides the KMS endpoint
// (e.g. LocalStack); leave it empty for AWS.
func New(ctx context.Context, endpoint string) (*kms.Client, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("awskms: load AWS config: %w", err)
	}
	return kms.NewFromConfig(cfg, func(o *kms.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
	}), nil
}

// ---- ECDSA P-384 ----

// ECDSASigner signs SHA-384 digests with a KMS P-384 key.
type ECDSASigner struct {
	api   API
	keyID string
	pub   *ecdsa.PublicKey
}

// NewECDSASigner checks that keyID is a P-384 signing key and returns its
// signer.
func NewECDSASigner(ctx context.Context, api API, keyID string) (*ECDSASigner, error) {
	pub, err := publicKey(ctx, api, keyID)
	if err != nil {
		return nil, err
	}
	ek, ok := pub.(*ecdsa.PublicKey)
	if !ok || ek.Curve != elliptic.P384() {
		return nil, fmt.Errorf("awskms: key %s is not an ECC_NIST_P384 key", keyID)
	}
	return &ECDSASigner{api: api, keyID: keyID, pub: ek}, nil
}

// Public returns the key's public half.
func (s *ECDSASigner) Public() *ecdsa.PublicKey { return s.pub }

// SignDigest signs a SHA-384 digest. KMS returns an ASN.1 DER signature;
// it is checked against the public key before it is returned.
func (s *ECDSASigner) SignDigest(ctx context.Context, digest []byte) (r, ss *big.Int, err error) {
	if len(digest) != 48 {
		return nil, nil, fmt.Errorf("awskms: want a 48-byte SHA-384 digest, got %d bytes", len(digest))
	}
	out, err := s.api.Sign(ctx, &kms.SignInput{
		KeyId:            aws.String(s.keyID),
		Message:          digest,
		MessageType:      types.MessageTypeDigest, // already hashed: KMS must not hash again
		SigningAlgorithm: types.SigningAlgorithmSpecEcdsaSha384,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("awskms: sign with %s: %w", s.keyID, err)
	}
	r, ss, err = ParseDER(out.Signature)
	if err != nil {
		return nil, nil, err
	}
	if !ecdsa.Verify(s.pub, digest, r, ss) {
		return nil, nil, fmt.Errorf("awskms: signature from %s does not verify", s.keyID)
	}
	return r, ss, nil
}

// ParseDER reads an ASN.1 DER ECDSA signature, SEQUENCE { r INTEGER,
// s INTEGER }, into r and s. Both must be positive and nothing may follow.
func ParseDER(der []byte) (r, s *big.Int, err error) {
	var sig struct{ R, S *big.Int }
	rest, err := asn1.Unmarshal(der, &sig)
	if err != nil {
		return nil, nil, fmt.Errorf("awskms: signature is not DER: %w", err)
	}
	if len(rest) != 0 {
		return nil, nil, errors.New("awskms: trailing bytes after the DER signature")
	}
	if sig.R == nil || sig.S == nil || sig.R.Sign() <= 0 || sig.S.Sign() <= 0 {
		return nil, nil, errors.New("awskms: signature r and s must be positive")
	}
	return sig.R, sig.S, nil
}

// ---- Ed25519 ----

// Ed25519Signer signs messages with a KMS Ed25519 key (pure EdDSA).
type Ed25519Signer struct {
	api   API
	keyID string
	pub   ed25519.PublicKey
}

// NewEd25519Signer checks that keyID is an ECC_NIST_EDWARDS25519 signing
// key and returns its signer.
func NewEd25519Signer(ctx context.Context, api API, keyID string) (*Ed25519Signer, error) {
	pub, err := publicKey(ctx, api, keyID)
	if err != nil {
		return nil, err
	}
	ek, ok := pub.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("awskms: key %s is not an ECC_NIST_EDWARDS25519 key", keyID)
	}
	return &Ed25519Signer{api: api, keyID: keyID, pub: ek}, nil
}

// Public returns the key's 32-byte public key.
func (s *Ed25519Signer) Public() ed25519.PublicKey { return s.pub }

// Sign signs msg with ED25519_SHA_512, which KMS only accepts as a RAW
// message (pure EdDSA hashes internally). The signature is checked
// against the public key before it is returned.
func (s *Ed25519Signer) Sign(ctx context.Context, msg []byte) ([]byte, error) {
	out, err := s.api.Sign(ctx, &kms.SignInput{
		KeyId:            aws.String(s.keyID),
		Message:          msg,
		MessageType:      types.MessageTypeRaw,
		SigningAlgorithm: types.SigningAlgorithmSpecEd25519Sha512,
	})
	if err != nil {
		return nil, fmt.Errorf("awskms: sign with %s: %w", s.keyID, err)
	}
	if len(out.Signature) != ed25519.SignatureSize || !ed25519.Verify(s.pub, msg, out.Signature) {
		return nil, fmt.Errorf("awskms: signature from %s does not verify", s.keyID)
	}
	return out.Signature, nil
}

func publicKey(ctx context.Context, api API, keyID string) (any, error) {
	if keyID == "" {
		return nil, errors.New("awskms: key id is empty")
	}
	out, err := api.GetPublicKey(ctx, &kms.GetPublicKeyInput{KeyId: aws.String(keyID)})
	if err != nil {
		return nil, fmt.Errorf("awskms: public key of %s: %w", keyID, err)
	}
	if out.KeyUsage != types.KeyUsageTypeSignVerify {
		return nil, fmt.Errorf("awskms: key %s is for %s, not signing", keyID, out.KeyUsage)
	}
	pub, err := x509.ParsePKIXPublicKey(out.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("awskms: parse public key of %s: %w", keyID, err)
	}
	return pub, nil
}

// ---- data key ----

// DecryptDataKey unwraps an envelope-encrypted data key: wrapped is the
// base64 CiphertextBlob from `aws kms encrypt` (or GenerateDataKey) of a
// 32-byte key under a symmetric KMS key. The plaintext lives only in
// memory; the wrapped form can sit in configuration.
func DecryptDataKey(ctx context.Context, api API, wrapped string) ([]byte, error) {
	blob, err := base64.StdEncoding.DecodeString(strings.TrimSpace(wrapped))
	if err != nil {
		return nil, fmt.Errorf("awskms: wrapped data key is not base64: %w", err)
	}
	out, err := api.Decrypt(ctx, &kms.DecryptInput{CiphertextBlob: blob})
	if err != nil {
		return nil, fmt.Errorf("awskms: decrypt data key: %w", err)
	}
	if len(out.Plaintext) != 32 {
		return nil, fmt.Errorf("awskms: data key must be 32 bytes, got %d", len(out.Plaintext))
	}
	return out.Plaintext, nil
}
