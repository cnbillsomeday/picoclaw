package server

import (
	_ "embed"

	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/a2a/wire"
)

// ---------- 方案二：Ed25519 消息签名 ----------
//
// payload = "v1|{from}|{to}|{timestamp}|{nonce}|{sha256hex(content)}"
// sig     = hex(Ed25519(seed, payload_utf8))
//
// 每 agent 一对密钥：平台首次启动时生成 seed 存 agent_keys.json(600)，
// peer 通过 POST /a2a/keys/redeem（Bearer token，一次性）把 seed 领走。
// 也允许 peer 自持密钥：POST /a2a/keys/register 登记公钥。

//go:embed assets/sign.py
var signPySource string

//go:embed assets/signing.md
var signingDoc string

const signPayloadVersion = wire.SignPayloadVersion

type agentKeyRec struct {
	Slug       string     `json:"slug"`
	Seed       string     `json:"seed_base64url,omitempty"`
	ExtraPubs  []string   `json:"extra_public_keys,omitempty"`
	RedeemedAt *time.Time `json:"redeemed_at,omitempty"`
	// SeedRetiredAt 非空 = 该 agent 已登记自持公钥、平台托管的 seed 已删除，
	// 平台不再持有能代表它签名的私钥。
	SeedRetiredAt *time.Time `json:"seed_retired_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

type agentKeysFile struct {
	Agents []*agentKeyRec `json:"agents"`
}

// signMode: off = 不验；warn = 验但放行（记录）；strict = 无签名/验签失败一律 403
func normalizeSignMode(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "off", "warn", "strict":
		return strings.ToLower(strings.TrimSpace(v))
	}
	return ""
}

// effectiveSignMode resolves the signing mode for a server instance:
// explicit instance setting > A2A_SIGN_MODE env > warn (legacy default).
func effectiveSignMode(override string) string {
	if mode := normalizeSignMode(override); mode != "" {
		return mode
	}
	if mode := normalizeSignMode(os.Getenv("A2A_SIGN_MODE")); mode != "" {
		return mode
	}
	return "warn"
}

// loadAgentKeys 为每个 allowlist 成员准备密钥；缺失则生成。
func (c *Config) loadAgentKeys() error {
	path := filepath.Join(c.Dir, "agent_keys.json")
	c.agentKeysPath = path
	c.agentKey = map[string]*agentKeyRec{}
	c.agentPub = map[string]ed25519.PublicKey{}
	c.agentPubAll = map[string][]ed25519.PublicKey{}

	var f agentKeysFile
	b, err := os.ReadFile(path)
	if err == nil {
		if err := ensureSecureFile(path); err != nil {
			return err
		}
		if err := json.Unmarshal(b, &f); err != nil {
			return fmt.Errorf("agent_keys.json: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, r := range f.Agents {
		c.agentKey[r.Slug] = r
	}

	changed := false
	now := time.Now().UTC().Truncate(time.Second)
	for _, a := range c.Agents {
		r := c.agentKey[a.Slug]
		if r == nil {
			r = &agentKeyRec{Slug: a.Slug, CreatedAt: now}
			c.agentKey[a.Slug] = r
			changed = true
		}
		if r.Seed == "" && len(r.ExtraPubs) == 0 {
			seed := make([]byte, ed25519.SeedSize)
			if _, err := rand.Read(seed); err != nil {
				return err
			}
			r.Seed = base64.RawURLEncoding.EncodeToString(seed)
			changed = true
		}
		if r.Seed != "" {
			seed, err := base64.RawURLEncoding.DecodeString(r.Seed)
			if err != nil || len(seed) != ed25519.SeedSize {
				return fmt.Errorf("agent_keys.json: invalid seed for %s", a.Slug)
			}
			pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
			c.agentPub[a.Slug] = pub
			c.agentPubAll[a.Slug] = append(c.agentPubAll[a.Slug], pub)
		}
		for _, ep := range r.ExtraPubs {
			raw, err := decodeKeyBytes(ep)
			if err != nil || len(raw) != ed25519.PublicKeySize {
				continue
			}
			c.agentPubAll[a.Slug] = append(c.agentPubAll[a.Slug], ed25519.PublicKey(raw))
		}
	}
	if changed {
		f.Agents = f.Agents[:0]
		for _, a := range c.Agents {
			f.Agents = append(f.Agents, c.agentKey[a.Slug])
		}
		b, err := json.MarshalIndent(f, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, b, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func (c *Config) saveAgentKeys() error {
	f := agentKeysFile{}
	for _, a := range c.Agents {
		if r := c.agentKey[a.Slug]; r != nil {
			f.Agents = append(f.Agents, r)
		}
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.agentKeysPath, b, 0o600)
}

func decodeKeyBytes(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("empty key")
	}
	if raw, err := base64.RawURLEncoding.DecodeString(s); err == nil && len(raw) == 32 {
		return raw, nil
	}
	if raw, err := base64.StdEncoding.DecodeString(s); err == nil && len(raw) == 32 {
		return raw, nil
	}
	if raw, err := hex.DecodeString(s); err == nil && len(raw) == 32 {
		return raw, nil
	}
	return nil, errors.New("key must be 32 bytes: base64url or hex")
}

// SignPayload 是与客户端逐字节一致的待签串。
func SignPayload(from, to string, ts int64, nonce, content string) string {
	return wire.SignPayload(from, to, ts, nonce, content)
}

// VerifyMessage 用 from 名下已登记的任一公钥验签。
func (c *Config) VerifyMessage(from, to, content string, ts int64, nonce, sig string) (bool, string) {
	pubs := c.agentPubAll[from]
	if len(pubs) == 0 {
		return false, "no-public-key"
	}
	raw, err := hex.DecodeString(strings.TrimSpace(sig))
	if err != nil {
		if raw, err = base64.RawURLEncoding.DecodeString(strings.TrimSpace(sig)); err != nil {
			return false, "bad-sig-encoding"
		}
	}
	if len(raw) != ed25519.SignatureSize {
		return false, "bad-sig-length"
	}
	payload := []byte(SignPayload(from, to, ts, nonce, content))
	for _, p := range pubs {
		if ed25519.Verify(p, payload, raw) {
			return true, "ok"
		}
	}
	return false, "sig-mismatch"
}

// ---------- nonce 防重放 ----------

type nonceCache struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]time.Time
}

func newNonceCache(ttl time.Duration) *nonceCache {
	return &nonceCache{ttl: ttl, m: map[string]time.Time{}}
}

// Use 返回 key 是否首次出现（true = 新鲜可用）。key 建议 "from|nonce"。
func (n *nonceCache) Use(key string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := time.Now()
	for k, t := range n.m {
		if now.Sub(t) > n.ttl {
			delete(n.m, k)
		}
	}
	if _, ok := n.m[key]; ok {
		return false
	}
	n.m[key] = now
	return true
}

// ---------- HTTP handlers ----------

func (s *Server) handleSignPyHash(w http.ResponseWriter, r *http.Request) {
	sum := sha256.Sum256([]byte(signPySource))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(hex.EncodeToString(sum[:]) + "  sign.py\n"))
}

func (c *Config) redeemedCount() int {
	n := 0
	for _, r := range c.agentKey {
		if r.RedeemedAt != nil {
			n++
		}
	}
	return n
}

func (s *Server) handleSigningDoc(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	_, _ = w.Write([]byte(signingDoc))
}

func (s *Server) handleSignPy(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-python; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(signPySource))
}

// POST /a2a/keys/redeem  (Bearer) —— 一次性领取本 agent 私钥 seed
func (s *Server) handleRedeem(w http.ResponseWriter, r *http.Request, slug string) {
	rec := s.cfg.agentKey[slug]
	if rec == nil {
		writeErr(w, http.StatusForbidden, "agent not in allowlist")
		return
	}
	if rec.Seed == "" {
		writeErr(w, http.StatusConflict, "no platform-held seed for this agent; register your own public key instead")
		return
	}
	first := rec.RedeemedAt == nil
	if first {
		now := time.Now().UTC()
		rec.RedeemedAt = &now
		if err := s.cfg.saveAgentKeys(); err != nil {
			s.log.Printf("redeem: save error: %v", err)
			writeErr(w, http.StatusInternalServerError, "storage error")
			return
		}
	}
	pub := s.cfg.agentPub[slug]
	writeJSON(w, http.StatusOK, map[string]any{
		"success":         true,
		"slug":            slug,
		"seed_base64url":  rec.Seed,
		"public_key":      base64.RawURLEncoding.EncodeToString(pub),
		"first_redeem":    first,
		"redeemed_at":     rec.RedeemedAt,
		"keep_it_secret":  "store as $HOME/.a2a/ed25519.seed with chmod 600; never send it anywhere",
		"payload_version": signPayloadVersion,
	})
	s.log.Printf("key redeem: slug=%s first=%v", slug, first)
}

// POST /a2a/keys/register (Bearer) —— 登记自持公钥（客户端自己生成密钥对）
func (s *Server) handleRegisterKey(w http.ResponseWriter, r *http.Request, slug string) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var in struct {
		PublicKey       string `json:"public_key"`
		PublicKeyB64URL string `json:"public_key_b64url"`
		PublicKeyHex    string `json:"public_key_hex"`
		// RetirePlatformSeed 为 true 时，登记成功后删除平台托管的 seed，
		// 让 did.json 的 #key-1 变成 peer 自己的公钥（平台不再能为它签名）。
		RetirePlatformSeed bool `json:"retire_platform_seed"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	key := in.PublicKey
	if key == "" {
		key = in.PublicKeyB64URL
	}
	if key == "" {
		key = in.PublicKeyHex
	}
	raw, err := decodeKeyBytes(key)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		writeErr(w, http.StatusUnprocessableEntity, "public_key must be 32 bytes (base64url or hex)")
		return
	}
	rec := s.cfg.agentKey[slug]
	if rec == nil {
		writeErr(w, http.StatusForbidden, "agent not in allowlist")
		return
	}
	norm := base64.RawURLEncoding.EncodeToString(raw)
	for _, existing := range rec.ExtraPubs {
		if existing == norm {
			retired := false
			if in.RetirePlatformSeed {
				if err := s.retirePlatformSeed(slug); err == nil {
					retired = true
				}
			}
			writeJSON(w, http.StatusOK, map[string]any{"success": true, "already_registered": true, "public_key": norm,
				"platform_seed_retired": retired, "has_platform_seed": s.cfg.agentKey[slug].Seed != ""})
			return
		}
	}
	rec.ExtraPubs = append(rec.ExtraPubs, norm)
	if err := s.cfg.saveAgentKeys(); err != nil {
		s.log.Printf("register key: save error: %v", err)
		writeErr(w, http.StatusInternalServerError, "storage error")
		return
	}
	s.cfg.agentPubAll[slug] = append(s.cfg.agentPubAll[slug], ed25519.PublicKey(raw))
	s.log.Printf("key register: slug=%s extra=%d", slug, len(rec.ExtraPubs))
	retired := false
	if in.RetirePlatformSeed {
		if err := s.retirePlatformSeed(slug); err != nil {
			// 登记本身已成功，退休失败只提示，不假装成功
			writeJSON(w, http.StatusOK, map[string]any{"success": true, "public_key": norm,
				"extra_public_keys": rec.ExtraPubs, "platform_seed_retired": false, "retire_error": err.Error()})
			return
		}
		retired = true
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "public_key": norm, "extra_public_keys": rec.ExtraPubs,
		"platform_seed_retired": retired, "has_platform_seed": s.cfg.agentKey[slug].Seed != ""})
}

