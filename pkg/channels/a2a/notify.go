// PicoClaw - Ultra-lightweight personal AI agent

package a2a

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/logger"
)

// noticeTimeLayout is the wall-clock format used in owner notifications.
const noticeTimeLayout = "2006-01-02 15:04:05"

// notifyOwnerForInbound sends one owner notification for a terminal inbound
// turn. Notifications are at-most-once per conversation/message.
func (c *Channel) notifyOwnerForInbound(
	convID, peer, msgID, inboundPreview string,
	inboundAt time.Time,
	reply string,
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
	c.publishOwnerNotification(ownerNotice{
		convID:    convID,
		msgID:     msgID,
		peer:      peer,
		status:    status,
		inbound:   inboundPreview,
		inboundAt: inboundAt,
		reply:     reply,
		failure:   result.Error,
		usage:     result.Usage,
	})
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
	c.publishOwnerNotification(ownerNotice{
		convID:    convID,
		msgID:     msgID,
		peer:      normalizePeer(msg.ChatID),
		status:    "completed",
		inbound:   strings.TrimSpace(msg.Context.Raw[notifyInboundKey]),
		inboundAt: parseMessageTime(msg.Context.Raw[notifyInboundAtKey]),
		reply:     msg.Content,
		usage:     msg.Usage,
	})
}

// ownerNotice is one inbound message and what happened to it.
type ownerNotice struct {
	convID    string
	msgID     string
	peer      string
	status    string
	inbound   string
	inboundAt time.Time
	reply     string
	failure   string
	usage     bus.TurnUsage
}

func (c *Channel) ownerNotificationsEnabled() bool {
	return c != nil && !c.dryRun && c.notifyMode != "off" &&
		strings.TrimSpace(c.notifyChannel) != "" && strings.TrimSpace(c.notifyChatID) != ""
}

func (c *Channel) publishOwnerNotification(notice ownerNotice) {
	convID, msgID := notice.convID, notice.msgID
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
		c.currentTime(),
		c.peerNoticeLabel(notice.peer),
		notice.status,
		notice.inbound,
		notice.inboundAt,
		notice.reply,
		notice.failure,
		notice.usage,
		c.notifyMaxLen,
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
		// Roll the at-most-once claim back so a later turn/retry can still
		// alert the owner. This trades a rare duplicate for not losing an
		// alert; the alternative (keep the claim) silently drops the message.
		c.state.UnmarkNotified(convID, msgID)
		if saveErr := c.state.Save(); saveErr != nil {
			logger.WarnCF("a2a", "failed to roll back owner notification state", map[string]any{
				"agent": c.agentSlug,
				"error": saveErr.Error(),
			})
		}
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
			"status":       notice.status,
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

// formatOwnerNotification renders the "wake-up summary" sent to the owner:
//
//	本轮唤醒小结（2026-10-21 00:20:05）
//
//	新消息 1 条
//	1、法龙（fa）16:19:57：
//	- ta说：
//	    CROSS-OK（跨机链路联通确认）
//	- 我说：
//	    确认链路正常……
//
//	📊 Token：入 63,195 / 出 299 / 合计 63,494（3 次调用，3 轮迭代）
func formatOwnerNotification(
	at time.Time,
	peerLabel, status, inboundPreview string,
	inboundAt time.Time,
	reply, failure string,
	usage bus.TurnUsage,
	maxChars int,
) string {
	if maxChars <= 0 {
		maxChars = 400
	}
	var out strings.Builder
	out.WriteString("本轮唤醒小结（")
	out.WriteString(at.Format(noticeTimeLayout))
	out.WriteString("）\n\n")
	out.WriteString("新消息 1 条\n")

	when := at
	if !inboundAt.IsZero() {
		when = inboundAt
	}
	label := firstNonEmpty(peerLabel, "unknown")
	// "法龙（fa）16:19:57" reads best without a space; a bare slug needs one.
	sep := " "
	if strings.HasSuffix(label, "）") || strings.HasSuffix(label, ")") {
		sep = ""
	}
	out.WriteString("1、")
	out.WriteString(label)
	out.WriteString(sep)
	out.WriteString(when.Format("15:04:05"))
	out.WriteString("：\n")

	out.WriteString("- ta说：\n")
	out.WriteString(quoteBlock(inboundPreview, maxChars))

	out.WriteString("- 我说：\n")
	trimmedReply := strings.TrimSpace(reply)
	if trimmedReply == "" || trimmedReply == noReplySentinel {
		out.WriteString("    （无）\n")
	} else {
		out.WriteString(quoteBlock(trimmedReply, maxChars))
	}

	if strings.TrimSpace(status) != "" && status != "completed" {
		out.WriteString("- 结果：\n")
		out.WriteString("    " + strings.TrimSpace(status))
		if f := strings.TrimSpace(failure); f != "" {
			out.WriteString("：" + previewText(f, maxChars))
		}
		out.WriteString("\n")
	}

	out.WriteString("\n")
	out.WriteString(formatTokenSummary(usage))
	return strings.TrimSpace(out.String())
}

// quoteBlock indents each line of a message body by four spaces so Telegram
// renders it as a readable quote under "- ta说：" / "- 我说：".
func quoteBlock(text string, maxChars int) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return "    （无）\n"
	}
	text = previewText(text, maxChars)
	var out strings.Builder
	for _, line := range strings.Split(text, "\n") {
		out.WriteString("    ")
		out.WriteString(strings.TrimRight(line, " \t"))
		out.WriteString("\n")
	}
	return out.String()
}

// formatTokenSummary renders the bottom statistics line.
func formatTokenSummary(usage bus.TurnUsage) string {
	detail := fmt.Sprintf("%d 次调用，%d 轮迭代", usage.LLMCalls, usage.Iterations)
	if usage.MaxIterations > 0 && usage.Iterations >= usage.MaxIterations {
		detail = fmt.Sprintf("%d 次调用，%d 轮迭代，已达上限 %d",
			usage.LLMCalls, usage.Iterations, usage.MaxIterations)
	}
	return fmt.Sprintf("📊 Token：入 %s / 出 %s / 合计 %s（%s）",
		formatThousands(usage.TokensIn),
		formatThousands(usage.TokensOut),
		formatThousands(usage.TotalTokens()),
		detail,
	)
}

// formatThousands groups digits with commas ("63195" -> "63,195").
func formatThousands(n int) string {
	if n < 0 {
		return "-" + formatThousands(-n)
	}
	digits := strconv.Itoa(n)
	if len(digits) <= 3 {
		return digits
	}
	var out strings.Builder
	pre := len(digits) % 3
	if pre > 0 {
		out.WriteString(digits[:pre])
		if len(digits) > pre {
			out.WriteByte(',')
		}
	}
	for i := pre; i < len(digits); i += 3 {
		out.WriteString(digits[i : i+3])
		if i+3 < len(digits) {
			out.WriteByte(',')
		}
	}
	return out.String()
}
