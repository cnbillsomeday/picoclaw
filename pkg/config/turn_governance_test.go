// PicoClaw - Ultra-lightweight personal AI agent

package config

import (
	"encoding/json"
	"testing"
	"time"
)

func TestTurnGovernanceDefaults(t *testing.T) {
	var d AgentDefaults
	if d.GetTurnTimeBudget() != 0 {
		t.Fatalf("budget default = %s, want 0", d.GetTurnTimeBudget())
	}
	if d.GetStepTimeout() != 0 {
		t.Fatalf("step timeout default = %s, want 0", d.GetStepTimeout())
	}
	if !d.GraceCallEnabled() {
		t.Fatal("grace call must default to enabled")
	}
	if !d.LoopDetection.IsEnabled() {
		t.Fatal("loop detection must default to enabled")
	}
	if got := d.LoopDetection.EffectiveIdenticalErrorLimit(); got != 3 {
		t.Fatalf("identical error limit = %d, want 3", got)
	}
	if got := d.LoopDetection.EffectiveRepeatStopThreshold(nil); got != 5 {
		t.Fatalf("repeat stop = %d, want 5", got)
	}
	if got := d.LoopDetection.EffectiveNoProgressLimit(); got != 5 {
		t.Fatalf("no-progress = %d, want 5", got)
	}
	if got := d.LoopDetection.EffectivePingPongCycles(); got != 4 {
		t.Fatalf("ping-pong = %d, want 4", got)
	}
}

func TestTurnGovernanceOverrides(t *testing.T) {
	disabled := false
	d := AgentDefaults{
		TurnTimeBudgetSeconds: 1200,
		StepTimeoutSecs:       120,
		TurnGraceCall:         boolPtrForTest(false),
		LoopDetection: LoopDetectionConfig{
			Enabled:              &disabled,
			RepeatStopThreshold:  7,
			RepeatWarnThresholds: []int{2, 6},
			IgnoreTools:          []string{"spawn_status"},
		},
	}
	if d.GetTurnTimeBudget() != 20*time.Minute {
		t.Fatalf("budget = %s, want 20m", d.GetTurnTimeBudget())
	}
	if d.GetStepTimeout() != 2*time.Minute {
		t.Fatalf("step timeout = %s, want 2m", d.GetStepTimeout())
	}
	if d.GraceCallEnabled() {
		t.Fatal("explicit false must disable the grace call")
	}
	if d.LoopDetection.IsEnabled() {
		t.Fatal("explicit false must disable loop detection")
	}
	if got := d.LoopDetection.EffectiveRepeatStopThreshold(nil); got != 7 {
		t.Fatalf("repeat stop = %d, want 7", got)
	}
	if !d.LoopDetection.IsIgnoredTool("SPAWN_STATUS") {
		t.Fatal("ignore_tools must match case-insensitively")
	}
}

func TestTurnGovernanceRoutingFallback(t *testing.T) {
	routing := &RoutingConfig{RepeatBreakerIterations: 9}
	d := AgentDefaults{}
	if got := d.LoopDetection.EffectiveRepeatStopThreshold(routing); got != 9 {
		t.Fatalf("routing fallback = %d, want 9", got)
	}
	d.LoopDetection.RepeatStopThreshold = 4
	if got := d.LoopDetection.EffectiveRepeatStopThreshold(routing); got != 4 {
		t.Fatalf("explicit threshold = %d, want 4", got)
	}
	d.LoopDetection.RepeatStopThreshold = -1
	if got := d.LoopDetection.EffectiveRepeatStopThreshold(routing); got != 0 {
		t.Fatalf("negative threshold = %d, want 0 (disabled)", got)
	}
}

func TestLoopDetectionJSON(t *testing.T) {
	var d AgentDefaults
	if err := jsonUnmarshalForTest(`{"turn_time_budget_seconds":600,"step_timeout_secs":30,"turn_grace_call":false,"loop_detection":{"repeat_stop_threshold":8,"ignore_tools":["spawn_status"]}}`, &d); err != nil {
		t.Fatal(err)
	}
	if d.GetTurnTimeBudget() != 10*time.Minute || d.GetStepTimeout() != 30*time.Second {
		t.Fatalf("decoded budget/step = %s/%s", d.GetTurnTimeBudget(), d.GetStepTimeout())
	}
	if d.GraceCallEnabled() {
		t.Fatal("decoded grace call = true, want false")
	}
	if got := d.LoopDetection.EffectiveRepeatStopThreshold(nil); got != 8 {
		t.Fatalf("decoded repeat stop = %d, want 8", got)
	}
}

func boolPtrForTest(v bool) *bool { return &v }

func jsonUnmarshalForTest(data string, target any) error {
	return json.Unmarshal([]byte(data), target)
}
