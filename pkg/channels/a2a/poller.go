// PicoClaw - Ultra-lightweight personal AI agent

package a2a

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/a2a/client"
	"github.com/sipeed/picoclaw/pkg/a2a/wire"
	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/logger"
)

func (c *Channel) pollLoop() {
	defer c.wg.Done()
	failStreak := 0
	for {
		if c.ctx.Err() != nil {
			return
		}
		err := c.pollOnce(c.ctx)
		delay := c.pollInterval
		if err != nil {
			failStreak++
			c.state.SetFail(failStreak)
			delay = c.backoffDelay(failStreak, err)
			logger.WarnCF("a2a", "poll failed", map[string]any{
				"agent":       c.agentSlug,
				"fail_streak": failStreak,
				"retry_in":    delay.String(),
				"error":       err.Error(),
			})
		} else {
			failStreak = 0
			c.state.SetOK(time.Now(), 0)
		}
		if err := c.state.Save(); err != nil {
			logger.WarnCF("a2a", "failed to save channel state", map[string]any{
				"agent": c.agentSlug,
				"error": err.Error(),
			})
		}
		if err := sleepCtx(c.ctx, delay); err != nil {
			return
		}
	}
}

func (c *Channel) pollOnce(ctx context.Context) error {
	now := c.currentTime()
	etag := c.state.ETag()
	// While a turn result is pending (or in dry_run) a 304 would hide the
	// unread message, so force a full view.
	if c.dryRun || c.state.HasPendingInbound() {
		etag = ""
	}

	home, newETag, notModified, err := c.client.Home(ctx, etag)
	if err != nil {
		return err
	}
	c.state.SetTick(now)
	c.refreshPeersIfDue(ctx)

	if notModified {
		logger.InfoCF("a2a", "tick.304", map[string]any{
			"agent": c.agentSlug,
			"etag":  etag,
		})
		return nil
	}

	conversations := append([]wire.Conversation(nil), home.Conversations...)
	sort.Slice(conversations, func(i, j int) bool {
		return conversations[i].UpdatedAt < conversations[j].UpdatedAt
	})

	delivered := false
	cleared := false
	attributedUnread := 0
	for _, conv := range conversations {
		if conv.UnreadCount <= 0 {
			continue
		}
		attributedUnread += conv.UnreadCount

		detail, err := c.client.Conversation(ctx, conv.ID, true)
		if err != nil {
			var apiErr *client.APIError
			if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
				logger.WarnCF("a2a", "conversation disappeared", map[string]any{
					"agent":        c.agentSlug,
					"conversation": conv.ID,
				})
				continue
			}
			return fmt.Errorf("fetch conversation %s: %w", conv.ID, err)
		}

		if c.dryRun {
			for _, msg := range detail.Messages {
				if c.isInboundForSelf(msg) {
					c.logDryRunOnce(conv.ID, msg)
				}
			}
			continue
		}

		c.state.ExpirePendingInbound(conv.ID, now, c.inboundTimeout)

		msg, ok := c.pickDeliverable(conv.ID, detail.Messages, now)
		if !ok {
			if c.clearUnreadIfSettled(ctx, conv.ID) {
				cleared = true
			}
			continue
		}

		peer := c.resolvePeer(detail, msg)
		if peer == "" {
			logger.WarnCF("a2a", "inbound rejected: cannot resolve peer", map[string]any{
				"agent":        c.agentSlug,
				"conversation": conv.ID,
				"message":      msg.ID,
			})
			c.state.MarkSeen(conv.ID, msg.ID)
			c.state.MarkInboundDead(conv.ID, msg.ID, now, "cannot resolve peer")
			c.notifyOwnerForInbound(conv.ID, "", msg.ID, previewText(msg.BodyText(), 200), "", bus.InboundResult{
				Status: "dead", Error: "cannot resolve peer",
			}, "dead")
			if c.clearUnreadIfSettled(ctx, conv.ID) {
				cleared = true
			}
			continue
		}
		if !c.IsAllowed(peer) {
			logger.WarnCF("a2a", "inbound rejected by allow_from", map[string]any{
				"agent":   c.agentSlug,
				"peer":    peer,
				"message": msg.ID,
			})
			c.state.MarkSeen(conv.ID, msg.ID)
			c.state.MarkInboundDead(conv.ID, msg.ID, now, "sender not in allow_from")
			c.notifyOwnerForInbound(conv.ID, peer, msg.ID, previewText(msg.BodyText(), 200), "", bus.InboundResult{
				Status: "dead", Error: "sender not in allow_from",
			}, "dead")
			if c.clearUnreadIfSettled(ctx, conv.ID) {
				cleared = true
			}
			continue
		}
		if c.requireSign && !strings.EqualFold(strings.TrimSpace(msg.SigState), "ok") {
			logger.WarnCF("a2a", "inbound rejected by signature", map[string]any{
				"agent":     c.agentSlug,
				"peer":      peer,
				"message":   msg.ID,
				"sig_state": msg.SigState,
			})
			c.state.MarkSeen(conv.ID, msg.ID)
			c.state.MarkInboundDead(conv.ID, msg.ID, now, "signature state: "+msg.SigState)
			c.notifyOwnerForInbound(conv.ID, peer, msg.ID, previewText(msg.BodyText(), 200), "", bus.InboundResult{
				Status: "dead", Error: "signature state: " + msg.SigState,
			}, "dead")
			if c.clearUnreadIfSettled(ctx, conv.ID) {
				cleared = true
			}
			continue
		}

		attempt := c.state.MarkInboundPending(conv.ID, msg.ID, now, msg.BodyText())
		if saveErr := c.state.Save(); saveErr != nil {
			logger.WarnCF("a2a", "failed to persist inbound pending state", map[string]any{
				"agent": c.agentSlug,
				"error": saveErr.Error(),
			})
		}
		if err := c.deliverInbound(ctx, peer, conv.ID, msg, attempt); err != nil {
			c.state.MarkInboundFailed(conv.ID, msg.ID, now, c.inboundRetry, err.Error())
			if saveErr := c.state.Save(); saveErr != nil {
				logger.WarnCF("a2a", "failed to save inbound retry state", map[string]any{
					"agent": c.agentSlug,
					"error": saveErr.Error(),
				})
			}
			return err
		}
		delivered = true
		// Do not clear unread or commit the ETag until the turn completes.
	}

	if home.UnreadTotal > attributedUnread {
		logger.WarnCF("a2a", "unattributed unread messages", map[string]any{
			"agent":      c.agentSlug,
			"unread":     home.UnreadTotal,
			"attributed": attributedUnread,
		})
	}

	if !c.dryRun && !delivered && !cleared && attributedUnread == 0 &&
		home.UnreadTotal == 0 && !c.state.HasPendingInbound() {
		c.state.SetETag(newETag)
	}

	if delivered && c.cooldown > 0 {
		if err := sleepCtx(ctx, c.cooldown); err != nil {
			return err
		}
	}
	if delivered {
		logger.InfoCF("a2a", "tick.signal", map[string]any{
			"agent":     c.agentSlug,
			"delivered": 1,
			"unread":    home.UnreadTotal,
		})
	}
	return nil
}

