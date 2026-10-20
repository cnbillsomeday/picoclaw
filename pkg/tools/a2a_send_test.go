// PicoClaw - Ultra-lightweight personal AI agent

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sipeed/picoclaw/pkg/a2a/proactive"
	"github.com/sipeed/picoclaw/pkg/a2a/wire"
	"github.com/sipeed/picoclaw/pkg/config"
)

func newA2ATestConfig(t *testing.T, baseURL, keyFile string, extra string) *config.Config {
	t.Helper()
	settings := fmt.Sprintf(
		`{"base_url":%q,"agent_slug":"alpha","token":"test-token","key_file":%q,"sign":true,"http_timeout_sec":5%s}`,
		baseURL, keyFile, extra,
	)
	return &config.Config{
		Version: config.CurrentVersion,
		Channels: config.ChannelsConfig{
			"a2a": &config.Channel{
				Enabled:   true,
				Type:      config.ChannelA2A,
				AllowFrom: config.FlexibleStringSlice{"alpha", "beta"},
				Settings:  config.RawNode(settings),
			},
		},
	}
}

func TestA2ASendToolSendsSignedMessage(t *testing.T) {
	var mu sync.Mutex
	var reqs []wire.SendRequest
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/a2a/beta" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		auth = append(auth, r.Header.Get("Authorization"))
		mu.Unlock()
		var body wire.SendRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		mu.Lock()
		reqs = append(reqs, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(wire.SendResponse{
			Success: true, MessageID: "m_1", ConversationID: "c_1",
			Delivered: true, Signed: true, SigState: "ok",
		})
	}))
	defer srv.Close()

	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i)
	}
	keyFile := filepath.Join(t.TempDir(), "ed25519.seed")
	if err := os.WriteFile(keyFile, seed, 0o600); err != nil {
		t.Fatal(err)
	}
	signer, err := wire.NewSigner(seed)
	if err != nil {
		t.Fatal(err)
	}
	cfg := newA2ATestConfig(t, srv.URL, keyFile, "")

	ctx := WithToolSessionContext(context.Background(), "alpha", "session-1", nil)
	result := NewA2ASendTool(cfg).Execute(ctx, map[string]any{"to": "beta", "text": "hello beta"})
	if result.IsError {
		t.Fatalf("a2a_send failed: %s", result.ForLLM)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reqs) != 1 || reqs[0].Content != "hello beta" {
		t.Fatalf("requests = %v, want one message containing hello beta", reqs)
	}
	if !wire.Verify(signer.PublicKey(), "alpha", "beta", reqs[0].Timestamp, reqs[0].Nonce, reqs[0].Content, reqs[0].Sig) {
		t.Fatal("outbound signature did not verify")
	}
	if len(auth) != 1 || auth[0] != "Bearer test-token" {
		t.Fatalf("auth = %v", auth)
	}
	if !wire.ConsumeExplicitSend("session-1", "beta") {
		t.Fatal("explicit send marker was not recorded")
	}
}

func TestA2ASendToolRejectsUnknownRecipient(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	keyFile := filepath.Join(t.TempDir(), "ed25519.seed")
	_ = os.WriteFile(keyFile, make([]byte, 32), 0o600)
	cfg := newA2ATestConfig(t, srv.URL, keyFile, "")

	result := NewA2ASendTool(cfg).Execute(context.Background(), map[string]any{"to": "gamma", "text": "hi"})
	if !result.IsError {
		t.Fatal("a2a_send accepted a recipient outside allow_from")
	}
}

func TestA2ASendToolDryRunDoesNotSend(t *testing.T) {
	called := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		http.Error(w, "should not send", http.StatusInternalServerError)
	}))
	defer srv.Close()
	keyFile := filepath.Join(t.TempDir(), "ed25519.seed")
	_ = os.WriteFile(keyFile, make([]byte, 32), 0o600)
	cfg := newA2ATestConfig(t, srv.URL, keyFile, `,"dry_run":true`)

	result := NewA2ASendTool(cfg).Execute(context.Background(), map[string]any{"to": "beta", "text": "hi"})
	if result.IsError {
		t.Fatalf("dry-run a2a_send returned error: %s", result.ForLLM)
	}
	if called != 0 {
		t.Fatalf("dry-run sent %d request(s), want 0", called)
	}
}

