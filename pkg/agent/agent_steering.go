// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"context"
	"errors"
	"sync"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/logger"
)

func (al *AgentLoop) processMessageSync(ctx context.Context, msg bus.InboundMessage) {
	if al.channelManager != nil {
		defer al.channelManager.InvokeTypingStop(msg.Channel, msg.ChatID)
	}

	response, err := al.processMessage(ctx, msg)
	al.publishResponseOrError(ctx, msg.Channel, msg.ChatID, msg.SessionKey, response, err)
}

// completionGate defers the inbound completion callback until the final
// response has been published, while still making it exactly-once.
type completionGate struct {
	completion func(bus.InboundResult)
	recordOnce sync.Once
	fireOnce   sync.Once
	mu         sync.Mutex
	result     bus.InboundResult
}

func newCompletionGate(completion func(bus.InboundResult)) *completionGate {
	return &completionGate{completion: completion}
}

func (g *completionGate) Record(result bus.InboundResult) {
	if g == nil {
		return
	}
	g.recordOnce.Do(func() {
		g.mu.Lock()
		g.result = result
		g.mu.Unlock()
	})
}

func (g *completionGate) SetReply(reply string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.result.Reply = reply
	g.mu.Unlock()
}

func (g *completionGate) Fire() {
	if g == nil || g.completion == nil {
		return
	}
	g.fireOnce.Do(func() {
		g.recordOnce.Do(func() {
			g.mu.Lock()
			g.result = bus.InboundResult{
				Status: bus.InboundResultError,
				Error:  "turn ended without a completion result",
			}
			g.mu.Unlock()
		})
		g.mu.Lock()
		result := g.result
		g.mu.Unlock()
		g.completion(result)
	})
}

func (al *AgentLoop) runTurnWithSteering(ctx context.Context, initialMsg bus.InboundMessage) {
	// Hold the completion callback until after the final response is published.
	var gate *completionGate
	if initialMsg.Completion != nil {
		gate = newCompletionGate(initialMsg.Completion)
		initialMsg.Completion = gate.Record
		defer gate.Fire()
	}

	// Process the initial message
	response, err := al.processMessage(ctx, initialMsg)
	if err != nil {
		if initialMsg.Completion != nil {
			status := bus.InboundResultError
			if errors.Is(err, context.Canceled) {
				status = bus.InboundResultAborted
			}
			initialMsg.Completion(bus.InboundResult{
				Status:     status,
				Error:      err.Error(),
				SessionKey: initialMsg.SessionKey,
			})
		}
		if !al.maybePublishError(ctx, initialMsg.Channel, initialMsg.ChatID, initialMsg.SessionKey, err) {
			return // context canceled
		}
		response = ""
	}
	finalResponse := response

	// Build continuation target
	target, targetErr := al.buildContinuationTarget(initialMsg)
	if targetErr != nil {
		logger.WarnCF("agent", "Failed to build steering continuation target",
			map[string]any{
				"channel": initialMsg.Channel,
				"error":   targetErr.Error(),
			})
		return
	}
	if target == nil {
		// System message or non-routable, response already published
		return
	}

	continued, continueErr := al.drainQueuedSteeringContinuations(ctx, target)
	if continueErr != nil {
		logger.WarnCF("agent", "Failed to continue queued steering",
			map[string]any{
				"channel": target.Channel,
				"chat_id": target.ChatID,
				"error":   continueErr.Error(),
			})
	} else if continued != "" {
		finalResponse = continued
	}

	// Publish final response
	if gate != nil {
		gate.SetReply(finalResponse)
	}
	if finalResponse != "" {
		if initialMsg.Channel == "a2a" {
			al.PublishInboundResponseIfNeeded(ctx, target.Channel, target.ChatID, target.SessionKey, finalResponse, initialMsg.Context)
		} else {
			al.PublishResponseIfNeeded(ctx, target.Channel, target.ChatID, target.SessionKey, finalResponse)
		}
	}
}

func (al *AgentLoop) drainQueuedSteeringContinuations(
	ctx context.Context,
	target *continuationTarget,
) (string, error) {
	if target == nil {
		return "", nil
	}

	finalResponse := ""
	for al.pendingSteeringCountForScope(target.SessionKey) > 0 {
		if err := ctx.Err(); err != nil {
			return finalResponse, err
		}

		logger.InfoCF("agent", "Continuing queued steering after turn end",
			map[string]any{
				"channel":     target.Channel,
				"chat_id":     target.ChatID,
				"session_key": target.SessionKey,
				"queue_depth": al.pendingSteeringCountForScope(target.SessionKey),
			})

		continued, continueErr := al.Continue(ctx, target.SessionKey, target.Channel, target.ChatID)
		if continueErr != nil {
			return finalResponse, continueErr
		}
		if continued == "" {
			break
		}
		finalResponse = continued
	}

	return finalResponse, nil
}

func (al *AgentLoop) resolveSteeringTarget(msg bus.InboundMessage) (string, string, bool) {
	if msg.Channel == "system" {
		return "", "", false
	}

	route, agent, err := al.resolveMessageRoute(msg)
	if err != nil || agent == nil {
		return "", "", false
	}
	allocation := al.allocateRouteSession(route, msg)

	return resolveScopeKey(allocation.SessionKey, msg.SessionKey), agent.ID, true
}