// pickDeliverable selects at most one message to hand to the agent. Only one
// message per conversation may be in flight, which keeps the turn-result
// callback unambiguous.
func (c *Channel) pickDeliverable(convID string, messages []wire.Message, now time.Time) (wire.Message, bool) {
	for _, msg := range messages {
		if !c.isInboundForSelf(msg) {
			continue
		}
		if strings.TrimSpace(msg.BodyText()) == "" {
			// Nothing to process; mark it so it cannot pin the conversation
			// unread forever.
			c.state.MarkSeen(convID, msg.ID)
			continue
		}
		if c.state.Seen(convID, msg.ID) {
			continue
		}
		entry := c.state.InboundEntry(convID, msg.ID)
		if msg.Read && entry == nil {
			// After a state rebuild/upgrade the platform may still expose old
			// messages in a full poll. A read message with no pending/retry
			// state has already been handled; replaying it would create a
			// duplicate turn. Mark it seen so it cannot keep pinning state.
			c.state.MarkSeen(convID, msg.ID)
			continue
		}
		if entry != nil {
			switch entry.Status {
			case "dead":
				continue
			case "pending":
				// An in-flight message; the timeout sweep will turn it into a
				// retryable failure if the callback never arrives.
				continue
			case "failed":
				if entry.Attempts >= c.inboundMaxAttempts {
					c.state.MarkSeen(convID, msg.ID)
					c.state.MarkInboundDead(convID, msg.ID, now, entry.LastError)
					continue
				}
				if entry.NextAttempt > now.Unix() {
					continue
				}
				if c.state.HasOtherPendingInbound(convID, msg.ID) {
					continue
				}
				return msg, true
			}
		}
		if c.state.PendingInboundForConv(convID) {
			continue
		}
		return msg, true
	}
	return wire.Message{}, false
}

