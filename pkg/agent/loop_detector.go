// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/utils"
)

type loopDecisionKind int

const (
	loopNone loopDecisionKind = iota
	loopWarn
	loopStop
)

type loopDecision struct {
	Kind   loopDecisionKind
	Reason string // identical_error | repeat | ping_pong | no_progress | critical
	Detail string
}

type loopEvent struct {
	signature  string
	tool       string
	resultHash string
	isError    bool
	errorText  string
	at         time.Time
}

// loopDetector inspects recent tool results for degenerate repetition. It is
// per-turn and not goroutine-safe (the tool loop is sequential).
type loopDetector struct {
	cfg        config.LoopDetectionConfig
	enabled    bool
	windowSize int
	startedAt  time.Time
	events     []loopEvent
}

func newLoopDetector(cfg config.LoopDetectionConfig, routing *config.RoutingConfig) *loopDetector {
	effective := cfg
	if effective.RepeatStopThreshold == 0 {
		effective.RepeatStopThreshold = effective.EffectiveRepeatStopThreshold(routing)
	}
	return &loopDetector{
		cfg:        effective,
		enabled:    cfg.IsEnabled(),
		windowSize: cfg.EffectiveWindowSize(),
		startedAt:  time.Now(),
	}
}

func toolSignature(tool string, args map[string]any) string {
	return tool + "?" + canonicalArgsFromMap(args)
}

func canonicalArgsFromMap(args map[string]any) string {
	if len(args) == 0 {
		return "{}"
	}
	return marshalSortedJSON(args)
}

