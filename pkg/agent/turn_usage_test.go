// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// TestTurnUsageReportedToInboundCompletion locks the plumbing that feeds the
// owner notification's token statistics: every LLM response must be counted,
// and the terminal InboundResult must carry the accumulated usage.
func TestTurnUsageReportedToInboundCompletion(t *testing.T) {
	var got bus.InboundResult
	ts := &turnState{
		agent: &AgentInstance{MaxIterations: 20},
		opts: processOptions{
			Completion: func(result bus.InboundResult) { got = result },
		},
	}
	ts.recordLLMCall(&providers.UsageInfo{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120})
	ts.recordLLMCall(&providers.UsageInfo{PromptTokens: 150, CompletionTokens: 30, TotalTokens: 180})
	// A provider that omits usage still counts as a call.
	ts.recordLLMCall(nil)
	ts.setIteration(3)

	ts.completeInbound(bus.InboundResult{Status: bus.InboundResultCompleted, Reply: "done"})

	if got.Usage.TokensIn != 250 || got.Usage.TokensOut != 50 {
		t.Fatalf("usage tokens = in %d out %d, want 250/50", got.Usage.TokensIn, got.Usage.TokensOut)
	}
	if got.Usage.TotalTokens() != 300 {
		t.Fatalf("total tokens = %d, want 300", got.Usage.TotalTokens())
	}
	if got.Usage.LLMCalls != 3 {
		t.Fatalf("llm calls = %d, want 3", got.Usage.LLMCalls)
	}
	if got.Usage.Iterations != 3 {
		t.Fatalf("iterations = %d, want 3", got.Usage.Iterations)
	}
	if got.Usage.MaxIterations != 20 {
		t.Fatalf("max iterations = %d, want 20", got.Usage.MaxIterations)
	}
	if got.Status != bus.InboundResultCompleted || got.Reply != "done" {
		t.Fatalf("result = %+v, want status/reply preserved", got)
	}
}

// TestInboundCompletionFiresOnce keeps the once-only contract with usage filled.
func TestInboundCompletionFiresOnce(t *testing.T) {
	calls := 0
	ts := &turnState{
		agent: &AgentInstance{MaxIterations: 5},
		opts: processOptions{
			Completion: func(bus.InboundResult) { calls++ },
		},
	}
	ts.recordLLMCall(&providers.UsageInfo{PromptTokens: 1, CompletionTokens: 1})
	ts.completeInbound(bus.InboundResult{Status: bus.InboundResultCompleted})
	ts.completeInbound(bus.InboundResult{Status: bus.InboundResultError})
	if calls != 1 {
		t.Fatalf("completion called %d times, want 1", calls)
	}
}
