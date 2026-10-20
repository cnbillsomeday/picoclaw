// PicoClaw - Ultra-lightweight personal AI agent

package a2a

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/a2a/client"
	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
)

func intPtr(v int) *int { return &v }

func peerUnread(t *testing.T, platform *testPlatform, slug string) int {
	t.Helper()
	home, _, _, err := client.New(platform.base, platform.tokens[slug], 5*time.Second).Home(context.Background(), "")
	if err != nil {
		t.Fatalf("home %s: %v", slug, err)
	}
	return home.UnreadTotal
}

func sendTestMessage(t *testing.T, ch *Channel, peer, content string) ([]string, error) {
	t.Helper()
	return ch.Send(context.Background(), bus.OutboundMessage{ChatID: peer, Content: content})
}

func TestSendSuppressesNoReplySentinel(t *testing.T) {
	platform := startTestPlatform(t, "alpha", "beta")
	ch, _ := newTestChannel(t, platform, "alpha", false)
	ch.SetRunning(true)
	defer ch.SetRunning(false)

	ids, err := sendTestMessage(t, ch, "beta", "  @@NO_REPLY@@  ")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("ids = %v, want none", ids)
	}
	if got := peerUnread(t, platform, "beta"); got != 0 {
		t.Fatalf("beta unread = %d, want 0", got)
	}
}

func TestSendSuppressesAckAndClosureMessages(t *testing.T) {
	platform := startTestPlatform(t, "alpha", "beta")
	ch, _ := newTestChannel(t, platform, "alpha", false)
	ch.SetRunning(true)
	defer ch.SetRunning(false)

	for _, content := range []string{
		"收到", "好的", "OK", "谢谢。", "（不回复，已闭环。）", "  嗯  ",
		"（已闭环，不继续。）", "（不回，沉默即确认。）", "已回复一次，简短确认。", "收到，测试确认完毕。收工。",
	} {
		if ids, err := sendTestMessage(t, ch, "beta", content); err != nil {
			t.Fatalf("Send(%q): %v", content, err)
		} else if len(ids) != 0 {
			t.Fatalf("Send(%q) ids = %v, want suppressed", content, ids)
		}
	}
	if got := peerUnread(t, platform, "beta"); got != 0 {
		t.Fatalf("beta unread = %d, want 0 for acks", got)
	}

	if ids, err := sendTestMessage(t, ch, "beta", "Please review the deployment log."); err != nil {
		t.Fatalf("Send(normal): %v", err)
	} else if len(ids) != 1 {
		t.Fatalf("normal send ids = %v, want 1", ids)
	}
	if got := peerUnread(t, platform, "beta"); got != 1 {
		t.Fatalf("beta unread = %d, want 1", got)
	}
}

func TestSendSentinelWithExtraTextIsNotSuppressed(t *testing.T) {
	platform := startTestPlatform(t, "alpha", "beta")
	ch, _ := newTestChannel(t, platform, "alpha", false)
	ch.SetRunning(true)
	defer ch.SetRunning(false)

	ids, err := sendTestMessage(t, ch, "beta", "@@NO_REPLY@@ but the deploy failed")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("ids = %v, want 1 (sentinel only suppresses an exact message)", ids)
	}
}

func TestSendSuppressesToolFeedback(t *testing.T) {
	platform := startTestPlatform(t, "alpha", "beta")
	ch, _ := newTestChannel(t, platform, "alpha", false)
	ch.SetRunning(true)
	defer ch.SetRunning(false)

	msg := bus.OutboundMessage{
		ChatID:  "beta",
		Content: "🔧 exec Continuing the current task...",
		Context: bus.InboundContext{Raw: map[string]string{"message_kind": "tool_feedback"}},
	}
	if ids, err := ch.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	} else if len(ids) != 0 {
		t.Fatalf("ids = %v, want none", ids)
	}
	if got := peerUnread(t, platform, "beta"); got != 0 {
		t.Fatalf("beta unread = %d, want 0", got)
	}
}

