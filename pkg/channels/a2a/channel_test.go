// PicoClaw - Ultra-lightweight personal AI agent

package a2a

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/a2a/client"
	"github.com/sipeed/picoclaw/pkg/a2a/server"
	"github.com/sipeed/picoclaw/pkg/a2a/wire"
	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
)

type testPlatform struct {
	inst   *server.Instance
	base   string
	tokens map[string]string
	signer map[string]*wire.Signer
}

func startTestPlatform(t *testing.T, agents ...string) *testPlatform {
	t.Helper()
	return startTestPlatformWithMode(t, "strict", agents...)
}

func startTestPlatformWithMode(t *testing.T, signMode string, agents ...string) *testPlatform {
	t.Helper()
	dir := t.TempDir()
	initRes, err := server.InitData(server.InitOptions{DataDir: dir, Agents: agents})
	if err != nil {
		t.Fatalf("InitData: %v", err)
	}
	inst, err := server.New(server.Options{
		Listen:             "127.0.0.1:0",
		DataDir:            dir,
		Domain:             "test.example",
		Retention:          24 * time.Hour,
		SignMode:           signMode,
		AllowDefaultAgents: false,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	if err := inst.Start(); err != nil {
		t.Fatalf("server.Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = inst.Stop(ctx)
	})
	base := "http://" + inst.Addr()

	signers := map[string]*wire.Signer{}
	for slug, token := range initRes.Tokens {
		seed, err := redeemSeed(base, token)
		if err != nil {
			t.Fatalf("redeem %s: %v", slug, err)
		}
		signer, err := wire.NewSigner(seed)
		if err != nil {
			t.Fatalf("signer %s: %v", slug, err)
		}
		signers[slug] = signer
	}
	return &testPlatform{inst: inst, base: base, tokens: initRes.Tokens, signer: signers}
}

func redeemSeed(base, token string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodPost, base+"/a2a/keys/redeem", strings.NewReader("{}"))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Seed string `json:"seed_base64url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Seed == "" {
		return nil, fmt.Errorf("redeem status %d without seed", resp.StatusCode)
	}
	return base64.RawURLEncoding.DecodeString(out.Seed)
}

func boolPtr(v bool) *bool { return &v }

func newTestChannel(
	t *testing.T,
	platform *testPlatform,
	slug string,
	dryRun bool,
	mutate ...func(*config.A2ASettings),
) (*Channel, *bus.MessageBus) {
	t.Helper()
	seedFile := filepath.Join(t.TempDir(), "ed25519.seed")
	// Write the raw seed again by redeeming (idempotent) to avoid keeping it
	// around in test helpers.
	rawSeed, err := redeemSeed(platform.base, platform.tokens[slug])
	if err != nil {
		t.Fatalf("redeem seed: %v", err)
	}
	if err := os.WriteFile(seedFile, rawSeed, 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	dataDir := t.TempDir()
	stateFile := filepath.Join(dataDir, "state.json")
	bc := &config.Channel{
		Enabled:   true,
		Type:      config.ChannelA2A,
		AllowFrom: config.FlexibleStringSlice{"alpha", "beta"},
	}
	settings := &config.A2ASettings{
		BaseURL:         platform.base,
		AgentSlug:       slug,
		Token:           *config.NewSecureString(platform.tokens[slug]),
		KeyFile:         seedFile,
		PollIntervalSec: 5,
		HTTPTimeoutSec:  5,
		Sign:            boolPtr(true),
		MaxMessageChars: wire.MaxMessageRunes,
		PeersRefreshSec: 30,
		StateFile:       stateFile,
		DryRun:          dryRun,
	}
	for _, fn := range mutate {
		fn(settings)
	}
	mb := bus.NewMessageBus()
	ch, err := NewA2AChannel(bc, settings, mb)
	if err != nil {
		t.Fatalf("NewA2AChannel: %v", err)
	}
	return ch, mb
}

func sendAs(t *testing.T, platform *testPlatform, from, to, content string) *wire.SendResponse {
	t.Helper()
	c := client.New(platform.base, platform.tokens[from], 5*time.Second)
	res, err := c.SendSigned(context.Background(), from, to, content, platform.signer[from], client.SendOptions{
		MaxAttempts: 1,
	})
	if err != nil {
		t.Fatalf("send %s->%s: %v", from, to, err)
	}
	return &wire.SendResponse{
		MessageID:      res.MessageID,
		ConversationID: res.ConversationID,
		Signed:         res.Signed,
		SigState:       res.SigState,
	}
}

func TestA2AChannelInboundOutbound(t *testing.T) {
	platform := startTestPlatform(t, "alpha", "beta")
	sent := sendAs(t, platform, "alpha", "beta", "hello beta")

	ch, mb := newTestChannel(t, platform, "beta", false)
	ctx, cancel := context.WithCancel(context.Background())
	if err := ch.Start(ctx); err != nil {
		t.Fatalf("channel.Start: %v", err)
	}
	defer func() {
		cancel()
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		_ = ch.Stop(stopCtx)
	}()

	var inbound bus.InboundMessage
	select {
	case inbound = <-mb.InboundChan():
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for inbound message")
	}
	if !strings.HasPrefix(inbound.Content, "【A2A 私信】") || !strings.Contains(inbound.Content, "hello beta") {
		t.Fatalf("inbound content missing reply policy or body: %q", inbound.Content)
	}
	if inbound.ChatID != "alpha" {
		t.Fatalf("inbound chat id = %q, want alpha", inbound.ChatID)
	}
	if inbound.Channel != "a2a" {
		t.Fatalf("inbound channel = %q, want a2a", inbound.Channel)
	}
	if inbound.MessageID != sent.MessageID {
		t.Fatalf("inbound message id = %q, want %q", inbound.MessageID, sent.MessageID)
	}
	if inbound.Completion == nil {
		t.Fatal("inbound message has no completion callback")
	}
	inbound.Completion(bus.InboundResult{Status: bus.InboundResultCompleted})

	// Reply goes back to alpha and must be signed by beta.
	ids, err := ch.Send(context.Background(), bus.OutboundMessage{ChatID: "alpha", Content: "hello alpha"})
	if err != nil {
		t.Fatalf("channel.Send: %v", err)
	}
	if len(ids) != 1 || ids[0] == "" {
		t.Fatalf("send ids = %v, want one message id", ids)
	}

	alphaClient := client.New(platform.base, platform.tokens["alpha"], 5*time.Second)
	home, _, _, err := alphaClient.Home(context.Background(), "")
	if err != nil {
		t.Fatalf("alpha home: %v", err)
	}
	if home.UnreadTotal != 1 {
		t.Fatalf("alpha unread = %d, want 1", home.UnreadTotal)
	}
	convID := home.Conversations[0].ID
	detail, err := alphaClient.Conversation(context.Background(), convID, true)
	if err != nil {
		t.Fatalf("alpha conversation: %v", err)
	}
	last := detail.Messages[len(detail.Messages)-1]
	if last.BodyText() != "hello alpha" {
		t.Fatalf("reply body = %q, want %q", last.BodyText(), "hello alpha")
	}
	if !last.Signed || last.SigState != "ok" {
		t.Fatalf("reply signature state = %v/%q, want true/ok", last.Signed, last.SigState)
	}

	// Unread must have been cleared on beta's side after delivery.
	betaHome, _, _, err := client.New(platform.base, platform.tokens["beta"], 5*time.Second).Home(context.Background(), "")
	if err != nil {
		t.Fatalf("beta home: %v", err)
	}
	if betaHome.UnreadTotal != 0 {
		t.Fatalf("beta unread = %d, want 0 after delivery", betaHome.UnreadTotal)
	}

	// State must be persisted with the ETag and seen message. The poller saves
	// right after delivering, so give it a moment.
	statePath := ch.state.Path()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(statePath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("state file was not written: %s", statePath)
		}
		time.Sleep(20 * time.Millisecond)
	}
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var st stateData
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if st.ETag != "" {
		t.Fatalf("state etag = %q, want empty until a clean poll", st.ETag)
	}
	if !ch.state.Seen(sent.ConversationID, sent.MessageID) {
		t.Fatal("delivered message not recorded in last_seen")
	}
}

func TestA2AChannelDryRunDoesNotDeliverOrSend(t *testing.T) {
	platform := startTestPlatform(t, "alpha", "beta")
	sendAs(t, platform, "alpha", "beta", "should stay unread")

	ch, mb := newTestChannel(t, platform, "beta", true)
	ctx, cancel := context.WithCancel(context.Background())
	if err := ch.Start(ctx); err != nil {
		t.Fatalf("channel.Start: %v", err)
	}
	defer func() {
		cancel()
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		_ = ch.Stop(stopCtx)
	}()

	// First poll happens immediately; give it time, then assert nothing was
	// delivered and the conversation is still unread.
	time.Sleep(1500 * time.Millisecond)
	select {
	case msg := <-mb.InboundChan():
		t.Fatalf("dry_run delivered inbound message: %q", msg.Content)
	default:
	}

	betaHome, _, _, err := client.New(platform.base, platform.tokens["beta"], 5*time.Second).Home(context.Background(), "")
	if err != nil {
		t.Fatalf("beta home: %v", err)
	}
	if betaHome.UnreadTotal != 1 {
		t.Fatalf("beta unread = %d, want 1 (dry_run must not clear)", betaHome.UnreadTotal)
	}

	if _, err := ch.Send(context.Background(), bus.OutboundMessage{ChatID: "alpha", Content: "should not send"}); err != nil {
		t.Fatalf("dry_run send returned error: %v", err)
	}
	alphaHome, _, _, err := client.New(platform.base, platform.tokens["alpha"], 5*time.Second).Home(context.Background(), "")
	if err != nil {
		t.Fatalf("alpha home: %v", err)
	}
	if alphaHome.UnreadTotal != 0 {
		t.Fatalf("alpha unread = %d, want 0 (dry_run must not send)", alphaHome.UnreadTotal)
	}
}

func TestA2AChannelRequiresAllowFrom(t *testing.T) {
	bc := &config.Channel{Enabled: true, Type: config.ChannelA2A}
	settings := &config.A2ASettings{BaseURL: "http://127.0.0.1:1", AgentSlug: "beta", Token: *config.NewSecureString("t")}
	if _, err := NewA2AChannel(bc, settings, bus.NewMessageBus()); err == nil {
		t.Fatal("NewA2AChannel accepted an empty allow_from list")
	}
}

func TestSplitRunes(t *testing.T) {
	content := strings.Repeat("你", 10)
	chunks := splitRunes(content, 4)
	if len(chunks) != 3 {
		t.Fatalf("chunks = %d, want 3", len(chunks))
	}
	joined := strings.Join(chunks, "")
	if joined != content {
		t.Fatalf("joined chunks = %q, want %q", joined, content)
	}
	if len([]rune(chunks[0])) != 4 {
		t.Fatalf("first chunk runes = %d, want 4", len([]rune(chunks[0])))
	}
}

func TestChannelStateCorruptReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := loadChannelState(path, "beta")
	if err == nil {
		t.Fatal("loadChannelState accepted corrupt JSON")
	}
	if st == nil || st.ETag() != "" {
		t.Fatal("expected a fresh state store")
	}
	if matches, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "state.json.corrupt.*")); len(matches) != 1 {
		t.Fatalf("corrupt backup files = %v, want exactly one", matches)
	}
}
