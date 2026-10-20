package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// AgentConfig is one allowlisted agent. The allowlist is the authorization
// boundary: an unknown recipient is rejected before a message is stored.
type AgentConfig struct {
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	DisplayName string    `json:"display_name"`
	CreatedAt   time.Time `json:"created_at"`
}

type allowlistFile struct {
	Agents []*AgentConfig `json:"agents"`
}

type tokenRecord struct {
	Slug   string `json:"slug"`
	SHA256 string `json:"sha256"`
}

type tokensFile struct {
	Tokens []tokenRecord `json:"tokens"`
}

type keysFile struct {
	Seed      string    `json:"ed25519_seed_base64url"`
	CreatedAt time.Time `json:"created_at"`
}

// Config holds loaded agents, token hashes and the identity key.
type Config struct {
	Dir       string
	Domain    string
	BaseURL   string
	Agents    []*AgentConfig
	BySlug    map[string]*AgentConfig
	tokenHash map[string]string // sha256 hex -> slug
	keys      ed25519.PrivateKey
	keySeed   []byte
	tokenPath string

	// 方案二：每 agent 一枚 Ed25519 密钥（平台生成 seed，一次性 redeem）
	agentKeysPath string
	agentKey      map[string]*agentKeyRec
	agentPub      map[string]ed25519.PublicKey
	agentPubAll   map[string][]ed25519.PublicKey
}

func defaultAgents() []*AgentConfig {
	now := time.Now().UTC().Truncate(time.Second)
	specs := []struct{ slug, name string }{
		{"alice", "Alice"},
		{"bob", "Bob"},
		{"carol", "Carol"},
		{"dave", "Dave"},
	}
	out := make([]*AgentConfig, 0, len(specs))
	for _, s := range specs {
		out = append(out, &AgentConfig{Slug: s.slug, Name: s.name, DisplayName: s.name, CreatedAt: now})
	}
	return out
}

func normalizeDomain(d string) string {
	d = strings.TrimSpace(d)
	d = strings.TrimPrefix(d, "https://")
	d = strings.TrimPrefix(d, "http://")
	d = strings.TrimSuffix(d, "/")
	return d
}

func ensureSecureFile(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is group/world writable (mode %o); chmod 600", path, st.Mode().Perm())
	}
	return nil
}

