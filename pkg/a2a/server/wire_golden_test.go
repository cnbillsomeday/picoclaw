// PicoClaw - Ultra-lightweight personal AI agent

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// wireCase is one recorded request/response pair used to prove that the ported
// server is wire-compatible with the legacy binary.
type wireCase struct {
	Name            string            `json:"name"`
	Method          string            `json:"method"`
	Path            string            `json:"path"`
	Token           string            `json:"token,omitempty"` // alpha | beta | empty
	ContentType     string            `json:"content_type,omitempty"`
	Body            string            `json:"body,omitempty"`
	IfNoneMatchFrom string            `json:"if_none_match_from,omitempty"`
	Status          int               `json:"status"`
	Headers         map[string]string `json:"headers,omitempty"`
	BodyNorm        any               `json:"body_norm,omitempty"`
}

type wireGoldenFile struct {
	GeneratedBy string     `json:"generated_by"`
	Cases       []wireCase `json:"cases"`
}

func wireCases() []wireCase {
	tooLong := strings.Repeat("x", 8001)
	conv := "c_4163834372780d81"
	return []wireCase{
		{Name: "healthz", Method: "GET", Path: "/healthz"},
		{Name: "root", Method: "GET", Path: "/"},
		{Name: "agents", Method: "GET", Path: "/api/v1/agents"},
		{Name: "agent-profile", Method: "GET", Path: "/api/v1/agents/alpha"},
		{Name: "u-profile", Method: "GET", Path: "/u/alpha"},
		{Name: "domain-did", Method: "GET", Path: "/.well-known/did.json"},
		{Name: "agent-did", Method: "GET", Path: "/agents/alpha/did.json"},
		{Name: "agent-card", Method: "GET", Path: "/agents/alpha/agent-card.json"},
		{Name: "heartbeat-md", Method: "GET", Path: "/heartbeat.md"},
		{Name: "skill-md", Method: "GET", Path: "/skill.md"},
		{Name: "style-md", Method: "GET", Path: "/style-guide.md"},
		{Name: "sign-py", Method: "GET", Path: "/sign.py"},
		{Name: "sign-py-hash", Method: "GET", Path: "/sign.py.sha256"},
		{Name: "signing-md", Method: "GET", Path: "/signing.md"},
		{Name: "home-beta", Method: "GET", Path: "/api/v1/home", Token: "beta"},
		{Name: "home-beta-304", Method: "GET", Path: "/api/v1/home", Token: "beta", IfNoneMatchFrom: "home-beta"},
		{Name: "me-beta", Method: "GET", Path: "/api/v1/me", Token: "beta"},
		{Name: "presence", Method: "GET", Path: "/api/v1/presence", Token: "beta"},
		{Name: "conversations-beta", Method: "GET", Path: "/api/v1/a2a/conversations", Token: "beta"},
		{Name: "conversation-peek", Method: "GET", Path: "/api/v1/a2a/conversations/" + conv + "?peek=1", Token: "beta"},
		{Name: "home-beta-after-peek", Method: "GET", Path: "/api/v1/home", Token: "beta"},
		{Name: "conversation-clear", Method: "GET", Path: "/api/v1/a2a/conversations/" + conv, Token: "beta"},
		{Name: "home-beta-after-clear", Method: "GET", Path: "/api/v1/home", Token: "beta"},
		{Name: "home-alpha", Method: "GET", Path: "/api/v1/home", Token: "alpha"},
		{Name: "stats", Method: "GET", Path: "/api/v1/stats", Token: "alpha"},
		{Name: "auth-missing", Method: "GET", Path: "/api/v1/home"},
		{Name: "send-unsigned", Method: "POST", Path: "/a2a/beta", Token: "alpha", ContentType: "application/json", Body: `{"content":"unsigned"}`},
		{Name: "send-unknown", Method: "POST", Path: "/a2a/nobody", Token: "alpha", ContentType: "application/json", Body: `{"content":"x"}`},
		{Name: "send-wrong-content-type", Method: "POST", Path: "/a2a/beta", Token: "alpha", ContentType: "text/plain", Body: `{"content":"x"}`},
		{Name: "send-empty", Method: "POST", Path: "/a2a/beta", Token: "alpha", ContentType: "application/json", Body: `{}`},
		{Name: "send-too-long", Method: "POST", Path: "/a2a/beta", Token: "alpha", ContentType: "application/json", Body: fmt.Sprintf(`{"content":%q}`, tooLong)},
		{Name: "not-found", Method: "GET", Path: "/nope"},
	}
}

var goldenTokens = map[string]string{
	"alpha": "alpha-token-fixture",
	"beta":  "beta-token-fixture",
}

var goldenHeaders = []string{
	"Content-Type",
	"Cache-Control",
	"ETag",
	"WWW-Authenticate",
	"X-Content-Type-Options",
}

func copyFixture(t *testing.T, dst string) {
	t.Helper()
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join("testdata", "fixture"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	for _, entry := range entries {
		src := filepath.Join("testdata", "fixture", entry.Name())
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("read %s: %v", src, err)
		}
		if err := os.WriteFile(filepath.Join(dst, entry.Name()), data, 0o600); err != nil {
			t.Fatalf("write %s: %v", entry.Name(), err)
		}
	}
}

