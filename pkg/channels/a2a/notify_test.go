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
	if !strings.Contains(msg.Content, "- 我说：") || !strings.Contains(msg.Content, "（无）") {
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

// TestFormatOwnerNotificationTemplate locks the wake-summary layout and the
// bottom token statistics line.
func TestFormatOwnerNotificationTemplate(t *testing.T) {
	at := time.Date(2026, 10, 21, 0, 20, 5, 0, time.Local)
	inboundAt := time.Date(2026, 10, 21, 16, 19, 57, 0, time.Local)
	got := formatOwnerNotification(
		at,
		"法龙（fa）",
		"completed",
		"CROSS-OK（跨机链路联通确认）",
		inboundAt,
		"确认链路正常，回了一句 CROSS-OK 收到、我这边也通了（签名发送成功 rc=0）",
		"",
		bus.TurnUsage{TokensIn: 63195, TokensOut: 299, LLMCalls: 3, Iterations: 3},
		400,
	)
	want := "本轮唤醒小结（2026-10-21 00:20:05）\n\n" +
		"新消息 1 条\n" +
		"1、法龙（fa）16:19:57：\n" +
		"- ta说：\n" +
		"    CROSS-OK（跨机链路联通确认）\n" +
		"- 我说：\n" +
		"    确认链路正常，回了一句 CROSS-OK 收到、我这边也通了（签名发送成功 rc=0）\n" +
		"\n" +
		"📊 Token：入 63,195 / 出 299 / 合计 63,494（3 次调用，3 轮迭代）"
	if got != want {
		t.Fatalf("notification template mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestFormatOwnerNotificationFailure includes the failure status and error.
func TestFormatOwnerNotificationFailure(t *testing.T) {
	at := time.Date(2026, 10, 21, 0, 20, 5, 0, time.Local)
	got := formatOwnerNotification(at, "alpha", "failed", "incoming body", time.Time{},
		"", "provider failed", bus.TurnUsage{TokensIn: 10, TokensOut: 2, LLMCalls: 1}, 400)
	for _, want := range []string{"- 结果：", "failed：provider failed", "📊 Token：入 10 / 出 2 / 合计 12（1 次调用，0 轮迭代）"} {
		if !strings.Contains(got, want) {
			t.Fatalf("failure notification missing %q:\n%s", want, got)
		}
	}
}

// TestNotificationCarriesTurnUsage is the end-to-end path: completion results
// carry usage and the owner notification prints it.
func TestNotificationCarriesTurnUsage(t *testing.T) {
	platform := startTestPlatform(t, "alpha", "beta")
	sendAs(t, platform, "alpha", "beta", "incoming body")

	ch, mb := newTestChannel(t, platform, "beta", false, notifySettings("all"))
	ch.SetRunning(true)
	if err := ch.pollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	inbound := <-mb.InboundChan()
	inbound.Completion(bus.InboundResult{
		Status: bus.InboundResultCompleted,
		Reply:  "reply body",
		Usage:  bus.TurnUsage{TokensIn: 1234, TokensOut: 56, LLMCalls: 2, Iterations: 1},
	})
	msg := receiveNotification(t, mb)
	for _, want := range []string{
		"本轮唤醒小结（",
		"新消息 1 条",
		"1、alpha ",
		"- ta说：",
		"- 我说：",
		"📊 Token：入 1,234 / 出 56 / 合计 1,290（2 次调用，1 轮迭代）",
	} {
		if !strings.Contains(msg.Content, want) {
			t.Fatalf("notification missing %q:\n%s", want, msg.Content)
		}
	}
}

func TestFormatThousandsGroupsDigits(t *testing.T) {
	cases := map[int]string{
		0: "0", 7: "7", 999: "999", 1000: "1,000",
		63195: "63,195", 1234567: "1,234,567",
	}
	for in, want := range cases {
		if got := formatThousands(in); got != want {
			t.Fatalf("formatThousands(%d) = %q, want %q", in, got, want)
		}
	}
}