func TestA2ASendToolSplitsLongMessages(t *testing.T) {
	var mu sync.Mutex
	contents := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The tool performs a best-effort /healthz clock probe; only count
		// real send requests here.
		if r.Method != http.MethodPost || r.URL.Path != "/a2a/beta" {
			http.NotFound(w, r)
			return
		}
		var body wire.SendRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		contents = append(contents, body.Content)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(wire.SendResponse{Success: true, MessageID: "m", Signed: true, SigState: "ok"})
	}))
	defer srv.Close()
	keyFile := filepath.Join(t.TempDir(), "ed25519.seed")
	_ = os.WriteFile(keyFile, make([]byte, 32), 0o600)
	cfg := newA2ATestConfig(t, srv.URL, keyFile, "")

	text := strings.Repeat("你", wire.MaxMessageRunes+10)
	result := NewA2ASendTool(cfg).Execute(context.Background(), map[string]any{"to": "beta", "text": text})
	if result.IsError {
		t.Fatalf("a2a_send failed: %s", result.ForLLM)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(contents) != 2 {
		t.Fatalf("chunks = %d, want 2", len(contents))
	}
	if joined := strings.Join(contents, ""); joined != text {
		t.Fatalf("joined chunks differ from original text")
	}
}

type proactiveSenderFunc func(context.Context, string, string, string) ([]string, error)

func (f proactiveSenderFunc) SendProactive(ctx context.Context, peer, text, sessionKey string) ([]string, error) {
	return f(ctx, peer, text, sessionKey)
}

// TestA2ASendToolDelegatesToLiveChannel: when the A2A channel is running the
// tool must go through it, so the rate limit / dry-run / signing / clock gates
// are the ones automatic replies obey. The configured base URL is unreachable,
// so a direct send would fail the test.
func TestA2ASendToolDelegatesToLiveChannel(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "ed25519.seed")
	if err := os.WriteFile(keyFile, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := newA2ATestConfig(t, "http://127.0.0.1:1", keyFile, "")

	var gotPeer, gotText, gotSession string
	sender := proactiveSenderFunc(func(_ context.Context, peer, text, sessionKey string) ([]string, error) {
		gotPeer, gotText, gotSession = peer, text, sessionKey
		return []string{"m_channel"}, nil
	})
	proactive.Register("a2a", sender)
	defer proactive.Unregister("a2a", sender)

	ctx := WithToolSessionContext(context.Background(), "alpha", "session-42", nil)
	result := NewA2ASendTool(cfg).Execute(ctx, map[string]any{"to": "beta", "text": "hello channel"})
	if result.IsError {
		t.Fatalf("a2a_send failed: %s", result.ForLLM)
	}
	if gotPeer != "beta" || gotText != "hello channel" {
		t.Fatalf("delegated call = (%q, %q)", gotPeer, gotText)
	}
	if gotSession != "session-42" {
		t.Fatalf("session key = %q, want session-42", gotSession)
	}
}

// TestA2ASendToolSurfacesChannelRateLimit: the channel's suppression must not
// look like a successful silent drop to the agent.
func TestA2ASendToolSurfacesChannelRateLimit(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "ed25519.seed")
	if err := os.WriteFile(keyFile, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := newA2ATestConfig(t, "http://127.0.0.1:1", keyFile, "")

	sender := proactiveSenderFunc(func(context.Context, string, string, string) ([]string, error) {
		return nil, errors.New("a2a: peer beta hit the 3-per-10m0s outbound limit")
	})
	proactive.Register("a2a", sender)
	defer proactive.Unregister("a2a", sender)

	result := NewA2ASendTool(cfg).Execute(context.Background(), map[string]any{"to": "beta", "text": "hello"})
	if !result.IsError {
		t.Fatal("a2a_send reported success although the channel refused the message")
	}
}
