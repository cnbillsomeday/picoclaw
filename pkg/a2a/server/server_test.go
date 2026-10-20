// PicoClaw - Ultra-lightweight personal AI agent

package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/a2a/wire"
)

type testSrv struct {
	inst   *Instance
	base   string
	tokens map[string]string
}

func newTestSrv(t *testing.T, signMode string, agents ...string) *testSrv {
	t.Helper()
	dir := t.TempDir()
	res, err := InitData(InitOptions{DataDir: dir, Agents: agents})
	if err != nil {
		t.Fatalf("InitData: %v", err)
	}
	inst, err := New(Options{
		Listen:    "127.0.0.1:0",
		DataDir:   dir,
		Domain:    "test.example",
		Retention: 24 * time.Hour,
		SignMode:  signMode,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := inst.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = inst.Stop(ctx)
	})
	return &testSrv{inst: inst, base: "http://" + inst.Addr(), tokens: res.Tokens}
}

func (s *testSrv) request(t *testing.T, method, path, token string, body any) (int, []byte, http.Header) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		switch v := body.(type) {
		case []byte:
			reader = bytes.NewReader(v)
		case string:
			reader = strings.NewReader(v)
		default:
			b, err := json.Marshal(body)
			if err != nil {
				t.Fatalf("marshal body: %v", err)
			}
			reader = bytes.NewReader(b)
		}
	}
	req, err := http.NewRequest(method, s.base+path, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, data, resp.Header
}

func (s *testSrv) redeem(t *testing.T, slug string) []byte {
	t.Helper()
	code, data, _ := s.request(t, http.MethodPost, "/a2a/keys/redeem", s.tokens[slug], "{}")
	if code != http.StatusOK {
		t.Fatalf("redeem %s: status=%d body=%s", slug, code, data)
	}
	var out struct {
		Seed string `json:"seed_base64url"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode redeem: %v", err)
	}
	seed, err := base64.RawURLEncoding.DecodeString(out.Seed)
	if err != nil {
		t.Fatalf("decode seed: %v", err)
	}
	return seed
}

func (s *testSrv) send(t *testing.T, from, to, content string, signer *wire.Signer) (int, []byte) {
	t.Helper()
	var body []byte
	if signer != nil {
		ts, nonce, sig, err := signer.Sign(from, to, content)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		body, _ = json.Marshal(wire.SendRequest{Content: content, Timestamp: ts, Nonce: nonce, Sig: sig})
	} else {
		body, _ = json.Marshal(map[string]string{"content": content})
	}
	code, data, _ := s.request(t, http.MethodPost, "/a2a/"+to, s.tokens[from], body)
	return code, data
}

func TestConversationIDMatchesLegacy(t *testing.T) {
	cases := []struct {
		a, b, want string
	}{
		{"fei", "vu", "c_c120d1e91ce7840f"},
		{"vu", "fei", "c_c120d1e91ce7840f"},
		{"alice", "bob", "c_141a4cd242b67e82"},
	}
	for _, tc := range cases {
		if got := ConversationID(tc.a, tc.b); got != tc.want {
			t.Fatalf("ConversationID(%s,%s) = %s, want %s", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestEmptyAllowlistRefusedWithoutOptIn(t *testing.T) {
	dir := t.TempDir()
	if _, err := New(Options{Listen: "127.0.0.1:0", DataDir: dir, Domain: "test.example"}); err == nil {
		t.Fatal("New accepted a missing allowlist with allow_default_agents=false")
	}
	inst, err := New(Options{
		Listen:             "127.0.0.1:0",
		DataDir:            dir,
		Domain:             "test.example",
		AllowDefaultAgents: true,
	})
	if err != nil {
		t.Fatalf("New with allow_default_agents: %v", err)
	}
	defer func() { _ = inst.Stop(context.Background()) }()
	if len(inst.Config().Agents) != 4 {
		t.Fatalf("default agents = %d, want 4", len(inst.Config().Agents))
	}
}

func TestGroupWorldWritableFileRefused(t *testing.T) {
	dir := t.TempDir()
	if _, err := InitData(InitOptions{DataDir: dir, Agents: []string{"fei", "vu"}}); err != nil {
		t.Fatalf("InitData: %v", err)
	}
	if err := os.Chmod(filepath.Join(dir, "allowlist.json"), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{Listen: "127.0.0.1:0", DataDir: dir, Domain: "test.example"}); err == nil {
		t.Fatal("New accepted a group/world-writable allowlist.json")
	}
}

func TestETagAnd304(t *testing.T) {
	s := newTestSrv(t, "strict", "fei", "vu")
	token := s.tokens["vu"]
	code, data, headers := s.request(t, http.MethodGet, "/api/v1/home", token, nil)
	if code != http.StatusOK {
		t.Fatalf("home status = %d body=%s", code, data)
	}
	etag := headers.Get("ETag")
	if !strings.HasPrefix(etag, `W/"`) {
		t.Fatalf("ETag = %q, want weak ETag", etag)
	}
	code, body, _ := s.requestWithHeader(t, http.MethodGet, "/api/v1/home", token, "", "If-None-Match", etag)
	if code != http.StatusNotModified || len(body) != 0 {
		t.Fatalf("304 check: status=%d body=%q", code, body)
	}
	seed := s.redeem(t, "fei")
	signer, _ := wire.NewSigner(seed)
	if code, data := s.send(t, "fei", "vu", "ping", signer); code != http.StatusOK {
		t.Fatalf("send status=%d body=%s", code, data)
	}
	code, _, headers = s.request(t, http.MethodGet, "/api/v1/home", token, nil)
	if code != http.StatusOK {
		t.Fatalf("home after send status = %d", code)
	}
	if headers.Get("ETag") == etag {
		t.Fatal("ETag did not change after a new message")
	}
}

