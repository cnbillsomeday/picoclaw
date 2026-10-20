// PicoClaw - Ultra-lightweight personal AI agent

package a2a

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/a2a/client"
	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
)

func regressionIntPtr(v int) *int { return &v }

// TestClosureMentionIsNotSuppressed guards the tightened ack heuristic: a real
// reply that merely mentions a closure word must go out.
func TestClosureMentionIsNotSuppressed(t *testing.T) {
	platform := startTestPlatform(t, "alpha", "beta")
	ch, _ := newTestChannel(t, platform, "alpha", false)
	ch.SetRunning(true)
	defer ch.SetRunning(false)

	for _, content := range []string{
		"好的，收工前再确认下日志",
		"收到，我马上处理",
		"已回复过一次，但对方又问了新问题",
	} {
		ids, err := sendTestMessage(t, ch, "beta", content)
		if err != nil {
			t.Fatalf("Send(%q): %v", content, err)
		}
		if len(ids) != 1 {
			t.Fatalf("Send(%q) ids = %v, want 1 (a real reply must not be swallowed)", content, ids)
		}
	}
}

// TestReplayedNonceCountsAsDelivered: a 403 "replayed nonce" means the earlier
// attempt landed, so the send must still consume rate-limit budget (otherwise
// the limiter and the owner notification both lie about what was sent).
func TestReplayedNonceCountsAsDelivered(t *testing.T) {
	platform := startTestPlatform(t, "alpha", "beta")
	ch, _ := newTestChannel(t, platform, "alpha", false, func(s *config.A2ASettings) {
		s.MaxRepliesPerWindow = regressionIntPtr(1)
		s.ReplyWindowSec = regressionIntPtr(600)
	})
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/a2a/beta" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "error": "replayed nonce"})
	}))
	defer fake.Close()
	ch.client = client.New(fake.URL, platform.tokens["alpha"], 5*time.Second)
	ch.SetRunning(true)
	defer ch.SetRunning(false)

	ids, err := ch.Send(context.Background(), bus.OutboundMessage{ChatID: "beta", Content: "hello beta"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("ids = %v, want none (the platform id is unknown on replay)", ids)
	}
	if ch.state.AllowOutbound("beta", ch.currentTime(), ch.replyWindow, ch.maxReplies) {
		t.Fatal("a replayed-nonce delivery was not counted against the rate limit")
	}
}

// TestProactiveSendHonoursRateLimit: the a2a_send path must share the same
// per-peer cap as automatic replies.
func TestProactiveSendHonoursRateLimit(t *testing.T) {
	platform := startTestPlatform(t, "alpha", "beta")
	ch, _ := newTestChannel(t, platform, "alpha", false, func(s *config.A2ASettings) {
		s.MaxRepliesPerWindow = regressionIntPtr(1)
		s.ReplyWindowSec = regressionIntPtr(600)
	})
	ch.SetRunning(true)
	defer ch.SetRunning(false)

	ids, err := ch.SendProactive(context.Background(), "beta", "first proactive", "sk-1")
	if err != nil {
		t.Fatalf("first SendProactive: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("first send ids = %v, want 1", ids)
	}
	if _, err := ch.SendProactive(context.Background(), "beta", "second proactive", "sk-1"); err == nil {
		t.Fatal("second proactive send bypassed the rate limit")
	}
	if got := peerUnread(t, platform, "beta"); got != 1 {
		t.Fatalf("beta unread = %d, want 1 (only the first message)", got)
	}
}

func TestAllowFromMatchingIsCaseInsensitive(t *testing.T) {
	platform := startTestPlatform(t, "alpha", "beta")
	ch, _ := newTestChannel(t, platform, "alpha", false)
	if !ch.isPeerAllowed("ALPHA") {
		t.Fatal("allow_from matching rejected a case variant of an allowed peer")
	}
	if ch.isPeerAllowed("gamma") {
		t.Fatal("allow_from accepted a peer that is not listed")
	}
}

// TestDeadInboundEntriesArePruned: dead letters must not accumulate forever.
func TestDeadInboundEntriesArePruned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := loadChannelState(path, "alpha")
	if err != nil {
		t.Fatalf("loadChannelState: %v", err)
	}
	old := time.Now().Add(-8 * 24 * time.Hour)
	st.MarkInboundDead("c_1", "m_old", old, "boom")
	if !st.IsInboundDead("c_1", "m_old") {
		t.Fatal("dead entry was not recorded")
	}
	st.gcInbound(time.Now())
	if entry := st.InboundEntry("c_1", "m_old"); entry != nil {
		t.Fatalf("expired dead entry survived the sweep: %+v", entry)
	}
}

// TestInboundGCKeepsRetryableEntries: delivery state is never collected, only
// dead letters are.
func TestInboundGCKeepsRetryableEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := loadChannelState(path, "alpha")
	if err != nil {
		t.Fatalf("loadChannelState: %v", err)
	}
	old := time.Now().Add(-8 * 24 * time.Hour)
	st.MarkInboundFailed("c_1", "m_retry", old, time.Minute, "temporary")
	st.gcInbound(time.Now())
	entry := st.InboundEntry("c_1", "m_retry")
	if entry == nil || entry.Status != "failed" {
		t.Fatalf("retryable entry was collected: %+v", entry)
	}
}

// TestUnmarkNotifiedAllowsRetry: a failed owner notification must be retryable.
func TestUnmarkNotifiedAllowsRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := loadChannelState(path, "alpha")
	if err != nil {
		t.Fatalf("loadChannelState: %v", err)
	}
	if !st.MarkNotified("c_1", "m_1") {
		t.Fatal("first MarkNotified returned false")
	}
	if st.MarkNotified("c_1", "m_1") {
		t.Fatal("duplicate MarkNotified returned true")
	}
	st.UnmarkNotified("c_1", "m_1")
	if !st.MarkNotified("c_1", "m_1") {
		t.Fatal("notification claim was not rolled back")
	}
}

// TestStateBackupsArePruned keeps stale backups (which hold message previews)
// from piling up.
func TestStateBackupsArePruned(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	for i := 0; i < 4; i++ {
		name := filepath.Join(dir, "state.json.corrupt."+time.Now().Add(time.Duration(i)*time.Second).Format("150405"))
		if err := os.WriteFile(name, []byte("{}"), 0o600); err != nil {
			t.Fatalf("write backup: %v", err)
		}
	}
	pruneStateBackups(path)
	matches, err := filepath.Glob(path + ".corrupt.*")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) > maxStateBackups {
		t.Fatalf("backups kept = %d, want at most %d", len(matches), maxStateBackups)
	}
}
