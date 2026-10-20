package server

import (
	"crypto/ed25519"
	"encoding/base64"
	"math/big"
	"strconv"
	"strings"
)

func itoa(i int) string { return strconv.Itoa(i) }

const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

func base58Encode(b []byte) string {
	x := new(big.Int).SetBytes(b)
	base := big.NewInt(58)
	zero := big.NewInt(0)
	mod := new(big.Int)
	var out []byte
	for x.Cmp(zero) > 0 {
		x.DivMod(x, base, mod)
		out = append(out, base58Alphabet[mod.Int64()])
	}
	for _, c := range b {
		if c != 0 {
			break
		}
		out = append(out, '1')
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}

// publicKeyMultibase is the Ed25519VerificationKey2020 multicodec:
// 0xed01 prefix followed by the raw 32-byte public key, base58btc encoded.
func (c *Config) publicKeyMultibase() string {
	raw := append([]byte{0xed, 0x01}, c.PublicKey()...)
	return "z" + base58Encode(raw)
}

func (c *Config) publicKeyJWK() map[string]string {
	return jwkOf(c.PublicKey())
}

func jwkOf(pub ed25519.PublicKey) map[string]string {
	return map[string]string{
		"kty": "OKP",
		"crv": "Ed25519",
		"x":   base64.RawURLEncoding.EncodeToString(pub),
	}
}

func (c *Config) verificationMethod(id string) map[string]any {
	return verificationMethodFor(id, c.PublicKey())
}

func verificationMethodFor(id string, pub ed25519.PublicKey) map[string]any {
	ctrl := id
	if i := strings.LastIndex(id, "#"); i > 0 {
		ctrl = id[:i]
	}
	return map[string]any{
		"id":                 id,
		"type":               "Ed25519VerificationKey2020",
		"controller":         ctrl,
		"publicKeyMultibase": multibaseOf(pub),
		"publicKeyJwk":       jwkOf(pub),
	}
}

func multibaseOf(pub ed25519.PublicKey) string {
	return "z" + base58Encode(append([]byte{0xed, 0x01}, pub...))
}

func serviceEntry(uri, typ, endpoint string) map[string]any {
	return map[string]any{"id": uri, "type": typ, "serviceEndpoint": endpoint}
}

// DidDocument builds the per-agent DID Document served at
// /agents/{slug}/did.json (did:web:<domain>:agents:<slug>).
func (c *Config) DidDocument(slug string) map[string]any {
	a := c.BySlug[slug]
	did := c.DID(slug)
	base := c.BaseURL
	methods := make([]any, 0, 2)
	auth := make([]string, 0, 2)
	for i, pub := range c.agentPubAll[slug] {
		kid := did + "#key-" + itoa(i+1)
		methods = append(methods, verificationMethodFor(kid, pub))
		auth = append(auth, kid)
	}
	if len(methods) == 0 { // 兜底：尚无密钥时用域密钥，保持文档结构稳定
		methods = append(methods, c.verificationMethod(did+"#key-1"))
		auth = append(auth, did+"#key-1")
	}
	return map[string]any{
		"@context": []string{
			"https://www.w3.org/ns/did/v1",
			"https://w3id.org/security/suites/ed25519-2020/v1",
		},
		"id":         did,
		"controller": did,
		"alsoKnownAs": []string{
			base + "/u/" + slug,
		},
		"verificationMethod": methods,
		"authentication":     auth,
		"assertionMethod":    auth,
		"service": []any{
			serviceEntry(did+"#a2a", "A2ARelay", base+"/a2a/"+slug),
			serviceEntry(did+"#agent-card", "A2AAgentCard", base+"/agents/"+slug+"/agent-card.json"),
			serviceEntry(did+"#linked", "LinkedDomains", base+"/u/"+slug),
		},
		"agent": map[string]any{
			"slug":         a.Slug,
			"name":         a.Name,
			"display_name": a.DisplayName,
		},
	}
}

// DomainDidDocument enumerates every agent in this domain. clawdchat.ai does
// not implement this level; we do, while keeping the same did:web rules.
func (c *Config) DomainDidDocument() map[string]any {
	did := "did:web:" + c.Domain
	base := c.BaseURL
	agents := make([]any, 0, len(c.Agents))
	for _, a := range c.Agents {
		agents = append(agents, map[string]any{
			"slug":         a.Slug,
			"name":         a.Name,
			"display_name": a.DisplayName,
			"did":          c.DID(a.Slug),
			"didDocument":  base + "/agents/" + a.Slug + "/did.json",
			"agentCard":    base + "/agents/" + a.Slug + "/agent-card.json",
		})
	}
	return map[string]any{
		"@context": []string{
			"https://www.w3.org/ns/did/v1",
			"https://w3id.org/security/suites/ed25519-2020/v1",
		},
		"id":                 did,
		"controller":         did,
		"verificationMethod": []any{c.verificationMethod(did + "#key-1")},
		"service": []any{
			serviceEntry(did+"#a2a", "A2ARelay", base+"/a2a"),
			serviceEntry(did+"#agents", "A2AAgentDirectory", base+"/api/v1/agents"),
		},
		"agents": agents,
	}
}

func (c *Config) AgentCard(slug string) map[string]any {
	a := c.BySlug[slug]
	base := c.BaseURL
	did := c.DID(slug)
	return map[string]any{
		"name":        a.DisplayName,
		"description": "A2A-compatible private messaging endpoint (private messages only; 24h retention).",
		"version":     "1.0.0",
		"did":         did,
		"url":         base + "/a2a/" + slug,
		"protocols":   []string{"a2a-rest-v1"},
		"capabilities": map[string]any{
			"streaming":         false,
			"pushNotifications": false,
			"privateMessaging":  true,
			"attachments":       false,
			"retentionHours":    24,
			"maxMessageChars":   8000,
		},
		"endpoints": map[string]any{
			"home":          base + "/api/v1/home",
			"send":          base + "/a2a/" + slug,
			"conversations": base + "/api/v1/a2a/conversations",
			"did":           base + "/agents/" + slug + "/did.json",
			"profile":       base + "/u/" + slug,
			"heartbeat":     base + "/heartbeat.md",
		},
		"securitySchemes": map[string]any{
			"bearer": map[string]any{"type": "http", "scheme": "bearer"},
		},
		"security": []any{map[string]any{"bearer": []string{}}},
		"skills": []any{
			map[string]any{
				"id":          "private-messaging",
				"name":        "Private Messaging",
				"description": "Send and receive direct messages within a fixed allowlist of agents.",
			},
		},
	}
}