func (s *testSrv) requestWithHeader(t *testing.T, method, path, token, body, key, value string) (int, []byte, http.Header) {
	t.Helper()
	req, err := http.NewRequest(method, s.base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		req.Header.Set(key, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data, resp.Header
}

func TestReadSemantics(t *testing.T) {
	s := newTestSrv(t, "strict", "fei", "vu")
	seed := s.redeem(t, "fei")
	signer, _ := wire.NewSigner(seed)
	if code, data := s.send(t, "fei", "vu", "hello", signer); code != http.StatusOK {
		t.Fatalf("send: %d %s", code, data)
	}

	var home HomeView
	_, data, _ := s.request(t, http.MethodGet, "/api/v1/home", s.tokens["vu"], nil)
	if err := json.Unmarshal(data, &home); err != nil {
		t.Fatalf("home decode: %v", err)
	}
	if home.UnreadTotal != 1 {
		t.Fatalf("unread = %d, want 1", home.UnreadTotal)
	}
	convID := home.Conversations[0].ID

	code, data, _ := s.request(t, http.MethodGet, "/api/v1/a2a/conversations/"+convID+"?peek=1", s.tokens["vu"], nil)
	if code != http.StatusOK {
		t.Fatalf("peek status=%d body=%s", code, data)
	}
	var peek struct {
		UnreadCleared int `json:"unread_cleared"`
	}
	_ = json.Unmarshal(data, &peek)
	if peek.UnreadCleared != 0 {
		t.Fatalf("peek cleared %d unread, want 0", peek.UnreadCleared)
	}
	_, data, _ = s.request(t, http.MethodGet, "/api/v1/home", s.tokens["vu"], nil)
	_ = json.Unmarshal(data, &home)
	if home.UnreadTotal != 1 {
		t.Fatalf("unread after peek = %d, want 1", home.UnreadTotal)
	}

	_, data, _ = s.request(t, http.MethodGet, "/api/v1/a2a/conversations/"+convID, s.tokens["vu"], nil)
	var detail struct {
		UnreadCleared int `json:"unread_cleared"`
	}
	_ = json.Unmarshal(data, &detail)
	if detail.UnreadCleared != 1 {
		t.Fatalf("detail cleared %d unread, want 1", detail.UnreadCleared)
	}
	_, data, _ = s.request(t, http.MethodGet, "/api/v1/home", s.tokens["vu"], nil)
	_ = json.Unmarshal(data, &home)
	if home.UnreadTotal != 0 {
		t.Fatalf("unread after detail = %d, want 0", home.UnreadTotal)
	}
}

func TestSendSignatureModes(t *testing.T) {
	s := newTestSrv(t, "strict", "fei", "vu")
	seed := s.redeem(t, "fei")
	signer, _ := wire.NewSigner(seed)
	otherSeed := make([]byte, ed25519.SeedSize)
	_, _ = rand.Read(otherSeed)
	wrongSigner, _ := wire.NewSigner(otherSeed)

	if code, _ := s.send(t, "fei", "vu", "unsigned", nil); code != http.StatusForbidden {
		t.Fatalf("unsigned strict status = %d, want 403", code)
	}
	if code, _ := s.send(t, "fei", "vu", "bad sig", wrongSigner); code != http.StatusForbidden {
		t.Fatalf("bad signature status = %d, want 403", code)
	}

	// Stale timestamp.
	ts := time.Now().Add(-10 * time.Minute).Unix()
	nonce := "00112233445566778899aabbccddeeff"
	sig := signer.SignAt("fei", "vu", ts, nonce, "stale")
	body, _ := json.Marshal(wire.SendRequest{Content: "stale", Timestamp: ts, Nonce: nonce, Sig: sig})
	if code, _, _ := s.request(t, http.MethodPost, "/a2a/vu", s.tokens["fei"], body); code != http.StatusForbidden {
		t.Fatalf("stale signature status = %d, want 403", code)
	}

	// Valid signature.
	ts = time.Now().Unix()
	sig = signer.SignAt("fei", "vu", ts, nonce, "ok")
	body, _ = json.Marshal(wire.SendRequest{Content: "ok", Timestamp: ts, Nonce: nonce, Sig: sig})
	code, data, _ := s.request(t, http.MethodPost, "/a2a/vu", s.tokens["fei"], body)
	if code != http.StatusOK {
		t.Fatalf("valid signature status=%d body=%s", code, data)
	}
	var resp wire.SendResponse
	_ = json.Unmarshal(data, &resp)
	if !resp.Signed || resp.SigState != "ok" {
		t.Fatalf("signature verdict = %v/%q", resp.Signed, resp.SigState)
	}

	// Replaying the same signed body must be rejected.
	if code, _, _ := s.request(t, http.MethodPost, "/a2a/vu", s.tokens["fei"], body); code != http.StatusForbidden {
		t.Fatalf("replayed nonce status = %d, want 403", code)
	}
}

func TestWarnModeAcceptsUnsigned(t *testing.T) {
	s := newTestSrv(t, "warn", "fei", "vu")
	code, data := s.send(t, "fei", "vu", "unsigned", nil)
	if code != http.StatusOK {
		t.Fatalf("warn mode unsigned status=%d body=%s", code, data)
	}
	var resp wire.SendResponse
	_ = json.Unmarshal(data, &resp)
	if resp.Signed || resp.SigState != "unsigned" {
		t.Fatalf("unsigned verdict = %v/%q", resp.Signed, resp.SigState)
	}
}

func TestSendValidationErrors(t *testing.T) {
	s := newTestSrv(t, "strict", "fei", "vu")

	if code, _, _ := s.request(t, http.MethodGet, "/api/v1/home", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("missing auth status = %d, want 401", code)
	}
	if code, _ := s.send(t, "fei", "nobody", "x", nil); code != http.StatusForbidden {
		t.Fatalf("unknown recipient status = %d, want 403", code)
	}
	// Wrong content type is checked before auth-independent decoding.
	req, _ := http.NewRequest(http.MethodPost, s.base+"/a2a/vu", strings.NewReader(`{"content":"x"}`))
	req.Header.Set("Authorization", "Bearer "+s.tokens["fei"])
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("wrong content type status = %d, want 415", resp.StatusCode)
	}
	if code, _, _ := s.request(t, http.MethodPost, "/a2a/vu", s.tokens["fei"], "{}"); code != http.StatusUnprocessableEntity {
		t.Fatalf("empty body status = %d, want 422", code)
	}
	if code, _, _ := s.request(t, http.MethodPost, "/a2a/vu", s.tokens["fei"], map[string]string{"content": strings.Repeat("x", wire.MaxMessageRunes+1)}); code != http.StatusUnprocessableEntity {
		t.Fatalf("too long status = %d, want 422", code)
	}
}

func TestKeyLifecycle(t *testing.T) {
	s := newTestSrv(t, "strict", "fei", "vu")
	_ = s.redeem(t, "fei")

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	code, data, _ := s.request(t, http.MethodPost, "/a2a/keys/register", s.tokens["fei"], map[string]any{
		"public_key":           pubB64,
		"retire_platform_seed": true,
	})
	if code != http.StatusOK {
		t.Fatalf("register status=%d body=%s", code, data)
	}
	var reg struct {
		Success             bool `json:"success"`
		PlatformSeedRetired bool `json:"platform_seed_retired"`
		HasPlatformSeed     bool `json:"has_platform_seed"`
	}
	_ = json.Unmarshal(data, &reg)
	if !reg.Success || !reg.PlatformSeedRetired || reg.HasPlatformSeed {
		t.Fatalf("register result = %s", data)
	}
	// Redeem after retiring the platform seed must be refused.
	code, _, _ = s.request(t, http.MethodPost, "/a2a/keys/redeem", s.tokens["fei"], "{}")
	if code != http.StatusConflict {
		t.Fatalf("redeem after retire status = %d, want 409", code)
	}
}

func TestRetentionGC(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	state := diskState{
		Version: 1,
		Messages: []*Message{
			{ID: "m_old", ConvID: ConversationID("fei", "vu"), From: "fei", To: "vu", Body: "old", CreatedAt: now.Add(-25 * time.Hour)},
			{ID: "m_new", ConvID: ConversationID("fei", "vu"), From: "fei", To: "vu", Body: "new", CreatedAt: now.Add(-time.Hour)},
		},
	}
	raw, _ := json.Marshal(state)
	path := filepath.Join(dir, "data.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(path, 24*time.Hour)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	messages, _, _ := store.Stats()
	if messages != 1 {
		t.Fatalf("messages after GC = %d, want 1", messages)
	}
}

func TestStaticEndpoints(t *testing.T) {
	s := newTestSrv(t, "strict", "fei")
	code, py, _ := s.request(t, http.MethodGet, "/sign.py", "", nil)
	if code != http.StatusOK || len(py) == 0 {
		t.Fatalf("sign.py status=%d len=%d", code, len(py))
	}
	code, hash, _ := s.request(t, http.MethodGet, "/sign.py.sha256", "", nil)
	if code != http.StatusOK {
		t.Fatalf("sign.py.sha256 status=%d", code)
	}
	sum := sha256.Sum256(py)
	fields := strings.Fields(string(hash))
	if len(fields) == 0 || fields[0] != hex.EncodeToString(sum[:]) {
		t.Fatalf("sign.py hash mismatch: endpoint=%q want=%s", strings.TrimSpace(string(hash)), hex.EncodeToString(sum[:]))
	}
	code, did, _ := s.request(t, http.MethodGet, "/agents/fei/did.json", "", nil)
	if code != http.StatusOK || !bytes.Contains(did, []byte("did:web:test.example:agents:fei")) {
		t.Fatalf("agent did status=%d body=%s", code, did)
	}
}

func TestInitAddAgents(t *testing.T) {
	dir := t.TempDir()
	if _, err := InitData(InitOptions{DataDir: dir, Agents: []string{"fei", "vu"}}); err != nil {
		t.Fatalf("InitData: %v", err)
	}
	res, err := InitData(InitOptions{DataDir: dir, AddAgents: []string{"hao"}})
	if err != nil {
		t.Fatalf("InitData add: %v", err)
	}
	if len(res.Tokens) != 1 || res.Tokens["hao"] == "" {
		t.Fatalf("add tokens = %v, want hao token", res.Tokens)
	}
	al, err := readAllowlist(filepath.Join(dir, "allowlist.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(al.Agents) != 3 {
		t.Fatalf("allowlist agents = %d, want 3", len(al.Agents))
	}
	plain, _ := readTokenPlain(filepath.Join(dir, "tokens.txt"))
	if plain["hao"] != res.Tokens["hao"] {
		t.Fatalf("tokens.txt missing the new token")
	}
}

func TestCheckDataReportsProblems(t *testing.T) {
	dir := t.TempDir()
	if _, err := InitData(InitOptions{DataDir: dir, Agents: []string{"fei", "vu"}}); err != nil {
		t.Fatalf("InitData: %v", err)
	}
	rep := CheckData(CheckOptions{DataDir: dir, Listen: "127.0.0.1:0"})
	if !rep.OK {
		t.Fatalf("fresh data dir failed check: %v", rep.Errors)
	}
	if err := os.Chmod(filepath.Join(dir, "tokens.json"), 0o666); err != nil {
		t.Fatal(err)
	}
	rep = CheckData(CheckOptions{DataDir: dir, Listen: "127.0.0.1:0"})
	if rep.OK {
		t.Fatal("check accepted a group/world-writable tokens.json")
	}
}

func TestNonLoopbackListenRefused(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:21220", ":21220", "192.168.1.5:21220"} {
		if err := validateLoopbackListen(addr); err == nil {
			t.Fatalf("validateLoopbackListen(%q) accepted a non-loopback address", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:21220", "localhost:21220", "[::1]:21220"} {
		if err := validateLoopbackListen(addr); err != nil {
			t.Fatalf("validateLoopbackListen(%q) = %v", addr, err)
		}
	}
}

func TestCheckNeverWrites(t *testing.T) {
	dir := t.TempDir()
	if _, err := InitData(InitOptions{DataDir: dir, Agents: []string{"fei", "vu"}}); err != nil {
		t.Fatalf("InitData: %v", err)
	}
	before := map[string]time.Time{}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		fi, _ := e.Info()
		before[e.Name()] = fi.ModTime()
	}
	_ = CheckData(CheckOptions{DataDir: dir, Listen: "127.0.0.1:0"})
	entries, _ = os.ReadDir(dir)
	for _, e := range entries {
		if _, ok := before[e.Name()]; !ok {
			t.Fatalf("check created a new file: %s", e.Name())
		}
	}
}
