// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	runtimeevents "github.com/sipeed/picoclaw/pkg/events"
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

// repeatBreakerResponse is the user-facing message when the repeat breaker
// stops a degenerate tool-call loop.
const repeatBreakerResponse = "Detected a repeating tool-call loop (%d identical iterations in a row) and stopped this turn before it consumed more tokens. Try rephrasing the request, or run this task on your primary model."

// toolCallSignature builds a stable, comparable signature for a turn's
// normalized tool calls so degenerate "same tool, same arguments" repetition
// can be detected across iterations.
func toolCallSignature(calls []providers.ToolCall) string {
	if len(calls) == 0 {
		return ""
	}
	parts := make([]string, 0, len(calls))
	for _, call := range calls {
		parts = append(parts, call.Name+"?"+canonicalToolArgs(call))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

// canonicalToolArgs normalizes tool-call arguments into a deterministic
// string so stylistic differences like key order or whitespace do not hide
// a repeated call. Normalized tool calls carry Arguments as a parsed map;
// the raw JSON string in Function.Arguments is the fallback.
func canonicalToolArgs(call providers.ToolCall) string {
	if len(call.Arguments) > 0 {
		return marshalSortedJSON(call.Arguments)
	}
	if call.Function != nil {
		return canonicalJSONString(call.Function.Arguments)
	}
	return ""
}

// canonicalJSONString re-encodes a JSON object string with sorted keys. Input
// that is not a JSON object is returned trimmed as-is.
func canonicalJSONString(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	var decoded any
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		return trimmed
	}
	if _, ok := decoded.(map[string]any); ok {
		return marshalSortedJSON(decoded)
	}
	return trimmed
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

func firstToolCallName(calls []providers.ToolCall) string {
	if len(calls) == 0 {
		return ""
	}
	return calls[0].Name
}

// ApplyRoutingGuards runs the v1.3.1 mid-turn routing guards after every LLM
// iteration:
//
//  1. Repeat breaker (applies to every model tier): stops the turn when the
//     model repeats the exact same tool call for RepeatBreakerIterations
//     consecutive responses — the degenerate loop that silently burns
//     millions of tokens.
//  2. Light→primary escalation: re-routes the rest of the turn to the
//     primary pool when a light-model response reports a ballooned prompt or
//     the work proves tool-heavy.
//
// Returns (breakTurn, breakerContent). When breakTurn is true the caller must
// end the turn with breakerContent as the final message.
func (p *Pipeline) ApplyRoutingGuards(
	ts *turnState,
	exec *turnExecution,
	iteration int,
) (bool, string) {
	if ts == nil || ts.agent == nil || exec == nil || exec.response == nil {
		return false, ""
	}

	hasToolCalls := len(exec.normalizedToolCalls) > 0
	usage := exec.response.Usage
	routingCfg := p.Cfg.Agents.Defaults.Routing

	// --- Repeat breaker: identical consecutive tool calls (any tier) ---
	if hasToolCalls {
		signature := toolCallSignature(exec.normalizedToolCalls)
		if signature == exec.repeatLastSignature {
			exec.repeatSignatureCount++
		} else {
			exec.repeatLastSignature = signature
			exec.repeatSignatureCount = 1
		}
		if breakerLimit := routingCfg.GetRepeatBreakerIterations(); breakerLimit > 0 &&
			exec.repeatSignatureCount >= breakerLimit {
			logger.ErrorCF("agent", "Repeat breaker: stopping degenerate tool-call loop", map[string]any{
				"agent_id":  ts.agent.ID,
				"iteration": iteration,
				"model":     exec.llmModelName,
				"repeats":   exec.repeatSignatureCount,
				"tool":      firstToolCallName(exec.normalizedToolCalls),
			})
			p.emitAgentErrorEvent(ts, "repeat_breaker", fmt.Sprintf(
				"repeat breaker after %d identical tool calls", exec.repeatSignatureCount))
			return true, fmt.Sprintf(repeatBreakerResponse, exec.repeatSignatureCount)
		}
	} else {
		exec.repeatLastSignature = ""
		exec.repeatSignatureCount = 0
	}

	// --- Mid-turn escalation: light model → primary pool (one-shot) ---
	if !exec.usedLight || exec.routingEscalated {
		return false, ""
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
		return false, ""
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
		return false, ""
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
	return false, ""
}

func usageTokensOrZero(usage *providers.UsageInfo) int {
	if usage == nil {
		return 0
	}
	return usage.PromptTokens
}

// emitAgentErrorEvent publishes a turn.error event with the given stage so
// router/monitoring surfaces repeat-breaker stoppages like any other error.
func (p *Pipeline) emitAgentErrorEvent(ts *turnState, stage, message string) {
	p.al.emitEvent(
		runtimeevents.KindAgentError,
		ts.eventMeta("ApplyRoutingGuards", "turn.error"),
		ErrorPayload{
			Stage:   stage,
			Message: message,
		},
	)
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
