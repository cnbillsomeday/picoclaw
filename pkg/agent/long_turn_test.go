// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	runtimeevents "github.com/sipeed/picoclaw/pkg/events"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
)

type ctxAwareSlowProvider struct {
	delay time.Duration
}

func (m *ctxAwareSlowProvider) Chat(
	ctx context.Context,
	_ []providers.Message,
	_ []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(m.delay):
		return &providers.LLMResponse{Content: "too late"}, nil
	}
}

func (m *ctxAwareSlowProvider) GetDefaultModel() string { return "slow-model" }

type deadlineErrorProvider struct {
	toolPhaseCalls  atomic.Int32
	finalPhaseCalls atomic.Int32
}

func (m *deadlineErrorProvider) Chat(
	_ context.Context,
	_ []providers.Message,
	tools []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	if len(tools) > 0 {
		m.toolPhaseCalls.Add(1)
		return nil, context.DeadlineExceeded
	}
	m.finalPhaseCalls.Add(1)
	return &providers.LLMResponse{Content: "grace summary"}, nil
}

func (m *deadlineErrorProvider) GetDefaultModel() string { return "deadline-model" }

type graceSummaryProvider struct {
	toolPhaseCalls  atomic.Int32
	finalPhaseCalls atomic.Int32
}

func (m *graceSummaryProvider) Chat(
	_ context.Context,
	_ []providers.Message,
	tools []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	if len(tools) > 0 {
		m.toolPhaseCalls.Add(1)
		return &providers.LLMResponse{
			ToolCalls: []providers.ToolCall{{
				ID:        "call_grace_tool",
				Type:      "function",
				Name:      "tool_limit_test_tool",
				Arguments: map[string]any{"value": "x"},
			}},
		}, nil
	}
	m.finalPhaseCalls.Add(1)
	return &providers.LLMResponse{Content: "grace summary"}, nil
}

func (m *graceSummaryProvider) GetDefaultModel() string { return "grace-model" }

func baseLongTurnConfig(dir string) *config.Config {
	return &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         dir,
				ModelName:         "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
			},
		},
	}
}

func TestTurnTimeBudgetSoftLands(t *testing.T) {
	cfg := baseLongTurnConfig(t.TempDir())
	cfg.Agents.Defaults.TurnTimeBudgetSeconds = 1
	cfg.Agents.Defaults.TurnGraceCall = boolPtr(false)

	provider := &ctxAwareSlowProvider{delay: 10 * time.Second}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)

	start := time.Now()
	response, err := al.ProcessDirectWithChannel(context.Background(), "hello", "budget", "test", "chat1")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("ProcessDirectWithChannel: %v", err)
	}
	if !strings.Contains(response, "time budget was exhausted") {
		t.Fatalf("response = %q, want a time-budget summary", response)
	}
	if elapsed > 6*time.Second {
		t.Fatalf("turn took %s, budget was not enforced", elapsed)
	}
}

func TestStepTimeoutSoftLandsViaGrace(t *testing.T) {
	cfg := baseLongTurnConfig(t.TempDir())
	cfg.Agents.Defaults.StepTimeoutSecs = 1
	cfg.Agents.Defaults.MaxLLMRetries = 1
	cfg.Agents.Defaults.LLMRetryBackoffSecs = 1
	// TurnGraceCall defaults to true.

	provider := &deadlineErrorProvider{}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)

	response, err := al.ProcessDirectWithChannel(context.Background(), "hello", "step-timeout", "test", "chat1")
	if err != nil {
		t.Fatalf("ProcessDirectWithChannel: %v", err)
	}
	if response != "grace summary" {
		t.Fatalf("response = %q, want grace summary", response)
	}
	if provider.finalPhaseCalls.Load() != 1 {
		t.Fatalf("final summary calls = %d, want 1", provider.finalPhaseCalls.Load())
	}
}

func TestMaxIterationsUsesToolLessGraceCall(t *testing.T) {
	cfg := baseLongTurnConfig(t.TempDir())
	cfg.Agents.Defaults.MaxToolIterations = 1

	provider := &graceSummaryProvider{}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	al.RegisterTool(&toolLimitTestTool{})

	response, err := al.ProcessDirectWithChannel(context.Background(), "hello", "grace", "test", "chat1")
	if err != nil {
		t.Fatalf("ProcessDirectWithChannel: %v", err)
	}
	if response != "grace summary" {
		t.Fatalf("response = %q, want grace summary", response)
	}
	if provider.toolPhaseCalls.Load() != 1 || provider.finalPhaseCalls.Load() != 1 {
		t.Fatalf("calls = tool:%d final:%d, want 1/1", provider.toolPhaseCalls.Load(), provider.finalPhaseCalls.Load())
	}
}

func TestGraceCallIsOneShot(t *testing.T) {
	ts := newTurnState(&AgentInstance{ID: "test"}, processOptions{}, turnEventScope{})
	if !ts.tryUseGraceCall() {
		t.Fatal("first grace call must be allowed")
	}
	if ts.tryUseGraceCall() {
		t.Fatal("second grace call must be refused")
	}
	if !ts.graceCallUsed() {
		t.Fatal("graceCallUsed must report true")
	}
}

