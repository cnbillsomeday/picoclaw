// PicoClaw - Ultra-lightweight personal AI agent

package a2a

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/a2a/client"
	"github.com/sipeed/picoclaw/pkg/a2a/wire"
	"github.com/sipeed/picoclaw/pkg/bus"
)

func TestInboundFailureRetriesThenSucceeds(t *testing.T) {
	platform := startTestPlatform(t, "alpha", "beta")
	sendAs(t, platform, "alpha", "beta", "please answer")

	ch, mb := newTestChannel(t, platform, "beta", false)
	ch.SetRunning(true)
	now := time.Now()
	ch.now = func() time.Time { return now }

	if err := ch.pollOnce(context.Background()); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	first := <-mb.InboundChan()
	if first.Completion == nil {
		t.Fatal("first inbound has no completion")
	}
	first.Completion(bus.InboundResult{Status: bus.InboundResultError, Error: "forced 400"})

	if got := peerUnread(t, platform, "beta"); got != 1 {
		t.Fatalf("beta unread after failure = %d, want 1", got)
	}
	if !ch.state.HasPendingInbound() {
		t.Fatal("failed message is not marked pending")
	}

	now = now.Add(61 * time.Second)
	if err := ch.pollOnce(context.Background()); err != nil {
		t.Fatalf("retry poll: %v", err)
	}
	var second bus.InboundMessage
	select {
	case second = <-mb.InboundChan():
	case <-time.After(2 * time.Second):
		t.Fatal("retry was not delivered")
	}
	if second.MessageID != first.MessageID {
		t.Fatalf("retry message id = %q, want %q", second.MessageID, first.MessageID)
	}
	second.Completion(bus.InboundResult{Status: bus.InboundResultCompleted})

	if got := peerUnread(t, platform, "beta"); got != 0 {
		t.Fatalf("beta unread after retry = %d, want 0", got)
	}
	if ch.state.HasPendingInbound() {
		t.Fatal("pending state not cleared after success")
	}
	if !ch.state.Seen(first.ChatID, first.MessageID) && !ch.state.Seen("c_4163834372780d81", first.MessageID) {
		t.Fatal("message not recorded as seen after completion")
	}
}

func TestETagNotCommittedWhenPollFails(t *testing.T) {
	platform := startTestPlatform(t, "alpha", "beta")
	ch, _ := newTestChannel(t, platform, "beta", false)
	ch.SetRunning(true)

	var ifNoneMatch []string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/home":
			ifNoneMatch = append(ifNoneMatch, r.Header.Get("If-None-Match"))
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("ETag", `W/"etag-one"`)
			_ = json.NewEncoder(w).Encode(wire.Home{
				Success: true,
				Conversations: []wire.Conversation{{
					ID:          "c_1",
					WithAgent:   wire.Agent{Slug: "alpha"},
					UnreadCount: 1,
					UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
				}},
				UnreadTotal: 1,
			})
		case r.URL.Path == "/api/v1/agents":
			_, _ = io.WriteString(w, `{"agents":[]}`)
		case strings.HasPrefix(r.URL.Path, "/api/v1/a2a/conversations/"):
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer fake.Close()
	ch.client = client.New(fake.URL, platform.tokens["beta"], 5*time.Second)

	if err := ch.pollOnce(context.Background()); err == nil {
		t.Fatal("poll should fail on conversation error")
	}
	if got := ch.state.ETag(); got != "" {
		t.Fatalf("etag after failed poll = %q, want empty", got)
	}
	_ = ch.pollOnce(context.Background())
	if len(ifNoneMatch) < 2 {
		t.Fatalf("home calls = %d, want at least 2", len(ifNoneMatch))
	}
	if ifNoneMatch[1] != "" {
		t.Fatalf("second home If-None-Match = %q, want empty (no 304 after failure)", ifNoneMatch[1])
	}
}

func TestDryRunDoesNotPersistETag(t *testing.T) {
	platform := startTestPlatform(t, "alpha", "beta")
	sendAs(t, platform, "beta", "alpha", "dry run message")
	ch, _ := newTestChannel(t, platform, "alpha", true)
	ch.SetRunning(true)
	if err := ch.pollOnce(context.Background()); err != nil {
		t.Fatalf("dry-run poll: %v", err)
	}
	if got := ch.state.ETag(); got != "" {
		t.Fatalf("dry-run etag = %q, want empty", got)
	}
	if got := peerUnread(t, platform, "alpha"); got != 1 {
		t.Fatalf("alpha unread = %d, want 1 (dry run must not clear)", got)
	}
}

