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
		_ = c.state.Save()
		if err := sleepCtx(c.ctx, delay); err != nil {
			return
		}
	}
}

func (c *Channel) pollOnce(ctx context.Context) error {
	now := time.Now()
	home, etag, notModified, err := c.client.Home(ctx, c.state.ETag())
	if err != nil {
		return err
	}
	c.state.SetTick(now)
	c.state.SetETag(etag)
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

	delivered := 0
	for _, conv := range conversations {
		if conv.UnreadCount <= 0 {
			continue
		}
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
		peer := strings.TrimSpace(detail.Conversation.WithAgent.Slug)
		if peer == "" {
			peer = strings.TrimSpace(conv.WithAgent.Slug)
		}

		convDelivered := 0
		for _, msg := range detail.Messages {
			if msg.From == c.agentSlug {
				continue
			}
			if msg.To != "" && msg.To != c.agentSlug {
				continue
			}
			body := msg.BodyText()
			if strings.TrimSpace(body) == "" {
				continue
			}
			if c.state.Seen(conv.ID, msg.ID) {
				continue
			}
			if c.dryRun {
				c.logDryRunOnce(conv.ID, msg)
				continue
			}
			if peer == "" {
				continue
			}
			if !c.IsAllowed(peer) {
				logger.WarnCF("a2a", "inbound rejected by allow_from", map[string]any{
					"agent":   c.agentSlug,
					"peer":    peer,
					"message": msg.ID,
				})
				c.state.MarkSeen(conv.ID, msg.ID)
				continue
			}
			if err := c.deliverInbound(ctx, peer, msg); err != nil {
				return err
			}
			c.state.MarkSeen(conv.ID, msg.ID)
			convDelivered++
			delivered++
		}

		if c.dryRun {
			continue
		}
		// Delivery succeeded (or was consciously skipped); clear unread so we
		// do not spin on the same conversation forever.
		if _, err := c.client.Conversation(ctx, conv.ID, false); err != nil {
			return fmt.Errorf("clear unread for %s: %w", conv.ID, err)
		}
		if convDelivered > 0 {
			logger.InfoCF("a2a", "inbound cleared", map[string]any{
				"agent":        c.agentSlug,
				"peer":         peer,
				"conversation": conv.ID,
				"delivered":    convDelivered,
			})
		}
	}

	if delivered > 0 && c.cooldown > 0 {
		if err := sleepCtx(ctx, c.cooldown); err != nil {
			return err
		}
	}
	if delivered > 0 {
		logger.InfoCF("a2a", "tick.signal", map[string]any{
			"agent":     c.agentSlug,
			"delivered": delivered,
			"unread":    home.UnreadTotal,
		})
	}
	return nil
}

func (c *Channel) deliverInbound(ctx context.Context, peer string, msg wire.Message) error {
	inbound := bus.InboundContext{
		Channel:   "a2a",
		ChatID:    peer,
		ChatType:  "direct",
		SenderID:  peer,
		MessageID: msg.ID,
	}
	sender := bus.SenderInfo{
		Platform:    "a2a",
		PlatformID:  peer,
		CanonicalID: "a2a:" + peer,
		Username:    peer,
		DisplayName: peer,
	}
	content := c.applyReplyPolicy(msg.BodyText())
	if err := c.HandleMessageWithContext(ctx, peer, content, nil, inbound, sender); err != nil {
		return fmt.Errorf("deliver message %s from %s: %w", msg.ID, peer, err)
	}
	logger.InfoCF("a2a", "inbound.delivered", map[string]any{
		"agent":        c.agentSlug,
		"peer":         peer,
		"conversation": msg.ConversationID,
		"message":      msg.ID,
		"signed":       msg.Signed,
		"sig_state":    msg.SigState,
		"policy_chars": len([]rune(strings.TrimSpace(c.replyPolicy))),
	})
	return nil
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
		"peer":         msg.From,
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