// LoadConfig initialises the directory on first run: allowlist, tokens and the
// ed25519 identity key. It never overwrites an existing key or token.
//
// When allowlist.json is missing:
//   - allowDefaultAgents=false (production default): refuse to start and point
//     the operator at `picoclaw a2a-server init`.
//   - allowDefaultAgents=true: generate the legacy alice/bob/carol/dave list
//     (development and compatibility tests only).
func LoadConfig(dir, domain string, allowDefaultAgents bool) (*Config, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	c := &Config{
		Dir:       dir,
		Domain:    normalizeDomain(domain),
		BaseURL:   "https://" + normalizeDomain(domain),
		BySlug:    map[string]*AgentConfig{},
		tokenHash: map[string]string{},
	}

	// ---- allowlist ----
	allowPath := filepath.Join(dir, "allowlist.json")
	var al allowlistFile
	b, err := os.ReadFile(allowPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if !allowDefaultAgents {
			return nil, fmt.Errorf(
				"%s is missing: run 'picoclaw a2a-server init --agents <slug,...>' "+
					"(or set a2a_server.allow_default_agents=true for development only)",
				allowPath,
			)
		}
		al.Agents = defaultAgents()
		b, _ = json.MarshalIndent(al, "", "  ")
		if err := os.WriteFile(allowPath, b, 0o600); err != nil {
			return nil, err
		}
	} else {
		if err := ensureSecureFile(allowPath); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(b, &al); err != nil {
			return nil, fmt.Errorf("allowlist.json: %w", err)
		}
	}
	if len(al.Agents) == 0 {
		return nil, errors.New("allowlist.json contains no agents")
	}
	for _, a := range al.Agents {
		a.Slug = strings.TrimSpace(a.Slug)
		if a.Slug == "" || strings.ContainsAny(a.Slug, "/ :?#") {
			return nil, fmt.Errorf("invalid slug %q", a.Slug)
		}
		if a.Name == "" {
			a.Name = a.Slug
		}
		if a.DisplayName == "" {
			a.DisplayName = a.Name
		}
		if a.CreatedAt.IsZero() {
			a.CreatedAt = time.Now().UTC()
		}
		if _, dup := c.BySlug[a.Slug]; dup {
			return nil, fmt.Errorf("duplicate slug %q", a.Slug)
		}
		c.BySlug[a.Slug] = a
		c.Agents = append(c.Agents, a)
	}

	// ---- tokens ----
	tokensPath := filepath.Join(dir, "tokens.json")
	plainPath := filepath.Join(dir, "tokens.txt")
	hashes := map[string]string{} // slug -> hash
	var tf tokensFile
	if b, err := os.ReadFile(tokensPath); err == nil {
		if err := ensureSecureFile(tokensPath); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(b, &tf); err != nil {
			return nil, fmt.Errorf("tokens.json: %w", err)
		}
		for _, t := range tf.Tokens {
			hashes[t.Slug] = t.SHA256
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	plain := map[string]string{}
	if b, err := os.ReadFile(plainPath); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.Split(line, "\t")
			if len(parts) >= 3 {
				plain[parts[0]] = parts[2]
			}
		}
	}

	changed := false
	for _, a := range c.Agents {
		if hashes[a.Slug] != "" {
			continue
		}
		tok := plain[a.Slug]
		if tok == "" {
			tok = randToken()
			halves := sha256.Sum256([]byte(tok))
			hashes[a.Slug] = hex.EncodeToString(halves[:])
			plain[a.Slug] = tok
			changed = true
		} else {
			sum := sha256.Sum256([]byte(tok))
			hashes[a.Slug] = hex.EncodeToString(sum[:])
			changed = true
		}
	}
	if changed || tf.Tokens == nil {
		tf.Tokens = tf.Tokens[:0]
		for _, a := range c.Agents {
			tf.Tokens = append(tf.Tokens, tokenRecord{Slug: a.Slug, SHA256: hashes[a.Slug]})
		}
		b, _ := json.MarshalIndent(tf, "", "  ")
		if err := os.WriteFile(tokensPath, b, 0o600); err != nil {
			return nil, err
		}
	}
	c.tokenPath = tokensPath
	for slug, h := range hashes {
		c.tokenHash[h] = slug
	}
	// Rewrite tokens.txt only when we know every plaintext token, so we never
	// clobber a file that is the sole copy of another agent's credential.
	allPlain := true
	for _, a := range c.Agents {
		if plain[a.Slug] == "" {
			allPlain = false
		}
	}
	if allPlain {
		var sb strings.Builder
		sb.WriteString("# A2A tokens — confidential. chmod 600. One line per agent.\n")
		sb.WriteString("# slug\tname\ttoken\n")
		for _, a := range c.Agents {
			sb.WriteString(a.Slug + "\t" + a.Name + "\t" + plain[a.Slug] + "\n")
		}
		if err := os.WriteFile(plainPath, []byte(sb.String()), 0o600); err != nil {
			return nil, err
		}
	} else if _, err := os.Stat(plainPath); errors.Is(err, os.ErrNotExist) {
		_ = os.WriteFile(plainPath, []byte("# Some existing token hashes have no plaintext copy.\n# Delete tokens.json and restart to reissue all tokens.\n"), 0o600)
	}

	// ---- identity key ----
	keysPath := filepath.Join(dir, "keys.json")
	var kf keysFile
	b, err = os.ReadFile(keysPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		seed := make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return nil, err
		}
		kf.Seed = base64.RawURLEncoding.EncodeToString(seed)
		kf.CreatedAt = time.Now().UTC()
		b, err = json.MarshalIndent(kf, "", "  ")
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(keysPath, b, 0o600); err != nil {
			return nil, err
		}
	} else if err := ensureSecureFile(keysPath); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &kf); err != nil {
		return nil, fmt.Errorf("keys.json: %w", err)
	}
	seed, err := base64.RawURLEncoding.DecodeString(kf.Seed)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("keys.json: invalid ed25519 seed")
	}
	c.keySeed = seed
	c.keys = ed25519.NewKeyFromSeed(seed)

	// ---- per-agent signing keys ----
	if err := c.loadAgentKeys(); err != nil {
		return nil, err
	}
	return c, nil
}

func randToken() string {
	b := make([]byte, 32) // 256 bits
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func (c *Config) Agent(slug string) *AgentConfig { return c.BySlug[slug] }

// ResolveAgent resolves a slug case-insensitively and returns the canonical
// allowlist entry. Exact matches win; the fold is only a compatibility net so
// "Alice" and "alice" cannot become two different (one unreachable) peers.
func (c *Config) ResolveAgent(name string) *AgentConfig {
	name = strings.TrimSpace(name)
	if a := c.BySlug[name]; a != nil {
		return a
	}
	for _, a := range c.Agents {
		if strings.EqualFold(a.Slug, name) {
			return a
		}
	}
	return nil
}

func (c *Config) Auth(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	sum := sha256.Sum256([]byte(token))
	slug, ok := c.tokenHash[hex.EncodeToString(sum[:])]
	return slug, ok
}

func (c *Config) PublicKey() ed25519.PublicKey {
	return c.keys.Public().(ed25519.PublicKey)
}

func (c *Config) DID(slug string) string {
	return "did:web:" + c.Domain + ":agents:" + slug
}
