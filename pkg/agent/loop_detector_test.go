// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
)

func observeN(d *loopDetector, tool string, args map[string]any, result string, isError bool, n int) loopDecision {
	var decision loopDecision
	for i := 0; i < n; i++ {
		decision = d.Observe(tool, args, result, isError)
	}
	return decision
}

func TestLoopDetectorIdenticalErrorStopsAtThree(t *testing.T) {
	d := newLoopDetector(config.LoopDetectionConfig{}, nil)
	if dec := observeN(d, "exec", map[string]any{"cmd": "x"}, "permission denied", true, 2); dec.Kind != loopNone {
		t.Fatalf("2 identical errors = %+v, want none", dec)
	}
	dec := d.Observe("exec", map[string]any{"cmd": "x"}, "permission denied", true)
	if dec.Kind != loopStop || dec.Reason != "identical_error" {
		t.Fatalf("3rd identical error = %+v, want stop/identical_error", dec)
	}
}

func TestLoopDetectorIdenticalCallsWarnThenStop(t *testing.T) {
	d := newLoopDetector(config.LoopDetectionConfig{}, nil)
	args := map[string]any{"cmd": "ls"}
	if dec := observeN(d, "exec", args, "same", false, 2); dec.Kind != loopNone {
		t.Fatalf("2 repeats = %+v, want none", dec)
	}
	if dec := d.Observe("exec", args, "same", false); dec.Kind != loopWarn {
		t.Fatalf("3rd repeat = %+v, want warn", dec)
	}
	if dec := d.Observe("exec", args, "same", false); dec.Kind != loopWarn {
		t.Fatalf("4th repeat = %+v, want warn", dec)
	}
	if dec := d.Observe("exec", args, "same", false); dec.Kind != loopStop || dec.Reason != "repeat" {
		t.Fatalf("5th repeat = %+v, want stop/repeat", dec)
	}
}

func TestLoopDetectorResetsOnDifferentArgs(t *testing.T) {
	d := newLoopDetector(config.LoopDetectionConfig{}, nil)
	observeN(d, "exec", map[string]any{"cmd": "a"}, "x", false, 3)
	if dec := d.Observe("exec", map[string]any{"cmd": "b"}, "x", false); dec.Kind != loopNone {
		t.Fatalf("different args after a run = %+v, want none", dec)
	}
}

func TestLoopDetectorNoProgressStopsAtFive(t *testing.T) {
	d := newLoopDetector(config.LoopDetectionConfig{}, nil)
	for i := 0; i < 4; i++ {
		if dec := d.Observe("web_search", map[string]any{"q": string(rune('a' + i))}, "no results", false); dec.Kind != loopNone {
			t.Fatalf("no-progress %d = %+v, want none", i+1, dec)
		}
	}
	dec := d.Observe("web_search", map[string]any{"q": "e"}, "no results", false)
	if dec.Kind != loopStop || dec.Reason != "no_progress" {
		t.Fatalf("5th no-progress = %+v, want stop/no_progress", dec)
	}
}

func TestLoopDetectorPingPongStopsAtFourCycles(t *testing.T) {
	d := newLoopDetector(config.LoopDetectionConfig{}, nil)
	argsA := map[string]any{"q": "a"}
	argsB := map[string]any{"q": "b"}
	for i := 0; i < 3; i++ {
		_ = d.Observe("a", argsA, "ra", false)
		_ = d.Observe("b", argsB, "rb", false)
	}
	_ = d.Observe("a", argsA, "ra", false)
	dec := d.Observe("b", argsB, "rb", false)
	if dec.Kind != loopStop || dec.Reason != "ping_pong" {
		t.Fatalf("after 4 ping-pong cycles = %+v, want stop/ping_pong", dec)
	}
}

func TestLoopDetectorIgnoreTools(t *testing.T) {
	enabled := true
	d := newLoopDetector(config.LoopDetectionConfig{
		Enabled:     &enabled,
		IgnoreTools: []string{"spawn_status"},
	}, nil)
	dec := observeN(d, "spawn_status", map[string]any{"id": "1"}, "running", false, 10)
	if dec.Kind != loopNone {
		t.Fatalf("ignored tool = %+v, want none", dec)
	}
}

func TestLoopDetectorDisabledKeepsErrorGuard(t *testing.T) {
	disabled := false
	d := newLoopDetector(config.LoopDetectionConfig{Enabled: &disabled}, nil)
	if dec := observeN(d, "exec", map[string]any{"cmd": "x"}, "same", false, 10); dec.Kind != loopNone {
		t.Fatalf("disabled detection still warned/stopped: %+v", dec)
	}
	dec := observeN(d, "exec", map[string]any{"cmd": "x"}, "boom", true, 3)
	if dec.Kind != loopStop || dec.Reason != "identical_error" {
		t.Fatalf("identical error must stay on when disabled: %+v", dec)
	}
}

func TestLoopDetectorRoutingFallback(t *testing.T) {
	routing := &config.RoutingConfig{EscalatePromptTokens: -1, EscalateToolIterations: -1, RepeatBreakerIterations: 7}
	d := newLoopDetector(config.LoopDetectionConfig{}, routing)
	if dec := observeN(d, "exec", map[string]any{"cmd": "x"}, "same", false, 6); dec.Kind != loopNone {
		t.Fatalf("6 repeats with threshold 7 = %+v, want none", dec)
	}
	if dec := d.Observe("exec", map[string]any{"cmd": "x"}, "same", false); dec.Kind != loopStop {
		t.Fatalf("7th repeat = %+v, want stop", dec)
	}
}
