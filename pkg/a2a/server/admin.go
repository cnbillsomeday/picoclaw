// PicoClaw - Ultra-lightweight personal AI agent

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
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// InitOptions configures `picoclaw a2a-server init`.
type InitOptions struct {
	DataDir   string
	Domain    string
	Agents    []string // fresh init: agents to create
	AddAgents []string // existing data: append these agents and mint tokens
}

// InitResult reports the newly created plaintext tokens (shown once).
type InitResult struct {
	DataDir string
	Added   []string
	Tokens  map[string]string
}

var (
	allowlistFileName = "allowlist.json"
	tokensFileName    = "tokens.json"
	tokensPlainName   = "tokens.txt"
	keysFileName      = "keys.json"
	agentKeysName     = "agent_keys.json"
	dataFileName      = "data.json"
	presenceFileName  = "presence.json"
	lockFileName      = ".a2a.lock"
)

func normalizeSlug(raw string) (string, error) {
	slug := strings.TrimSpace(raw)
	if slug == "" {
		return "", errors.New("empty agent slug")
	}
	if strings.ContainsAny(slug, "/ :?#") {
		return "", fmt.Errorf("invalid agent slug %q", slug)
	}
	return slug, nil
}

func normalizeSlugs(raw []string) ([]string, error) {
	out := make([]string, 0, len(raw))
	seen := map[string]struct{}{}
	for _, item := range raw {
		slug, err := normalizeSlug(item)
		if err != nil {
			return nil, err
		}
		if _, dup := seen[slug]; dup {
			return nil, fmt.Errorf("duplicate agent slug %q", slug)
		}
		seen[slug] = struct{}{}
		out = append(out, slug)
	}
	return out, nil
}