func runWireCases(t *testing.T, baseURL string) []wireCase {
	t.Helper()
	out := make([]wireCase, 0, len(wireCases()))
	etags := map[string]string{}
	client := &http.Client{Timeout: 10 * time.Second}
	for _, tc := range wireCases() {
		req, err := http.NewRequest(tc.Method, baseURL+tc.Path, strings.NewReader(tc.Body))
		if err != nil {
			t.Fatalf("%s: new request: %v", tc.Name, err)
		}
		if tc.Token != "" {
			req.Header.Set("Authorization", "Bearer "+goldenTokens[tc.Token])
		}
		if tc.ContentType != "" {
			req.Header.Set("Content-Type", tc.ContentType)
		}
		if tc.IfNoneMatchFrom != "" {
			etag := etags[tc.IfNoneMatchFrom]
			if etag == "" {
				t.Fatalf("%s: no ETag recorded for %s", tc.Name, tc.IfNoneMatchFrom)
			}
			req.Header.Set("If-None-Match", etag)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s: request failed: %v", tc.Name, err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("%s: read body: %v", tc.Name, err)
		}
		if etag := resp.Header.Get("ETag"); etag != "" {
			etags[tc.Name] = etag
		}
		headers := map[string]string{}
		for _, h := range goldenHeaders {
			if v := resp.Header.Get(h); v != "" {
				headers[h] = v
			}
		}
		tc.Status = resp.StatusCode
		tc.Headers = headers
		tc.BodyNorm = normalizeWireBody(t, resp.Header.Get("Content-Type"), body)
		out = append(out, tc)
	}
	return out
}

// normalizeWireBody removes fields that legitimately vary between runs while
// keeping everything that defines the wire contract.
func normalizeWireBody(t *testing.T, contentType string, body []byte) any {
	t.Helper()
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return ""
	}
	if !strings.Contains(strings.ToLower(contentType), "application/json") {
		return string(body)
	}
	var v any
	if err := json.Unmarshal(trimmed, &v); err != nil {
		return string(body)
	}
	return normalizeWireValue(v, "")
}

func normalizeWireValue(v any, key string) any {
	switch key {
	case "time", "server_time", "now":
		return ""
	case "uptime_seconds", "age_seconds":
		return 0
	case "last_seen":
		if v != nil {
			return "<ts>"
		}
		return nil
	}
	switch value := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		for k, item := range value {
			out[k] = normalizeWireValue(item, k)
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = normalizeWireValue(item, key)
		}
		return out
	default:
		return value
	}
}

func sortedJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func diffCase(t *testing.T, got, want wireCase) {
	t.Helper()
	if got.Status != want.Status {
		t.Fatalf("%s: status = %d, want %d", want.Name, got.Status, want.Status)
	}
	gotHeaders := sortedJSON(t, got.Headers)
	wantHeaders := sortedJSON(t, want.Headers)
	if gotHeaders != wantHeaders {
		t.Fatalf("%s: headers = %s, want %s", want.Name, gotHeaders, wantHeaders)
	}
	gotBody := sortedJSON(t, got.BodyNorm)
	wantBody := sortedJSON(t, want.BodyNorm)
	if gotBody != wantBody {
		t.Fatalf("%s: body mismatch:\n got: %s\nwant: %s", want.Name, truncate(gotBody, 2000), truncate(wantBody, 2000))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// TestWireGolden replays the legacy corpus against the ported server.
func TestWireGolden(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "wire_golden.json"))
	if err != nil {
		t.Fatalf("wire golden corpus is missing; regenerate with A2A_RECORD_LEGACY=1: %v", err)
	}
	var golden wireGoldenFile
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("decode golden: %v", err)
	}

	dir := t.TempDir()
	copyFixture(t, dir)
	inst, err := New(Options{
		Listen:    "127.0.0.1:0",
		DataDir:   dir,
		Domain:    "test.example",
		Retention: 876000 * time.Hour,
		SignMode:  "strict",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := inst.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = inst.Stop(ctx)
	}()

	got := runWireCases(t, "http://"+inst.Addr())
	if len(got) != len(golden.Cases) {
		t.Fatalf("case count = %d, want %d", len(got), len(golden.Cases))
	}
	for i := range golden.Cases {
		diffCase(t, got[i], golden.Cases[i])
	}
}

// TestRecordWireGolden regenerates the corpus from the legacy binary. It is
// gated behind A2A_RECORD_LEGACY=1 and A2A_LEGACY_BIN so normal CI never needs
// the old binary.
func TestRecordWireGolden(t *testing.T) {
	if os.Getenv("A2A_RECORD_LEGACY") != "1" {
		t.Skip("set A2A_RECORD_LEGACY=1 and A2A_LEGACY_BIN to regenerate")
	}
	legacy := strings.TrimSpace(os.Getenv("A2A_LEGACY_BIN"))
	if legacy == "" {
		t.Fatal("A2A_LEGACY_BIN is required")
	}
	dir := t.TempDir()
	copyFixture(t, dir)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cmd := exec.Command(legacy,
		"-addr", addr,
		"-dir", dir,
		"-domain", "test.example",
		"-retention", "876000h",
	)
	cmd.Env = append(os.Environ(), "A2A_SIGN_MODE=strict")
	var stderr bytes.Buffer
	cmd.Stdout = &stderr
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start legacy server: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	base := "http://" + addr
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("legacy server did not start: %v\n%s", err, stderr.String())
		}
		time.Sleep(50 * time.Millisecond)
	}

	cases := runWireCases(t, base)
	golden := wireGoldenFile{GeneratedBy: "agent-a2a legacy a2a-server", Cases: cases}
	raw, err := json.MarshalIndent(golden, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", "wire_golden.json")
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	sort.Slice(cases, func(i, j int) bool { return cases[i].Name < cases[j].Name })
	t.Logf("recorded %d cases to %s", len(cases), path)
}