func (c *Channel) resolvePeer(detail *wire.ConversationDetail, msg wire.Message) string {
	peer := normalizePeer(detail.Conversation.WithAgent.Slug)
	if peer == "" {
		peer = normalizePeer(msg.From)
	}
	if peer == "" {
		peer = normalizePeer(msg.FromAgent)
	}
	return peer
}

func (c *Channel) isInboundForSelf(msg wire.Message) bool {
	from := normalizePeer(firstNonEmpty(msg.From, msg.FromAgent))
	to := normalizePeer(firstNonEmpty(msg.To, msg.ToAgent))
	if from == "" || from == c.agentSlug {
		return false
	}
	return to == "" || to == c.agentSlug
}

func (c *Channel) deliverInbound(ctx context.Context, peer, convID string, msg wire.Message, attempt int) error {
	inbound := bus.InboundContext{
		Channel:   "a2a",
		ChatID:    peer,
		ChatType:  "direct",
		SenderID:  peer,
		MessageID: msg.ID,
	}
	if inbound.Raw == nil {
		inbound.Raw = map[string]string{}
	}
	inbound.Raw[notifyConversationKey] = convID
	inbound.Raw[notifyInboundKey] = previewText(msg.BodyText(), 200)
	sender := bus.SenderInfo{
		Platform:    "a2a",
		PlatformID:  peer,
		CanonicalID: "a2a:" + peer,
		Username:    peer,
		DisplayName: peer,
	}
	completion := func(result bus.InboundResult) {
		c.handleInboundResult(convID, peer, msg.ID, attempt, result)
	}
	content := c.applyReplyPolicy(msg.BodyText())
	if err := c.HandleMessageWithCompletion(ctx, peer, content, nil, inbound, completion, sender); err != nil {
		return fmt.Errorf("deliver message %s from %s: %w", msg.ID, peer, err)
	}
	logger.InfoCF("a2a", "inbound.delivered", map[string]any{
		"agent":        c.agentSlug,
		"peer":         peer,
		"conversation": convID,
		"message":      msg.ID,
		"attempt":      attempt,
		"signed":       msg.Signed,
		"sig_state":    msg.SigState,
		"policy_chars": len([]rune(strings.TrimSpace(c.replyPolicy))),
	})
	return nil
}

func (c *Channel) handleInboundResult(convID, peer, msgID string, attempt int, result bus.InboundResult) {
	now := c.currentTime()
	entry := c.state.InboundEntry(convID, msgID)
	inboundPreview := ""
	if entry != nil {
		inboundPreview = entry.BodyPreview
	}
	status, applied := c.state.ApplyInboundResult(
		convID,
		msgID,
		attempt,
		now,
		strings.TrimSpace(result.Status),
		result.Error,
		c.inboundRetry,
		c.inboundMaxAttempts,
	)
	if !applied {
		// Stale callback (e.g. after retry/restart) — ignore.
		return
	}
	logger.InfoCF("a2a", "inbound turn result", map[string]any{
		"agent":        c.agentSlug,
		"conversation": convID,
		"message":      msgID,
		"attempt":      attempt,
		"status":       status,
		"error":        result.Error,
	})
	if err := c.state.Save(); err != nil {
		logger.WarnCF("a2a", "failed to save inbound result", map[string]any{
			"agent": c.agentSlug,
			"error": err.Error(),
		})
	}
	c.notifyOwnerForInbound(convID, peer, msgID, inboundPreview, inboundReply(result.Reply, c.isAckOrClosure), result, status)
	// On completed, leave explicit-send markers for the final reply's Send
	// call to consume (Send may be dispatched asynchronously). On failure the
	// error outbound is filtered, so clear them here to avoid stale suppression.
	if status != "completed" {
		if result.SessionKey != "" {
			wire.ClearExplicitSendsForSession(result.SessionKey)
		} else {
			wire.ClearExplicitSendsForPeer(peer)
		}
	}
	if status == "completed" || status == "dead" {
		timeout := c.httpTimeout
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		c.clearUnreadIfSettled(ctx, convID)
	}
}

