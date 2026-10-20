// PicoClaw - Ultra-lightweight personal AI agent

package a2a

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
)

func notifySettings(mode string) func(*config.A2ASettings) {
	return func(settings *config.A2ASettings) {
		settings.NotifyChannel = "telegram"
		settings.NotifyChatID = "owner-chat"
		settings.NotifyMode = mode
		settings.NotifyMaxChars = 120
	}
}

func receiveNotification(t *testing.T, mb *bus.MessageBus) bus.OutboundMessage {
	t.Helper()
	select {
	case msg := <-mb.OutboundChan():
		return msg
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for owner notification")
		return bus.OutboundMessage{}
	}
}

func assertNoNotification(t *testing.T, mb *bus.MessageBus) {
	t.Helper()
	select {
	case msg := <-mb.OutboundChan():
		t.Fatalf("unexpected owner notification: channel=%q content=%q", msg.Channel, msg.Content)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestOwnerNotificationAllModes(t *testing.T) {
	platform := startTestPlatform(t, "alpha", "beta")
	sendAs(t, platform, "alpha", "beta", "incoming body")

	ch, mb := newTestChannel(t, platform, "beta", false, notifySettings("all"))
	ch.SetRunning(true)
	if err := ch.pollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	inbound := <-mb.InboundChan()
	inbound.Completion(bus.InboundResult{Status: bus.InboundResultCompleted, Reply: "reply body"})
	msg := receiveNotification(t, mb)
	if msg.Channel != "telegram" || msg.ChatID != "owner-chat" {
		t.Fatalf("notification target = %q/%q", msg.Channel, msg.ChatID)
	}
	if !strings.Contains(msg.Content, "alpha") || !strings.Contains(msg.Content, "incoming body") ||
		!strings.Contains(msg.Content, "reply body") {
		t.Fatalf("notification content = %q", msg.Content)
	}
}

func TestOwnerNotificationReplyOnlyAfterSuccessfulSend(t *testing.T) {
	platform := startTestPlatform(t, "alpha", "beta")
	sendAs(t, platform, "alpha", "beta", "incoming body")

	ch, mb := newTestChannel(t, platform, "beta", false, notifySettings("reply"))
	ch.SetRunning(true)
	if err := ch.pollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	inbound := <-mb.InboundChan()
	inbound.Completion(bus.InboundResult{Status: bus.InboundResultCompleted, Reply: "queued reply"})
	assertNoNotification(t, mb)

	ctx := inbound.Context
	ids, err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "alpha",
		Context: ctx,
		Content: "actual reply",
	})
	if err != nil {
		t.Fatalf("send reply: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("reply ids = %v", ids)
	}
	msg := receiveNotification(t, mb)
	if !strings.Contains(msg.Content, "actual reply") || !strings.Contains(msg.Content, "incoming body") {
		t.Fatalf("notification content = %q", msg.Content)
	}

	// A second outbound for the same inbound message must not notify twice.
	if _, err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "alpha",
		Context: ctx,
		Content: "duplicate reply",
	}); err != nil {
		t.Fatalf("send duplicate: %v", err)
	}
	assertNoNotification(t, mb)
}

func TestOwnerNotificationFailureMode(t *testing.T) {
	platform := startTestPlatform(t, "alpha", "beta")
	sendAs(t, platform, "alpha", "beta", "will fail")

	ch, mb := newTestChannel(t, platform, "beta", false, notifySettings("failure"))
	ch.SetRunning(true)
	if err := ch.pollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	inbound := <-mb.InboundChan()
	inbound.Completion(bus.InboundResult{Status: bus.InboundResultError, Error: "provider failed"})
	msg := receiveNotification(t, mb)
	if !strings.Contains(msg.Content, "failed") || !strings.Contains(msg.Content, "provider failed") {
		t.Fatalf("failure notification content = %q", msg.Content)
	}
}

func TestOwnerNotificationOffByDefault(t *testing.T) {
	platform := startTestPlatform(t, "alpha", "beta")
	sendAs(t, platform, "alpha", "beta", "no notification")

	ch, mb := newTestChannel(t, platform, "beta", false)
	ch.SetRunning(true)
	if err := ch.pollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	inbound := <-mb.InboundChan()
	inbound.Completion(bus.InboundResult{Status: bus.InboundResultCompleted, Reply: "reply"})
	assertNoNotification(t, mb)
}

func TestOwnerNotificationAllWithoutReply(t *testing.T) {
	platform := startTestPlatform(t, "alpha", "beta")
	sendAs(t, platform, "alpha", "beta", "incoming body")

	ch, mb := newTestChannel(t, platform, "beta", false, notifySettings("all"))
	ch.SetRunning(true)
	if err := ch.pollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	inbound := <-mb.InboundChan()
	inbound.Completion(bus.InboundResult{Status: bus.InboundResultCompleted})
	msg := receiveNotification(t, mb)
	if !strings.Contains(msg.Content, "回复：（无）") && !strings.Contains(msg.Content, "回复: （无）") {
		t.Fatalf("notification should say no reply: %q", msg.Content)
	}
}

func TestOwnerNotificationDedupePersists(t *testing.T) {
	path := t.TempDir() + "/state.json"
	st := &channelState{path: path, data: stateData{Version: stateVersion, Agent: "beta"}}
	if !st.MarkNotified("c_1", "m_1") {
		t.Fatal("first notification claim was rejected")
	}
	if st.MarkNotified("c_1", "m_1") {
		t.Fatal("duplicate notification claim was accepted")
	}
	if err := st.Save(); err != nil {
		t.Fatalf("save state: %v", err)
	}
	loaded, err := loadChannelState(path, "beta")
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if loaded.MarkNotified("c_1", "m_1") {
		t.Fatal("notification dedupe did not survive reload")
	}
}
