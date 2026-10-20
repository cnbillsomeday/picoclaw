// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/logger"
)

const (
	defaultGraceTimeout = 60 * time.Second
	maxSummaryOutput    = 500
)

func stopReasonText(reason string) string {
	switch reason {
	case "budget":
		return "the turn time budget was exhausted"
	case "step_timeout":
		return "a model request timed out"
	case "max_iterations":
		return "the maximum number of tool iterations was reached"
	case "identical_error":
		return "the same tool call failed repeatedly"
	case "repeat":
		return "the same tool call was repeated too many times"
	case "no_progress":
		return "tool calls stopped making progress"
	case "ping_pong":
		return "tools started alternating in a loop"
	case "critical":
		return "a tool call repeated excessively"
	default:
		return "the turn was stopped by a safety guard"
	}
}

func truncateText(s string, n int) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

func lastProgressText(exec *turnExecution) string {
	if exec == nil {
		return ""
	}
	if strings.TrimSpace(exec.lastToolResult) != "" {
		return truncateText(exec.lastToolResult, maxSummaryOutput)
	}
	return truncateText(exec.lastLLMContent, maxSummaryOutput)
}

func buildStopSummary(reason string, elapsed time.Duration, iterations int, lastTool, lastOutput string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Turn stopped after %s (%d step(s)): %s.",
		elapsed.Round(time.Second), iterations, stopReasonText(reason))
	if strings.TrimSpace(lastTool) != "" {
		fmt.Fprintf(&b, "\nLast tool: %s.", lastTool)
	}
	if strings.TrimSpace(lastOutput) != "" {
		fmt.Fprintf(&b, "\nLast output: %s", truncateText(lastOutput, maxSummaryOutput))
	}
	b.WriteString("\nThe task was not completed. Split it into smaller steps or adjust the request.")
	return b.String()
}

// SoftLanding runs the one-shot, tool-less final summary call and returns the
// text to deliver. It never returns an empty string.
func (p *Pipeline) SoftLanding(
	ctx context.Context,
	turnCtx context.Context,
	ts *turnState,
	exec *turnExecution,
	reason string,
) string {
	started := time.Now()
	output := ""
	if p.Cfg.Agents.Defaults.GraceCallEnabled() && ts.tryUseGraceCall() {
		exec.toolsDisabled = true
		exec.finalInstruction = "The turn has stopped early. Do not call any tools. " +
			"Briefly summarize what was completed, what failed or is still missing, and suggest a smaller next step."
		timeout := p.Cfg.Agents.Defaults.GetStepTimeout()
		if timeout <= 0 {
			timeout = defaultGraceTimeout
		}
		// turnCtx is cancel-only (no turn deadline), so this fresh timeout lets
		// the final call run after the budget expired while still honoring /stop.
		finalCtx, cancel := context.WithTimeout(turnCtx, timeout)
		defer cancel()

		exec.response = nil
		exec.finalContent = ""
		ctrl, err := p.CallLLM(ctx, finalCtx, ts, exec, ts.currentIteration()+1)
		output = strings.TrimSpace(exec.finalContent)
		if output == "" && exec.response != nil {
			output = strings.TrimSpace(exec.response.Content)
		}
		logger.InfoCF("agent", "soft landing final call", map[string]any{
			"agent_id":   ts.agent.ID,
			"reason":     reason,
			"control":    int(ctrl),
			"error":      errString(err),
			"output_len": len(output),
			"duration":   time.Since(started).Round(time.Millisecond).String(),
		})
	}
	exec.toolsDisabled = false
	exec.finalInstruction = ""

	if strings.TrimSpace(output) != "" {
		return output
	}
	return buildStopSummary(
		reason,
		time.Since(ts.startedAt),
		ts.currentIteration(),
		exec.lastToolName,
		lastProgressText(exec),
	)
}

// BuildLoopStopMessage builds the deterministic message for a loop-detector stop.
func BuildLoopStopMessage(ts *turnState, exec *turnExecution) string {
	detail := strings.TrimSpace(exec.loopStopDetail)
	summary := buildStopSummary(
		exec.loopStopReason,
		time.Since(ts.startedAt),
		ts.currentIteration(),
		exec.lastToolName,
		lastProgressText(exec),
	)
	if detail != "" {
		return fmt.Sprintf("Turn stopped: %s.\n%s", detail, summary)
	}
	return summary
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
