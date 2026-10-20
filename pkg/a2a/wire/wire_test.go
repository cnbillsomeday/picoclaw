// PicoClaw - Ultra-lightweight personal AI agent

package wire

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type signVectorFile struct {
	SeedHex      string       `json:"seed_hex"`
	PubkeyBase64 string       `json:"pubkey_base64url"`
	Vectors      []signVector `json:"vectors"`
}

type signVector struct {
	From    string `json:"from"`
	To      string `json:"to"`
	TS      int64  `json:"ts"`
	Nonce   string `json:"nonce"`
	Content string `json:"content"`
	Payload string `json:"payload"`
	Sig     string `json:"sig"`
}

func loadSignVectors(t *testing.T) signVectorFile {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "sign_vectors.json"))
	if err != nil {
		t.Fatalf("read sign vectors: %v", err)
	}
	var f signVectorFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("decode sign vectors: %v", err)
	}
	return f
}

// TestSignPayloadMatchesSignPy is the byte-for-byte parity check against the
// legacy Python implementation (assets/sign.py). Any change here breaks the
// wire contract and will make every strict-signed message fail with 403.
func TestSignPayloadMatchesSignPy(t *testing.T) {
	f := loadSignVectors(t)
	for _, v := range f.Vectors {
		got := SignPayload(v.From, v.To, v.TS, v.Nonce, v.Content)
		if got != v.Payload {
			t.Fatalf("SignPayload(%q,%q,...) = %q, want %q", v.From, v.To, got, v.Payload)
		}
	}
}

func TestSignerMatchesSignPy(t *testing.T) {
	f := loadSignVectors(t)
	seed, err := hex.DecodeString(f.SeedHex)
	if err != nil {
		t.Fatalf("decode seed: %v", err)
	}
	signer, err := NewSigner(seed)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	if got := base64.RawURLEncoding.EncodeToString(signer.PublicKey()); got != f.PubkeyBase64 {
		t.Fatalf("public key = %q, want %q", got, f.PubkeyBase64)
	}
	for _, v := range f.Vectors {
		got := signer.SignAt(v.From, v.To, v.TS, v.Nonce, v.Content)
		if got != v.Sig {
			t.Fatalf("signature for %q -> %q = %q, want %q", v.From, v.To, got, v.Sig)
		}
		if !Verify(signer.PublicKey(), v.From, v.To, v.TS, v.Nonce, v.Content, v.Sig) {
			t.Fatalf("Verify rejected a valid vector for %q -> %q", v.From, v.To)
		}
		if Verify(signer.PublicKey(), v.From, v.To, v.TS, v.Nonce, v.Content+"x", v.Sig) {
			t.Fatalf("Verify accepted tampered content for %q -> %q", v.From, v.To)
		}
	}
}

func TestLoadSignerFormats(t *testing.T) {
	f := loadSignVectors(t)
	seed, err := hex.DecodeString(f.SeedHex)
	if err != nil {
		t.Fatalf("decode seed: %v", err)
	}
	dir := t.TempDir()
	cases := map[string][]byte{
		"raw.seed":    seed,
		"b64.seed":    []byte(base64.RawURLEncoding.EncodeToString(seed)),
		"b64pad.seed": []byte(base64.URLEncoding.EncodeToString(seed)),
		"hex.seed":    []byte(f.SeedHex),
		"hexnl.seed":  []byte(f.SeedHex + "\n"),
	}
	for name, content := range cases {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		signer, err := LoadSigner(path)
		if err != nil {
			t.Fatalf("LoadSigner(%s): %v", name, err)
		}
		got := base64.RawURLEncoding.EncodeToString(signer.PublicKey())
		if got != f.PubkeyBase64 {
			t.Fatalf("LoadSigner(%s) public key = %q, want %q", name, got, f.PubkeyBase64)
		}
	}
}