// InitData initialises (or extends) a data directory without touching an
// existing key or token unless explicitly asked to add agents.
func InitData(opts InitOptions) (*InitResult, error) {
	dir := strings.TrimSpace(opts.DataDir)
	if dir == "" {
		return nil, errors.New("data_dir is required")
	}
	if len(opts.Agents) > 0 && len(opts.AddAgents) > 0 {
		return nil, errors.New("use either --agents or --add-agents, not both")
	}
	if len(opts.AddAgents) > 0 {
		return addAgents(dir, opts.AddAgents)
	}
	if len(opts.Agents) == 0 {
		return nil, errors.New("at least one agent slug is required (--agents slug1,slug2,...)")
	}
	agents, err := normalizeSlugs(opts.Agents)
	if err != nil {
		return nil, err
	}

	if err := ensureDirEmptyOrMissing(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	lock, err := acquireDataLock(dir)
	if err != nil {
		return nil, err
	}
	defer lock.Release()

	al := allowlistFile{}
	now := time.Now().UTC().Truncate(time.Second)
	for _, slug := range agents {
		al.Agents = append(al.Agents, &AgentConfig{
			Slug:        slug,
			Name:        slug,
			DisplayName: slug,
			CreatedAt:   now,
		})
	}
	if err := writeAllowlist(filepath.Join(dir, allowlistFileName), al); err != nil {
		return nil, err
	}

	tokens := map[string]string{}
	hashes := map[string]string{}
	for _, slug := range agents {
		tok := randToken()
		sum := sha256.Sum256([]byte(tok))
		tokens[slug] = tok
		hashes[slug] = hex.EncodeToString(sum[:])
	}
	if err := writeTokens(filepath.Join(dir, tokensFileName), al.Agents, hashes); err != nil {
		return nil, err
	}
	if err := writeTokensPlain(filepath.Join(dir, tokensPlainName), al.Agents, tokens); err != nil {
		return nil, err
	}
	if err := writePlatformKey(filepath.Join(dir, keysFileName)); err != nil {
		return nil, err
	}
	// agent_keys.json is generated on first server start (legacy behavior).

	return &InitResult{DataDir: dir, Added: agents, Tokens: tokens}, nil
}

func addAgents(dir string, raw []string) (*InitResult, error) {
	agents, err := normalizeSlugs(raw)
	if err != nil {
		return nil, err
	}
	allowPath := filepath.Join(dir, allowlistFileName)
	if _, err := os.Stat(allowPath); err != nil {
		return nil, fmt.Errorf("%s is missing; initialise the directory first", allowPath)
	}
	lock, err := acquireDataLock(dir)
	if err != nil {
		return nil, err
	}
	defer lock.Release()

	al, err := readAllowlist(allowPath)
	if err != nil {
		return nil, err
	}
	existing := map[string]struct{}{}
	for _, a := range al.Agents {
		existing[a.Slug] = struct{}{}
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, slug := range agents {
		if _, dup := existing[slug]; dup {
			return nil, fmt.Errorf("agent %q already exists in allowlist", slug)
		}
		al.Agents = append(al.Agents, &AgentConfig{
			Slug:        slug,
			Name:        slug,
			DisplayName: slug,
			CreatedAt:   now,
		})
	}
	if err := writeAllowlist(allowPath, al); err != nil {
		return nil, err
	}

	hashes, err := readTokenHashes(filepath.Join(dir, tokensFileName))
	if err != nil {
		return nil, err
	}
	plain, _ := readTokenPlain(filepath.Join(dir, tokensPlainName))
	newTokens := map[string]string{}
	for _, slug := range agents {
		tok := randToken()
		sum := sha256.Sum256([]byte(tok))
		hashes[slug] = hex.EncodeToString(sum[:])
		plain[slug] = tok
		newTokens[slug] = tok
	}
	if err := writeTokens(filepath.Join(dir, tokensFileName), al.Agents, hashes); err != nil {
		return nil, err
	}
	allPlain := true
	for _, a := range al.Agents {
		if strings.TrimSpace(plain[a.Slug]) == "" {
			allPlain = false
			break
		}
	}
	if allPlain {
		if err := writeTokensPlain(filepath.Join(dir, tokensPlainName), al.Agents, plain); err != nil {
			return nil, err
		}
	} else {
		// Never clobber a file that may be the only copy of another agent's
		// token. Append only the newly minted lines.
		if err := appendTokensPlain(filepath.Join(dir, tokensPlainName), al.Agents, newTokens); err != nil {
			return nil, err
		}
	}

	return &InitResult{DataDir: dir, Added: agents, Tokens: newTokens}, nil
}

func ensureDirEmptyOrMissing(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if entry.Name() == lockFileName {
			continue
		}
		return fmt.Errorf("data directory %s is not empty; refusing to overwrite", dir)
	}
	return nil
}

func readAllowlist(path string) (allowlistFile, error) {
	var al allowlistFile
	b, err := os.ReadFile(path)
	if err != nil {
		return al, err
	}
	if err := json.Unmarshal(b, &al); err != nil {
		return al, fmt.Errorf("%s: %w", path, err)
	}
	return al, nil
}

func writeAllowlist(path string, al allowlistFile) error {
	b, err := json.MarshalIndent(al, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func readTokenHashes(path string) (map[string]string, error) {
	hashes := map[string]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return hashes, nil
		}
		return nil, err
	}
	var tf tokensFile
	if err := json.Unmarshal(b, &tf); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, t := range tf.Tokens {
		hashes[t.Slug] = t.SHA256
	}
	return hashes, nil
}

func writeTokens(path string, agents []*AgentConfig, hashes map[string]string) error {
	// Preserve allowlist order, matching the legacy server's file layout.
	tf := tokensFile{}
	for _, a := range agents {
		if a == nil {
			continue
		}
		hash := hashes[a.Slug]
		if hash == "" {
			continue
		}
		tf.Tokens = append(tf.Tokens, tokenRecord{Slug: a.Slug, SHA256: hash})
	}
	b, err := json.MarshalIndent(tf, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func readTokenPlain(path string) (map[string]string, error) {
	out := map[string]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, nil
		}
		return nil, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) >= 3 {
			out[parts[0]] = parts[2]
		}
	}
	return out, nil
}

func writeTokensPlain(path string, agents []*AgentConfig, plain map[string]string) error {
	var sb strings.Builder
	sb.WriteString("# A2A tokens — confidential. chmod 600. One line per agent.\n")
	sb.WriteString("# slug\tname\ttoken\n")
	for _, a := range agents {
		tok := plain[a.Slug]
		if tok == "" {
			continue
		}
		sb.WriteString(a.Slug + "\t" + a.Name + "\t" + tok + "\n")
	}
	return os.WriteFile(path, []byte(sb.String()), 0o600)
}