func TestBuildStopSummaryIncludesReasonAndLastTool(t *testing.T) {
	summary := buildStopSummary("budget", 20*time.Minute, 23, "exec", "partial output")
	for _, want := range []string{"time budget was exhausted", "23 step", "Last tool: exec", "partial output", "not completed"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary missing %q: %s", want, summary)
		}
	}
}

func TestTurnDeadlineHelpers(t *testing.T) {
	ts := newTurnState(&AgentInstance{ID: "test"}, processOptions{}, turnEventScope{})
	now := time.Now()
	ts.nowFn = func() time.Time { return now }
	ts.setDeadline(now.Add(10 * time.Second))
	if ts.deadlineExceeded() {
		t.Fatal("deadline must not be exceeded yet")
	}
	if remaining, ok := ts.remainingBudget(); !ok || remaining != 10*time.Second {
		t.Fatalf("remaining = %s ok=%v", remaining, ok)
	}
	now = now.Add(11 * time.Second)
	if !ts.deadlineExceeded() {
		t.Fatal("deadline must be exceeded")
	}
	if remaining, ok := ts.remainingBudget(); !ok || remaining != 0 {
		t.Fatalf("remaining after deadline = %s ok=%v", remaining, ok)
	}
	ts.markTimedOut("budget")
	if timedOut, reason := ts.timedOutInfo(); !timedOut || reason != "budget" {
		t.Fatalf("timedOut=%v reason=%q", timedOut, reason)
	}
}

type fixedToolCallProvider struct {
	toolName string
	args     map[string]any
	calls    atomic.Int32
}

func (m *fixedToolCallProvider) Chat(
	_ context.Context,
	_ []providers.Message,
	_ []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	m.calls.Add(1)
	return &providers.LLMResponse{
		ToolCalls: []providers.ToolCall{{
			ID:        "call_fixed",
			Type:      "function",
			Name:      m.toolName,
			Arguments: m.args,
		}},
	}, nil
}

func (m *fixedToolCallProvider) GetDefaultModel() string { return "fixed-tool-model" }

type ctxAwareSlowTool struct{}

func (m *ctxAwareSlowTool) Name() string        { return "slow_tool" }
func (m *ctxAwareSlowTool) Description() string { return "slow test tool" }
func (m *ctxAwareSlowTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (m *ctxAwareSlowTool) Execute(ctx context.Context, _ map[string]any) *tools.ToolResult {
	select {
	case <-ctx.Done():
		return tools.ErrorResult("tool canceled: " + ctx.Err().Error())
	case <-time.After(10 * time.Second):
		return tools.SilentResult("done")
	}
}

type alwaysErrorTool struct{}

func (m *alwaysErrorTool) Name() string        { return "always_error" }
func (m *alwaysErrorTool) Description() string { return "always fails" }
func (m *alwaysErrorTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (m *alwaysErrorTool) Execute(_ context.Context, _ map[string]any) *tools.ToolResult {
	return tools.ErrorResult("boom: permission denied")
}

func TestTurnTimeBudgetInterruptsRunningTool(t *testing.T) {
	cfg := baseLongTurnConfig(t.TempDir())
	cfg.Agents.Defaults.TurnTimeBudgetSeconds = 1
	cfg.Agents.Defaults.TurnGraceCall = boolPtr(false)

	provider := &fixedToolCallProvider{toolName: "slow_tool"}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	al.RegisterTool(&ctxAwareSlowTool{})

	start := time.Now()
	response, err := al.ProcessDirectWithChannel(context.Background(), "hello", "budget-tool", "test", "chat1")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("ProcessDirectWithChannel: %v", err)
	}
	if !strings.Contains(response, "time budget was exhausted") {
		t.Fatalf("response = %q, want time-budget summary", response)
	}
	if elapsed > 6*time.Second {
		t.Fatalf("turn took %s, running tool exceeded the budget", elapsed)
	}
}

func TestIdenticalToolErrorStopsTurn(t *testing.T) {
	cfg := baseLongTurnConfig(t.TempDir())
	provider := &fixedToolCallProvider{toolName: "always_error"}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	al.RegisterTool(&alwaysErrorTool{})

	response, err := al.ProcessDirectWithChannel(context.Background(), "hello", "loop-error", "test", "chat1")
	if err != nil {
		t.Fatalf("ProcessDirectWithChannel: %v", err)
	}
	if !strings.Contains(response, "failed") || !strings.Contains(response, "boom: permission denied") {
		t.Fatalf("response = %q, want a visible identical-error stop", response)
	}
	if calls := provider.calls.Load(); calls != 3 {
		t.Fatalf("LLM calls = %d, want 3 (stop after 3 identical errors)", calls)
	}
}

func TestTimeoutEventSeverity(t *testing.T) {
	sev := runtimeSeverityForAgentEvent(runtimeevents.KindAgentTurnEnd, TurnEndPayload{Status: TurnEndStatusTimeout})
	if sev != runtimeevents.SeverityWarn {
		t.Fatalf("timeout severity = %v, want warn", sev)
	}
}
