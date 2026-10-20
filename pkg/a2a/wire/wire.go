// PicoClaw - Ultra-lightweight personal AI agent
//
// Package wire holds the private A2A platform's wire contract shared by the
// built-in server and the client channel.
//
// IMPORTANT: this package is NOT the standard a2a-protocol.org protocol. It
// describes the self-hosted private direct-message platform (Bearer token +
// Ed25519 message signatures) that lives on its own port (default 21220).
package wire

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	// SignPayloadVersion is the prefix of every signed payload. It must match
	// the legacy server and sign.py byte for byte.
	SignPayloadVersion = "v1"

	// MaxMessageRunes is the platform's per-message body limit (UTF-8 runes).
	MaxMessageRunes = 8000

	// MaxBodyBytes is the server's request body limit for send requests.
	MaxBodyBytes = 64 << 10

	// NonceBytes is the size of the random nonce (hex-encoded) per message.
	NonceBytes = 16

	// SignatureSkew is the accepted clock skew for signed messages.
	SignatureSkew = 300 * time.Second
)

// SignPayload returns the exact byte string that must be signed:
//
//	v1|from|to|timestamp|nonce|sha256hex(content)
//
// The content is hashed as-is: never trim or normalize it before calling.
func SignPayload(from, to string, ts int64, nonce, content string) string {
	sum := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%s|%s|%s|%d|%s|%s",
		SignPayloadVersion, from, to, ts, nonce, hex.EncodeToString(sum[:]))
}

// Signer signs outbound messages with a per-agent Ed25519 seed.
type Signer struct {
	key ed25519.PrivateKey
}

// NewSigner builds a signer from a 32-byte Ed25519 seed.
func NewSigner(seed []byte) (*Signer, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("ed25519 seed must be %d bytes, got %d", ed25519.SeedSize, len(seed))
	}
	return &Signer{key: ed25519.NewKeyFromSeed(seed)}, nil
}

// LoadSigner reads a seed file. Accepted formats (matching sign.py):
//   - raw 32 bytes
//   - base64url text (padding optional)
//   - hex text
func LoadSigner(path string) (*Signer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) == ed25519.SeedSize {
		return NewSigner(raw)
	}
	seed, err := decodeSeedText(string(raw))
	if err != nil {
		return nil, fmt.Errorf("key file %s: %w", path, err)
	}
	return NewSigner(seed)
}

func decodeSeedText(text string) ([]byte, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errors.New("empty seed")
	}
	if b, err := decodeBase64Any(text); err == nil && len(b) == ed25519.SeedSize {
		return b, nil
	}
	if b, err := hex.DecodeString(text); err == nil && len(b) == ed25519.SeedSize {
		return b, nil
	}
	return nil, errors.New("cannot decode 32-byte seed (expected raw bytes, base64url or hex)")
}

func decodeBase64Any(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if pad := len(s) % 4; pad != 0 {
		s += strings.Repeat("=", 4-pad)
	}
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.StdEncoding.DecodeString(s)
}

// NewNonce returns a fresh random hex nonce.
func NewNonce() (string, error) {
	nonceBytes := make([]byte, NonceBytes)
	if _, err := rand.Read(nonceBytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(nonceBytes), nil
}

// Sign builds the wire fields for a message using the current clock and a
// fresh random nonce.
func (s *Signer) Sign(from, to, content string) (ts int64, nonce, sig string, err error) {
	nonce, err = NewNonce()
	if err != nil {
		return 0, "", "", err
	}
	ts, sig, err = s.SignWithNonce(from, to, content, nonce)
	return ts, nonce, sig, err
}

// SignWithNonce signs content at the current clock with a caller-provided
// nonce.
//
// Retries must keep the nonce (that is what keeps a resend idempotent: the
// platform consumes nonces and answers 403 "replayed nonce" for the copy that
// already landed) but they must refresh the timestamp. Reusing one signed
// payload across a retry chain with backoff drifts the timestamp past the
// server's +/-300s window, after which every attempt is rejected as stale.
func (s *Signer) SignWithNonce(from, to, content, nonce string) (int64, string, error) {
	nonce = strings.TrimSpace(nonce)
	if nonce == "" {
		return 0, "", errors.New("nonce is required")
	}
	ts := time.Now().Unix()
	return ts, s.SignAt(from, to, ts, nonce, content), nil
}

// SignAt signs with caller-provided timestamp and nonce (used by tests and
// when the exact signed bytes must be reproduced).
func (s *Signer) SignAt(from, to string, ts int64, nonce, content string) string {
	payload := SignPayload(from, to, ts, nonce, content)
	return hex.EncodeToString(ed25519.Sign(s.key, []byte(payload)))
}

// PublicKey returns the signer's public key.
func (s *Signer) PublicKey() ed25519.PublicKey {
	return s.key.Public().(ed25519.PublicKey)
}

// Verify checks a hex signature against a public key.
func Verify(pub ed25519.PublicKey, from, to string, ts int64, nonce, content, sig string) bool {
	raw, err := hex.DecodeString(strings.TrimSpace(sig))
	if err != nil || len(raw) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(pub, []byte(SignPayload(from, to, ts, nonce, content)), raw)
}
