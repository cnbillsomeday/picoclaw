// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// maxSteeringIterationsFactor is the multiplier applied to max_tool_iterations
// to derive the absolute iteration ceiling for the steering bypass. The turn
// loop lets pending steering messages extend a turn past max_tool_iterations;
// without a hard cap that bypass could extend a broken loop indefinitely.
const maxSteeringIterationsFactor = 3

// steeringHardCap returns the absolute iteration ceiling for steering-bypassed
// turns. A returned value of 0 means "no extra cap" (unlimited configuration).
func steeringHardCap(maxIterations int) int {
	if maxIterations <= 0 {
		return 0
	}
	capValue := maxIterations * maxSteeringIterationsFactor
	if capValue < maxIterations+20 {
		capValue = maxIterations + 20
	}
	return capValue
}

// marshalSortedJSON marshals a map[string]any with sorted keys (Go's JSON
// encoder sorts map keys deterministically).
func marshalSortedJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(encoded)
}

// ApplyRoutingEscalation re-routes the rest of a light-model turn to the
// primary pool when a response reports a ballooned prompt or the work proves
// tool-heavy. It is a no-op for primary-model turns.
func (p *Pipeline) ApplyRoutingEscalation(
	ts *turnState,
	exec *turnExecution,
	iteration int,
) {
	if ts == nil || ts.agent == nil || exec == nil || exec.response == nil {
		return
	}

	hasToolCalls := len(exec.normalizedToolCalls) > 0
	usage := exec.response.Usage
	routingCfg := p.Cfg.Agents.Defaults.Routing

	// --- Mid-turn escalation: light model → primary pool (one-shot) ---
	if !exec.usedLight || exec.routingEscalated {
		return
	}

	escalateReason := ""
	if promptLimit := routingCfg.GetEscalatePromptTokens(); promptLimit > 0 &&
		usage != nil && usage.PromptTokens >= promptLimit {
		escalateReason = "prompt_tokens"
	}
	if escalateReason == "" {
		if toolLimit := routingCfg.GetEscalateToolIterations(); toolLimit > 0 {
			if hasToolCalls {
				exec.consecutiveToolIters++
			} else {
				exec.consecutiveToolIters = 0
			}
			if exec.consecutiveToolIters >= toolLimit {
				escalateReason = "tool_intensity"
			}
		}
	}
	if escalateReason == "" {
		return
	}

	primaryModel := strings.TrimSpace(ts.agent.Model)
	var targetCandidates []providers.FallbackCandidate
	if len(ts.agent.Candidates) > 0 {
		targetCandidates = append([]providers.FallbackCandidate(nil), ts.agent.Candidates...)
	}
	if err := p.switchExecCandidates(
		ts,
		exec,
		targetCandidates,
		primaryModel,
		"escalate_"+escalateReason,
	); err != nil {
		logger.WarnCF("agent", "Routing escalation failed; staying on light model", map[string]any{
			"agent_id": ts.agent.ID,
			"reason":   escalateReason,
			"error":    err.Error(),
		})
		return
	}
	applyTurnThinkingOptions(exec, ts.agent, exec.activeProvider, true)

	logger.InfoCF("agent", "Model routing: escalated to primary mid-turn", map[string]any{
		"agent_id":      ts.agent.ID,
		"iteration":     iteration,
		"reason":        escalateReason,
		"model":         exec.activeModel,
		"model_name":    exec.llmModelName,
		"prompt_tokens": usageTokensOrZero(usage),
	})
}

func usageTokensOrZero(usage *providers.UsageInfo) int {
	if usage == nil {
		return 0
	}
	return usage.PromptTokens
}

// switchExecCandidates repoints a running turn execution at a new candidate
// pool (primary, image model, or provider-only target). It is the shared
// switch used by media-turn routing bypass and the v1.3.1 routing guards.
func (p *Pipeline) switchExecCandidates(
	ts *turnState,
	exec *turnExecution,
	targetCandidates []providers.FallbackCandidate,
	targetModelName string,
	reason string,
) error {
	if ts == nil || ts.agent == nil || exec == nil {
		return fmt.Errorf("cannot switch candidates: nil turn state")
	}
	if len(targetCandidates) == 0 && strings.TrimSpace(targetModelName) == "" {
		return fmt.Errorf("cannot switch candidates: no candidates or model name")
	}

	targetModel := resolvedCandidateModel(targetCandidates, targetModelName)
	targetProvider := exec.activeProvider
	if len(targetCandidates) > 0 {
		firstCandidate := targetCandidates[0]
		provider, err := providerForFallbackCandidate(
			ts.agent,
			ts.agent.Provider,
			ts.agent.Candidates,
			firstCandidate,
		)
		if err != nil {
			return err
		}
		if provider != nil {
			targetProvider = provider
		}
	} else if ts.agent.Provider != nil {
		targetProvider = ts.agent.Provider
	}

	resolvedModelName := resolvedCandidateModelName(targetCandidates, targetModelName)
	if sameCandidateSet(exec.activeCandidates, targetCandidates) &&
		exec.activeModel == targetModel &&
		exec.llmModelName == resolvedModelName {
		return nil
	}

	exec.activeCandidates = targetCandidates
	exec.activeModel = targetModel
	exec.activeProvider = targetProvider
	// Keep the Chat model string in lockstep: unlike the media-bypass path
	// (which runs before CallLLM's per-call llmModel refresh at the top of
	// CallLLM), guard-driven switches happen after it, so updating
	// activeModel alone would leave exec.llmModel pointing at the old model.
	exec.llmModel = targetModel
	if len(targetCandidates) > 0 {
		exec.activeModelConfig = resolveActiveModelConfig(
			p.Cfg,
			ts.agent.Workspace,
			targetCandidates,
			targetModel,
			p.Cfg.Agents.Defaults.Provider,
		)
	} else {
		exec.activeModelConfig = nil
	}
	exec.llmModelName = resolvedModelName
	exec.usedLight = false

	logger.InfoCF("agent", "Model routing switched candidates mid-turn", map[string]any{
		"agent_id":   ts.agent.ID,
		"reason":     reason,
		"model":      exec.activeModel,
		"model_name": exec.llmModelName,
		"candidates": len(exec.activeCandidates),
	})
	return nil
}
