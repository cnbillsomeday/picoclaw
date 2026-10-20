// PicoClaw - Ultra-lightweight personal AI agent

package a2a

import (
	"context"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/logger"
)

// notifyOwnerForInbound sends one owner notification for a terminal inbound
// turn. Notifications are at-most-once per conversation/message.
func (c *Channel) notifyOwnerForInbound(
	convID, peer, msgID, inboundPreview, reply string,
	result bus.InboundResult,
	status string,
) {
	if c == nil || c.msgBus == nil || !c.ownerNotificationsEnabled() {
		return
	}
	send := false
	switch c.notifyMode {
	case "all":
		send = status == "completed"
	case "failure":
		send = status != "" && status != "completed"
	default:
		return
	}
	if !send {
		return
	}
	c.publishOwnerNotification(convID, msgID, peer, status, inboundPreview, reply, result.Error)
}

// notifyReplySent is called only after the A2A reply has actually been
// delivered successfully by the platform client.
func (c *Channel) notifyReplySent(msg bus.OutboundMessage) {
	if c == nil || c.msgBus == nil || c.notifyMode != "reply" || !c.ownerNotificationsEnabled() {
		return
	}
	convID := strings.TrimSpace(msg.Context.Raw[notifyConversationKey])
	msgID := strings.TrimSpace(msg.Context.MessageID)
	if convID == "" || msgID == "" {
		return
	}
	inboundPreview := strings.TrimSpace(msg.Context.Raw[notifyInboundKey])
	c.publishOwnerNotification(convID, msgID, normalizePeer(msg.ChatID), "completed", inboundPreview, msg.Content, "")
}

func (c *Channel) ownerNotificationsEnabled() bool {
	return c != nil && !c.dryRun && c.notifyMode != "off" &&
		strings.TrimSpace(c.notifyChannel) != "" && strings.TrimSpace(c.notifyChatID) != ""
}

func (c *Channel) publishOwnerNotification(
	convID, msgID, peer, status, inboundPreview, reply, failure string,
) {
	if c == nil || c.state == nil {
		return
	}
	if !c.state.MarkNotified(convID, msgID) {
		logger.DebugCF("a2a", "owner notification deduplicated", map[string]any{
			"agent":        c.agentSlug,
			"conversation": convID,
			"message":      msgID,
			"mode":         c.notifyMode,
		})
		return
	}

	content := formatOwnerNotification(
		c.currentTime().UTC(), peer, status, inboundPreview, reply, failure, c.notifyMaxLen,
	)
	// Persist the at-most-once claim before publishing. A crash after this
	// point may miss one notification, but can never duplicate one.
	if err := c.state.Save(); err != nil {
		logger.WarnCF("a2a", "failed to save owner notification state", map[string]any{
			"agent": c.agentSlug,
			"error": err.Error(),
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := c.msgBus.PublishOutbound(ctx, bus.OutboundMessage{
		Channel: c.notifyChannel,
		ChatID:  c.notifyChatID,
		Context: bus.NewOutboundContext(c.notifyChannel, c.notifyChatID, ""),
		Content: content,
	})
	if err != nil {
		logger.WarnCF("a2a", "failed to publish owner notification", map[string]any{
			"agent":        c.agentSlug,
			"conversation": convID,
			"message":      msgID,
			"channel":      c.notifyChannel,
			"error":        err.Error(),
		})
	} else {
		logger.InfoCF("a2a", "owner notification published", map[string]any{
			"agent":        c.agentSlug,
			"conversation": convID,
			"message":      msgID,
			"channel":      c.notifyChannel,
			"mode":         c.notifyMode,
			"status":       status,
		})
	}
}

func inboundReply(reply string, isAck func(string) bool) string {
	reply = strings.TrimSpace(reply)
	if reply == "" || reply == noReplySentinel {
		return ""
	}
	if isAck != nil && isAck(reply) {
		return ""
	}
	return reply
}

func formatOwnerNotification(
	at time.Time,
	peer, status, inboundPreview, reply, failure string,
	maxChars int,
) string {
	if maxChars <= 0 {
		maxChars = 400
	}
	var out strings.Builder
	out.WriteString("【A2A 私信】\n")
	out.WriteString("时间: " + at.Format(time.RFC3339) + "\n")
	out.WriteString("来源: " + firstNonEmpty(peer, "unknown") + "\n")
	out.WriteString("状态: " + firstNonEmpty(status, "completed") + "\n")
	out.WriteString("来信: " + previewText(inboundPreview, maxChars) + "\n")
	if reply = strings.TrimSpace(reply); reply != "" && reply != noReplySentinel {
		out.WriteString("回复: " + previewText(reply, maxChars) + "\n")
	} else {
		out.WriteString("回复: （无）\n")
	}
	if strings.TrimSpace(failure) != "" {
		out.WriteString("错误: " + previewText(failure, maxChars) + "\n")
	}
	return strings.TrimSpace(out.String())
}