func loopResultHash(content string, isError bool) string {
	// Bound the hash input; only exact repetition matters.
	if len(content) > 8192 {
		content = content[:8192]
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%t|%s", isError, content)))
	return hex.EncodeToString(sum[:8])
}

// Observe records one executed tool result and returns a decision.
func (d *loopDetector) Observe(tool string, args map[string]any, result string, isError bool) loopDecision {
	if d == nil {
		return loopDecision{}
	}
	if d.cfg.IsIgnoredTool(tool) {
		return loopDecision{}
	}
	event := loopEvent{
		signature:  toolSignature(tool, args),
		tool:       tool,
		resultHash: loopResultHash(result, isError),
		isError:    isError,
		at:         time.Now(),
	}
	if isError {
		event.errorText = utils.Truncate(strings.TrimSpace(result), 200)
	}
	d.events = append(d.events, event)
	if len(d.events) > d.windowSize && d.windowSize > 0 {
		d.events = d.events[len(d.events)-d.windowSize:]
	}

	// 1) Identical (tool, args, error) is an always-on safety guard.
	if isError {
		errorRun := d.trailingIdenticalErrorRun()
		if limit := d.cfg.EffectiveIdenticalErrorLimit(); limit > 0 && errorRun >= limit {
			return loopDecision{
				Kind:   loopStop,
				Reason: "identical_error",
				Detail: fmt.Sprintf("tool %s failed %d times in a row with the same arguments: %s",
					tool, errorRun, event.errorText),
			}
		}
	}

	if !d.enabled {
		return loopDecision{}
	}
	if minElapsed := d.cfg.MinElapsed(); minElapsed > 0 && time.Since(d.startedAt) < minElapsed {
		return loopDecision{}
	}

	// 2) Identical call signature: warn at [3,4], stop at the configured limit.
	sigRun := d.trailingSignatureRun()
	if stop := d.cfg.RepeatStopThreshold; stop > 0 && sigRun >= stop {
		return loopDecision{
			Kind:   loopStop,
			Reason: "repeat",
			Detail: fmt.Sprintf("the same tool call (%s) was repeated %d times in a row", tool, sigRun),
		}
	}
	for _, threshold := range d.cfg.EffectiveRepeatWarnThresholds() {
		if sigRun == threshold {
			return loopDecision{
				Kind:   loopWarn,
				Reason: "repeat",
				Detail: fmt.Sprintf("You have called %s with identical arguments %d times in a row. "+
					"Re-check the approach and do not repeat the same call.", tool, sigRun),
			}
		}
	}

	// 3) Ping-pong: A -> B -> A -> B ...
	if cycles := d.cfg.EffectivePingPongCycles(); cycles > 0 && d.trailingPingPongCycles() >= cycles {
		return loopDecision{
			Kind:   loopStop,
			Reason: "ping_pong",
			Detail: fmt.Sprintf("tools are alternating in a loop (%d cycles)", cycles),
		}
	}

	// 4) No progress: same tool and same result, even with different arguments.
	if limit := d.cfg.EffectiveNoProgressLimit(); limit > 0 {
		if run := d.trailingNoProgressRun(); run >= limit {
			return loopDecision{
				Kind:   loopStop,
				Reason: "no_progress",
				Detail: fmt.Sprintf("tool %s returned the same result %d times without progress", tool, run),
			}
		}
	}

	// 5) Window-wide circuit breaker (only reachable when repeat stop is off).
	if critical := d.cfg.EffectiveCriticalThreshold(); critical > 0 {
		if count := d.countSignature(event.signature); count >= critical {
			return loopDecision{
				Kind:   loopStop,
				Reason: "critical",
				Detail: fmt.Sprintf("the same tool call (%s) occurred %d times in this turn", tool, count),
			}
		}
	}
	return loopDecision{}
}

func (d *loopDetector) trailingSignatureRun() int {
	if len(d.events) == 0 {
		return 0
	}
	sig := d.events[len(d.events)-1].signature
	count := 0
	for i := len(d.events) - 1; i >= 0; i-- {
		if d.events[i].signature != sig {
			break
		}
		count++
	}
	return count
}

func (d *loopDetector) trailingIdenticalErrorRun() int {
	if len(d.events) == 0 {
		return 0
	}
	last := d.events[len(d.events)-1]
	if !last.isError {
		return 0
	}
	count := 0
	for i := len(d.events) - 1; i >= 0; i-- {
		ev := d.events[i]
		if !ev.isError || ev.signature != last.signature || ev.resultHash != last.resultHash {
			break
		}
		count++
	}
	return count
}

func (d *loopDetector) trailingNoProgressRun() int {
	if len(d.events) < 2 {
		return 0
	}
	last := d.events[len(d.events)-1]
	prev := d.events[len(d.events)-2]
	// Identical calls are handled by the exact-repeat guard; no-progress is
	// specifically "same tool/result with changing arguments".
	if last.signature == prev.signature {
		return 0
	}
	count := 0
	for i := len(d.events) - 1; i >= 0; i-- {
		ev := d.events[i]
		if ev.tool != last.tool || ev.resultHash != last.resultHash {
			break
		}
		count++
	}
	return count
}

func (d *loopDetector) trailingPingPongCycles() int {
	n := len(d.events)
	if n < 4 {
		return 0
	}
	a := d.events[n-1].signature
	b := d.events[n-2].signature
	if a == b {
		return 0
	}
	// Count alternating pairs backwards from the end.
	count := 0
	for i := n - 1; i >= 1; i -= 2 {
		if d.events[i].signature != a || d.events[i-1].signature != b {
			break
		}
		count++
	}
	// A full cycle is A,B; require at least 2 repetitions.
	return count
}

func (d *loopDetector) countSignature(sig string) int {
	count := 0
	for _, ev := range d.events {
		if ev.signature == sig {
			count++
		}
	}
	return count
}

// applyToolLoopGuard records a tool result, appends a warning to the result
// text when needed, and marks the execution for a loop stop.
func applyToolLoopGuard(
	ts *turnState,
	exec *turnExecution,
	tool string,
	args map[string]any,
	content string,
	isError bool,
) string {
	if exec == nil || exec.loopDetector == nil {
		return content
	}
	exec.lastToolName = tool
	exec.lastToolResult = truncateText(content, maxSummaryOutput)
	if ts != nil {
		ts.setLastTool(tool)
	}
	decision := exec.loopDetector.Observe(tool, args, content, isError)
	switch decision.Kind {
	case loopWarn:
		return content + "\n\n[Loop Detection] " + decision.Detail
	case loopStop:
		exec.loopStopReason = decision.Reason
		exec.loopStopDetail = decision.Detail
		exec.allResponsesHandled = false
	}
	return content
}