func TestSendRateLimitPerPeer(t *testing.T) {
	platform := startTestPlatform(t, "alpha", "beta")
	ch, _ := newTestChannel(t, platform, "alpha", false)
	ch.SetRunning(true)
	defer ch.SetRunning(false)

	now := time.Now()
	ch.now = func() time.Time { return now }

	for i, content := range []string{"message one", "message two", "message three"} {
		ids, err := sendTestMessage(t, ch, "beta", content)
		if err != nil {
			t.Fatalf("Send #%d: %v", i+1, err)
		}
		if len(ids) != 1 {
			t.Fatalf("Send #%d ids = %v, want 1", i+1, ids)
		}
	}
	if got := peerUnread(t, platform, "beta"); got != 3 {
		t.Fatalf("beta unread = %d, want 3", got)
	}

	ids, err := sendTestMessage(t, ch, "beta", "message four (should be suppressed)")
	if err != nil {
		t.Fatalf("4th Send: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("4th Send ids = %v, want suppressed", ids)
	}
	if got := peerUnread(t, platform, "beta"); got != 3 {
		t.Fatalf("beta unread = %d, want 3 after suppression", got)
	}

	now = now.Add(11 * time.Minute)
	ids, err = sendTestMessage(t, ch, "beta", "message five after the window")
	if err != nil {
		t.Fatalf("5th Send: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("5th Send ids = %v, want 1", ids)
	}
	if got := peerUnread(t, platform, "beta"); got != 4 {
		t.Fatalf("beta unread = %d, want 4", got)
	}
}

func TestSendRateLimitDisabled(t *testing.T) {
	platform := startTestPlatform(t, "alpha", "beta")
	ch, _ := newTestChannel(t, platform, "alpha", false, func(s *config.A2ASettings) {
		s.MaxRepliesPerWindow = intPtr(0)
	})
	ch.SetRunning(true)
	defer ch.SetRunning(false)

	for i := 0; i < 5; i++ {
		if ids, err := sendTestMessage(t, ch, "beta", "message"); err != nil {
			t.Fatalf("Send #%d: %v", i+1, err)
		} else if len(ids) != 1 {
			t.Fatalf("Send #%d ids = %v, want 1", i+1, ids)
		}
	}
	if got := peerUnread(t, platform, "beta"); got != 5 {
		t.Fatalf("beta unread = %d, want 5", got)
	}
}

func TestSendCustomSuppressPattern(t *testing.T) {
	platform := startTestPlatform(t, "alpha", "beta")
	ch, _ := newTestChannel(t, platform, "alpha", false, func(s *config.A2ASettings) {
		s.SuppressPatterns = []string{`(?i)^ping$`}
	})
	ch.SetRunning(true)
	defer ch.SetRunning(false)

	if ids, err := sendTestMessage(t, ch, "beta", "PING"); err != nil {
		t.Fatalf("Send: %v", err)
	} else if len(ids) != 0 {
		t.Fatalf("ids = %v, want suppressed", ids)
	}
}

func TestReplyPolicyDefaultsAndFileOverride(t *testing.T) {
	platform := startTestPlatform(t, "alpha", "beta")

	ch, _ := newTestChannel(t, platform, "alpha", false)
	if !strings.Contains(ch.applyReplyPolicy("body"), defaultReplyPolicy) {
		t.Fatal("default reply policy was not applied")
	}

	policyFile := filepath.Join(t.TempDir(), "reply_policy.txt")
	if err := os.WriteFile(policyFile, []byte("CUSTOM POLICY"), 0o600); err != nil {
		t.Fatal(err)
	}
	ch2, _ := newTestChannel(t, platform, "alpha", false, func(s *config.A2ASettings) {
		s.ReplyPolicyFile = policyFile
	})
	if got := ch2.applyReplyPolicy("body"); !strings.Contains(got, "CUSTOM POLICY") {
		t.Fatalf("custom policy not applied: %q", got)
	}
	if ch2.replyPolicy != "CUSTOM POLICY" {
		t.Fatalf("replyPolicy = %q, want CUSTOM POLICY", ch2.replyPolicy)
	}
}

func TestReplyPolicyMissingFileFailsClosed(t *testing.T) {
	platform := startTestPlatform(t, "alpha", "beta")
	bc := &config.Channel{Enabled: true, Type: config.ChannelA2A, AllowFrom: config.FlexibleStringSlice{"alpha", "beta"}}
	disabledSign := false
	settings := &config.A2ASettings{
		BaseURL:         platform.base,
		AgentSlug:       "alpha",
		Token:           *config.NewSecureString(platform.tokens["alpha"]),
		Sign:            &disabledSign,
		ReplyPolicyFile: filepath.Join(t.TempDir(), "missing.txt"),
	}
	if _, err := NewA2AChannel(bc, settings, bus.NewMessageBus()); err == nil {
		t.Fatal("missing reply_policy_file must fail channel construction")
	}
}

func TestRateLimitStatePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	now := time.Now()
	st := &channelState{path: path, data: stateData{Version: stateVersion, Agent: "alpha"}}
	if !st.AllowOutbound("beta", now, 10*time.Minute, 3) {
		t.Fatal("first outbound must be allowed")
	}
	st.RecordOutbound("beta", now, 10*time.Minute)
	if err := st.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := loadChannelState(path, "alpha")
	if err != nil {
		t.Fatalf("loadChannelState: %v", err)
	}
	if loaded.AllowOutbound("beta", now.Add(time.Minute), 10*time.Minute, 1) {
		t.Fatal("persisted outbound count was not loaded")
	}
}