func TestInboundRequireSignRejectsUnsigned(t *testing.T) {
	platform := startTestPlatformWithMode(t, "warn", "alpha", "beta")
	req, err := http.NewRequest(http.MethodPost, platform.base+"/a2a/beta", strings.NewReader(`{"content":"unsigned message"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+platform.tokens["alpha"])
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unsigned send status = %d, want 200 in warn mode", resp.StatusCode)
	}

	ch, mb := newTestChannel(t, platform, "beta", false)
	ch.SetRunning(true)
	if err := ch.pollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	select {
	case msg := <-mb.InboundChan():
		t.Fatalf("unsigned message was delivered: %q", msg.Content)
	case <-time.After(200 * time.Millisecond):
	}
	// The next clean poll should clear the rejected message.
	_ = ch.pollOnce(context.Background())
	if got := peerUnread(t, platform, "beta"); got != 0 {
		t.Fatalf("beta unread = %d, want 0 after rejected message is cleared", got)
	}
}

func TestInboundStateDeadLetterAndStaleCallback(t *testing.T) {
	st := &channelState{path: filepath.Join(t.TempDir(), "state.json"), data: stateData{
		Version: stateVersion, Agent: "alpha", Inbound: map[string]map[string]*inboundEntry{},
	}}
	now := time.Now()
	if attempt := st.MarkInboundPending("c_1", "m_1", now, "hello"); attempt != 1 {
		t.Fatalf("attempt = %d, want 1", attempt)
	}
	status, applied := st.ApplyInboundResult("c_1", "m_1", 1, now, bus.InboundResultError, "boom", time.Minute, 1)
	if !applied || status != "dead" {
		t.Fatalf("status=%q applied=%v, want dead/true", status, applied)
	}
	if !st.Seen("c_1", "m_1") {
		t.Fatal("dead-lettered message not marked seen")
	}
	if st.HasPendingInbound() {
		t.Fatal("dead message still counts as pending")
	}
	if _, applied := st.ApplyInboundResult("c_1", "m_1", 1, now, bus.InboundResultCompleted, "", time.Minute, 1); applied {
		t.Fatal("stale callback was applied")
	}

	if attempt := st.MarkInboundPending("c_2", "m_2", now, "hello"); attempt != 1 {
		t.Fatalf("attempt = %d, want 1", attempt)
	}
	status, applied = st.ApplyInboundResult("c_2", "m_2", 1, now, bus.InboundResultError, "retry", time.Minute, 3)
	if !applied || status != "failed" {
		t.Fatalf("status=%q applied=%v, want failed/true", status, applied)
	}
	if attempt := st.MarkInboundPending("c_2", "m_2", now.Add(2*time.Minute), "hello"); attempt != 2 {
		t.Fatalf("retry attempt = %d, want 2", attempt)
	}
	if _, applied := st.ApplyInboundResult("c_2", "m_2", 1, now, bus.InboundResultCompleted, "", time.Minute, 3); applied {
		t.Fatal("stale attempt-1 callback applied after retry")
	}
}

func TestPickDeliverableSkipsReadHistoryWithoutState(t *testing.T) {
	st := &channelState{path: filepath.Join(t.TempDir(), "state.json"), data: stateData{
		Version: stateVersion, Agent: "beta",
	}}
	ch := &Channel{state: st, agentSlug: "beta"}
	read := wire.Message{
		ID: "m_read", From: "alpha", To: "beta", Body: "old message", Read: true,
	}
	msg, ok := ch.pickDeliverable("c_1", []wire.Message{read}, time.Now())
	if ok {
		t.Fatalf("read history message was delivered: %q", msg.ID)
	}
	if !st.Seen("c_1", "m_read") {
		t.Fatal("read history message was not marked seen")
	}

	fresh := wire.Message{ID: "m_new", From: "alpha", To: "beta", Body: "new message"}
	msg, ok = ch.pickDeliverable("c_1", []wire.Message{fresh}, time.Now())
	if !ok || msg.ID != fresh.ID {
		t.Fatalf("pick = %q/%v, want new message", msg.ID, ok)
	}
}

func TestPickDeliverableRetriesReadMessageWithFailedState(t *testing.T) {
	st := &channelState{path: filepath.Join(t.TempDir(), "state.json"), data: stateData{
		Version: stateVersion, Agent: "beta",
	}}
	now := time.Now()
	st.MarkInboundPending("c_1", "m_retry", now, "hello")
	status, applied := st.ApplyInboundResult("c_1", "m_retry", 1, now, bus.InboundResultError, "boom", time.Minute, 3)
	if !applied || status != "failed" {
		t.Fatalf("status=%q applied=%v, want failed/true", status, applied)
	}

	ch := &Channel{state: st, agentSlug: "beta", inboundMaxAttempts: 3}
	msg := wire.Message{ID: "m_retry", From: "alpha", To: "beta", Body: "hello", Read: true}
	got, ok := ch.pickDeliverable("c_1", []wire.Message{msg}, now.Add(2*time.Minute))
	if !ok || got.ID != msg.ID {
		t.Fatalf("pick = %q/%v, want retryable read message", got.ID, ok)
	}
}

func TestDryRunThenLiveCatchesUp(t *testing.T) {
	platform := startTestPlatform(t, "alpha", "beta")
	sendAs(t, platform, "beta", "alpha", "queued during dry run")

	dry, _ := newTestChannel(t, platform, "alpha", true)
	dry.SetRunning(true)
	if err := dry.pollOnce(context.Background()); err != nil {
		t.Fatalf("dry-run poll: %v", err)
	}
	if got := peerUnread(t, platform, "alpha"); got != 1 {
		t.Fatalf("alpha unread after dry run = %d, want 1", got)
	}

	live, mb := newTestChannel(t, platform, "alpha", false)
	live.state = dry.state // simulate switching the same installation to live
	live.SetRunning(true)
	if err := live.pollOnce(context.Background()); err != nil {
		t.Fatalf("live poll: %v", err)
	}
	select {
	case msg := <-mb.InboundChan():
		if !strings.Contains(msg.Content, "queued during dry run") {
			t.Fatalf("inbound content = %q", msg.Content)
		}
		msg.Completion(bus.InboundResult{Status: bus.InboundResultCompleted})
	case <-time.After(2 * time.Second):
		t.Fatal("dry-run backlog was not delivered after switching to live")
	}
	if got := peerUnread(t, platform, "alpha"); got != 0 {
		t.Fatalf("alpha unread after live = %d, want 0", got)
	}
}