// retirePlatformSeed 删除平台托管 seed，只保留 peer 自持公钥。
// 前置：至少已登记一把自持公钥（否则会把账号弄成无人能签）。
// 之后 did.json 的 #key-1 即该 peer 自己的公钥，redeem 会回 409。
func (s *Server) retirePlatformSeed(slug string) error {
	rec := s.cfg.agentKey[slug]
	if rec == nil {
		return errors.New("agent not in allowlist")
	}
	if rec.Seed == "" {
		return nil // 幂等：已经退休
	}
	if len(rec.ExtraPubs) == 0 {
		return errors.New("no self-held public key registered yet; refusing to drop the platform seed")
	}
	now := time.Now().UTC()
	rec.Seed = ""
	rec.SeedRetiredAt = &now
	if err := s.cfg.saveAgentKeys(); err != nil {
		return err
	}
	if err := s.cfg.loadAgentKeys(); err != nil { // 重建 agentPub / agentPubAll 索引
		return err
	}
	s.log.Printf("key retire: slug=%s platform seed dropped", slug)
	return nil
}

// POST /a2a/keys/retire-seed (Bearer) —— 已登记自持公钥后撤销平台 seed
func (s *Server) handleRetireSeed(w http.ResponseWriter, r *http.Request, slug string) {
	if err := s.retirePlatformSeed(slug); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	rec := s.cfg.agentKey[slug]
	keys := make([]string, 0, 2)
	for _, p := range s.cfg.agentPubAll[slug] {
		keys = append(keys, base64.RawURLEncoding.EncodeToString(p))
	}
	out := map[string]any{"success": true, "slug": slug, "public_keys": keys,
		"has_platform_seed": rec != nil && rec.Seed != ""}
	if rec != nil {
		out["seed_retired_at"] = rec.SeedRetiredAt
	}
	writeJSON(w, http.StatusOK, out)
}

// GET /a2a/keys (Bearer) —— 查看本人密钥状态
func (s *Server) handleMyKeys(w http.ResponseWriter, r *http.Request, slug string) {
	rec := s.cfg.agentKey[slug]
	out := map[string]any{"success": true, "slug": slug, "sign_mode": s.signMode, "payload_version": signPayloadVersion}
	if rec != nil {
		out["has_platform_seed"] = rec.Seed != ""
		out["redeemed"] = rec.RedeemedAt != nil
		out["redeemed_at"] = rec.RedeemedAt
		out["extra_public_keys"] = rec.ExtraPubs
		out["seed_retired_at"] = rec.SeedRetiredAt
	}
	if pub := s.cfg.agentPub[slug]; pub != nil {
		out["public_key"] = base64.RawURLEncoding.EncodeToString(pub)
	}
	writeJSON(w, http.StatusOK, out)
}