// clearUnreadIfSettled clears the conversation only when every unread message
// has already been completed or dead-lettered. New messages that arrived while
// the turn was running are left unread for the next poll.
func (c *Channel) clearUnreadIfSettled(ctx context.Context, convID string) bool {
	detail, err := c.client.Conversation(ctx, convID, true)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			logger.WarnCF("a2a", "cannot inspect unread state", map[string]any{
				"agent":        c.agentSlug,
				"conversation": convID,
				"error":        err.Error(),
			})
		}
		return false
	}
	for _, msg := range detail.Messages {
		if !c.isInboundForSelf(msg) || msg.Read {
			continue
		}
		if !c.state.Seen(convID, msg.ID) {
			return false
		}
	}
	if _, err := c.client.Conversation(ctx, convID, false); err != nil {
		logger.WarnCF("a2a", "failed to clear unread", map[string]any{
			"agent":        c.agentSlug,
			"conversation": convID,
			"error":        err.Error(),
		})
		return false
	}
	logger.InfoCF("a2a", "inbound cleared", map[string]any{
		"agent":        c.agentSlug,
		"conversation": convID,
	})
	return true
}

func (c *Channel) logDryRunOnce(convID string, msg wire.Message) {
	key := convID + "|" + msg.ID
	c.drySeenMu.Lock()
	if _, ok := c.drySeen[key]; ok {
		c.drySeenMu.Unlock()
		return
	}
	c.drySeen[key] = struct{}{}
	c.drySeenMu.Unlock()
	logger.InfoCF("a2a", "inbound (dry_run, not delivered)", map[string]any{
		"agent":        c.agentSlug,
		"peer":         normalizePeer(msg.From),
		"conversation": convID,
		"message":      msg.ID,
		"chars":        len([]rune(msg.BodyText())),
		"signed":       msg.Signed,
		"sig_state":    msg.SigState,
	})
}

func (c *Channel) refreshPeersIfDue(ctx context.Context) {
	if time.Since(c.peersAt) < c.peersRefresh {
		return
	}
	agents, err := c.client.Agents(ctx)
	if err != nil {
		logger.WarnCF("a2a", "peer refresh failed", map[string]any{
			"agent": c.agentSlug,
			"error": err.Error(),
		})
	} else {
		peers := make(map[string]wire.Agent, len(agents))
		for _, a := range agents {
			if a.Slug != "" {
				peers[a.Slug] = a
			}
		}
		c.peersMu.Lock()
		c.peers = peers
		c.peersAt = time.Now()
		c.peersMu.Unlock()
		logger.InfoCF("a2a", "peer list refreshed", map[string]any{
			"agent": c.agentSlug,
			"peers": len(peers),
		})
	}
	// Piggyback clock skew detection on the same cadence.
	if err := c.ensureClock(ctx, false); err != nil {
		logger.WarnCF("a2a", "clock check failed", map[string]any{
			"agent": c.agentSlug,
			"error": err.Error(),
		})
	}
}

func (c *Channel) backoffDelay(failStreak int, err error) time.Duration {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Status {
		case http.StatusUnauthorized, http.StatusForbidden:
			return authBackoff
		case http.StatusTooManyRequests:
			// Retry-After is not exposed on APIError; fall through to the
			// exponential delay with a floor of the normal interval.
		}
	}
	exponent := failStreak - 1
	if exponent < 0 {
		exponent = 0
	}
	if exponent > 4 {
		exponent = 4
	}
	delay := c.pollInterval * time.Duration(1<<exponent)
	if delay < c.pollInterval {
		delay = c.pollInterval
	}
	if delay > 5*time.Minute {
		delay = 5 * time.Minute
	}
	return delay
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