func appendTokensPlain(path string, agents []*AgentConfig, plain map[string]string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	var sb strings.Builder
	for _, a := range agents {
		tok, ok := plain[a.Slug]
		if !ok || tok == "" {
			continue
		}
		sb.WriteString(a.Slug + "\t" + a.Name + "\t" + tok + "\n")
	}
	_, err = f.WriteString(sb.String())
	return err
}

func writePlatformKey(path string) error {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return err
	}
	kf := keysFile{
		Seed:      base64.RawURLEncoding.EncodeToString(seed),
		CreatedAt: time.Now().UTC().Truncate(time.Second),
	}
	b, err := json.MarshalIndent(kf, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// CheckOptions configures the read-only `check` command.
type CheckOptions struct {
	DataDir  string
	Listen   string
	Domain   string
	SignMode string
}

// CheckReport is the read-only self-check result.
type CheckReport struct {
	DataDir       string   `json:"data_dir"`
	OK            bool     `json:"ok"`
	Errors        []string `json:"errors"`
	Warnings      []string `json:"warnings"`
	Agents        int      `json:"agents"`
	Messages      int      `json:"messages"`
	Conversations int      `json:"conversations"`
	SignMode      string   `json:"sign_mode"`
	Listen        string   `json:"listen"`
	ListenInUse   bool     `json:"listen_in_use"`
}

// CheckData inspects the data directory without writing anything.
func CheckData(opts CheckOptions) *CheckReport {
	rep := &CheckReport{
		DataDir:  strings.TrimSpace(opts.DataDir),
		SignMode: effectiveSignMode(opts.SignMode),
		Listen:   opts.Listen,
		Errors:   []string{},
		Warnings: []string{},
	}
	addErr := func(format string, args ...any) {
		rep.Errors = append(rep.Errors, fmt.Sprintf(format, args...))
	}
	addWarn := func(format string, args ...any) {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf(format, args...))
	}

	if rep.DataDir == "" {
		addErr("data_dir is required")
		rep.OK = false
		return rep
	}
	st, err := os.Stat(rep.DataDir)
	if err != nil {
		addErr("data directory: %v", err)
		rep.OK = false
		return rep
	}
	if !st.IsDir() {
		addErr("%s is not a directory", rep.DataDir)
		rep.OK = false
		return rep
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		addWarn("data directory mode is %o, expected 0700", perm)
	}

	checkSecure := func(name string) string {
		path := filepath.Join(rep.DataDir, name)
		fi, err := os.Stat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return ""
			}
			addErr("%s: %v", name, err)
			return ""
		}
		if perm := fi.Mode().Perm(); perm&0o022 != 0 {
			addErr("%s is group/world writable (mode %o)", name, perm)
		}
		return path
	}

	allowPath := checkSecure(allowlistFileName)
	var allow allowlistFile
	if allowPath == "" {
		addErr("allowlist.json is missing; run 'picoclaw a2a-server init'")
	} else if b, err := os.ReadFile(allowPath); err != nil {
		addErr("allowlist.json: %v", err)
	} else if err := json.Unmarshal(b, &allow); err != nil {
		addErr("allowlist.json: %v", err)
	} else {
		seen := map[string]struct{}{}
		for _, a := range allow.Agents {
			if a == nil || strings.TrimSpace(a.Slug) == "" {
				addErr("allowlist.json contains an empty slug")
				continue
			}
			if _, dup := seen[a.Slug]; dup {
				addErr("allowlist.json contains duplicate slug %q", a.Slug)
			}
			seen[a.Slug] = struct{}{}
		}
		rep.Agents = len(allow.Agents)
		if rep.Agents == 0 {
			addErr("allowlist.json contains no agents")
		}
	}

	tokensPath := checkSecure(tokensFileName)
	hashes := map[string]string{}
	if tokensPath == "" {
		addErr("tokens.json is missing")
	} else if hashes, err = readTokenHashes(tokensPath); err != nil {
		addErr("tokens.json: %v", err)
	}
	if allowPath != "" && tokensPath != "" {
		for _, a := range allow.Agents {
			if a == nil {
				continue
			}
			if strings.TrimSpace(hashes[a.Slug]) == "" {
				addErr("agent %q has no token hash", a.Slug)
			}
		}
	}

	if plainPath := checkSecure(tokensPlainName); plainPath != "" {
		plain, err := readTokenPlain(plainPath)
		if err != nil {
			addErr("tokens.txt: %v", err)
		} else {
			for slug, tok := range plain {
				want, ok := hashes[slug]
				if !ok {
					addWarn("tokens.txt has %q which is not in tokens.json", slug)
					continue
				}
				sum := sha256.Sum256([]byte(tok))
				if hex.EncodeToString(sum[:]) != want {
					addWarn("tokens.txt entry for %q does not match tokens.json", slug)
				}
			}
			for slug := range hashes {
				if strings.TrimSpace(plain[slug]) == "" {
					addWarn("tokens.txt has no plaintext token for %q", slug)
				}
			}
		}
	}

	if keysPath := checkSecure(keysFileName); keysPath == "" {
		addErr("keys.json is missing")
	} else if b, err := os.ReadFile(keysPath); err != nil {
		addErr("keys.json: %v", err)
	} else {
		var kf keysFile
		if err := json.Unmarshal(b, &kf); err != nil {
			addErr("keys.json: %v", err)
		} else if seed, err := base64.RawURLEncoding.DecodeString(kf.Seed); err != nil || len(seed) != ed25519.SeedSize {
			addErr("keys.json: invalid ed25519 seed")
		}
	}

	if agentKeysPath := checkSecure(agentKeysName); agentKeysPath == "" {
		addWarn("agent_keys.json is missing (generated on first server start)")
	} else if b, err := os.ReadFile(agentKeysPath); err != nil {
		addErr("agent_keys.json: %v", err)
	} else {
		var f agentKeysFile
		if err := json.Unmarshal(b, &f); err != nil {
			addErr("agent_keys.json: %v", err)
		} else {
			for _, rec := range f.Agents {
				if rec == nil {
					continue
				}
				if rec.Seed != "" {
					if seed, err := base64.RawURLEncoding.DecodeString(rec.Seed); err != nil || len(seed) != ed25519.SeedSize {
						addErr("agent_keys.json: invalid seed for %q", rec.Slug)
					}
				}
			}
		}
	}

	if dataPath := checkSecure(dataFileName); dataPath == "" {
		addWarn("data.json is missing (no messages stored yet)")
	} else if b, err := os.ReadFile(dataPath); err != nil {
		addErr("data.json: %v", err)
	} else {
		var st diskState
		if err := json.Unmarshal(b, &st); err != nil {
			addErr("data.json: %v", err)
		} else {
			rep.Messages = len(st.Messages)
			convs := map[string]struct{}{}
			for _, m := range st.Messages {
				if m == nil {
					continue
				}
				convs[m.ConvID] = struct{}{}
				if want := ConversationID(m.From, m.To); want != m.ConvID {
					addWarn("message %s conversation_id %q does not match %q", m.ID, m.ConvID, want)
				}
			}
			rep.Conversations = len(convs)
		}
	}

	if presencePath := checkSecure(presenceFileName); presencePath != "" {
		if b, err := os.ReadFile(presencePath); err != nil {
			addErr("presence.json: %v", err)
		} else {
			var raw map[string]string
			if err := json.Unmarshal(b, &raw); err != nil {
				addErr("presence.json: %v", err)
			}
			for slug, ts := range raw {
				if _, err := time.Parse(time.RFC3339, ts); err != nil {
					addWarn("presence.json: invalid timestamp for %q", slug)
				}
			}
		}
	}

	if rep.Listen != "" {
		if err := validateLoopbackListen(rep.Listen); err != nil {
			addErr("%v", err)
		} else if ln, err := net.Listen("tcp", rep.Listen); err != nil {
			// The expected production state is "server already running".
			rep.ListenInUse = true
		} else {
			_ = ln.Close()
		}
	}

	rep.OK = len(rep.Errors) == 0
	return rep
}
