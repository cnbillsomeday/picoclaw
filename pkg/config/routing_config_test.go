// PicoClaw - Ultra-lightweight personal AI agent
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors

package config

import (
	"encoding/json"
	"testing"
)

func TestRoutingConfigGuardGetters_DefaultsWhenZeroOrMissing(t *testing.T) {
	// A missing (nil) routing section must not panic and must yield defaults
	// so the guards stay active for every default configuration.
	var nilRouting *RoutingConfig
	if got := nilRouting.GetEscalatePromptTokens(); got != DefaultRoutingEscalatePromptTokens {
		t.Fatalf("nil GetEscalatePromptTokens() = %d, want %d", got, DefaultRoutingEscalatePromptTokens)
	}
	if got := nilRouting.GetEscalateToolIterations(); got != DefaultRoutingEscalateToolIterations {
		t.Fatalf("nil GetEscalateToolIterations() = %d, want %d", got, DefaultRoutingEscalateToolIterations)
	}
	if got := nilRouting.GetRepeatBreakerIterations(); got != DefaultRoutingRepeatBreakerIterations {
		t.Fatalf("nil GetRepeatBreakerIterations() = %d, want %d", got, DefaultRoutingRepeatBreakerIterations)
	}

	zero := &RoutingConfig{}
	if got := zero.GetEscalatePromptTokens(); got != DefaultRoutingEscalatePromptTokens {
		t.Fatalf("zero GetEscalatePromptTokens() = %d, want %d", got, DefaultRoutingEscalatePromptTokens)
	}
	if got := zero.GetEscalateToolIterations(); got != DefaultRoutingEscalateToolIterations {
		t.Fatalf("zero GetEscalateToolIterations() = %d, want %d", got, DefaultRoutingEscalateToolIterations)
	}
	if got := zero.GetRepeatBreakerIterations(); got != DefaultRoutingRepeatBreakerIterations {
		t.Fatalf("zero GetRepeatBreakerIterations() = %d, want %d", got, DefaultRoutingRepeatBreakerIterations)
	}
}

func TestRoutingConfigGuardGetters_NegativeDisables(t *testing.T) {
	cfg := &RoutingConfig{EscalatePromptTokens: -1, EscalateToolIterations: -1, RepeatBreakerIterations: -1}
	if got := cfg.GetEscalatePromptTokens(); got != 0 {
		t.Fatalf("negative GetEscalatePromptTokens() = %d, want 0 (disabled)", got)
	}
	if got := cfg.GetEscalateToolIterations(); got != 0 {
		t.Fatalf("negative GetEscalateToolIterations() = %d, want 0 (disabled)", got)
	}
	if got := cfg.GetRepeatBreakerIterations(); got != 0 {
		t.Fatalf("negative GetRepeatBreakerIterations() = %d, want 0 (disabled)", got)
	}
}

func TestRoutingConfigGuardGetters_PositiveOverrides(t *testing.T) {
	cfg := &RoutingConfig{EscalatePromptTokens: 8000, EscalateToolIterations: 7, RepeatBreakerIterations: 2}
	if got := cfg.GetEscalatePromptTokens(); got != 8000 {
		t.Fatalf("GetEscalatePromptTokens() = %d, want 8000", got)
	}
	if got := cfg.GetEscalateToolIterations(); got != 7 {
		t.Fatalf("GetEscalateToolIterations() = %d, want 7", got)
	}
	if got := cfg.GetRepeatBreakerIterations(); got != 2 {
		t.Fatalf("GetRepeatBreakerIterations() = %d, want 2", got)
	}
}

func TestRoutingConfigNewFields_KeepUnknownInputRoundTrip(t *testing.T) {
	// The new guard fields must round-trip through plain JSON unmarshalling
	// (config.json embeds the routing section inline).
	raw := `{"escalate_prompt_tokens":40000,"escalate_tool_iterations":2,"repeat_breaker_iterations":9}`
	decoded := &RoutingConfig{}
	if err := json.Unmarshal([]byte(raw), decoded); err != nil {
		t.Fatalf("unmarshal RoutingConfig = %v", err)
	}
	if decoded.EscalatePromptTokens != 40000 || decoded.EscalateToolIterations != 2 || decoded.RepeatBreakerIterations != 9 {
		t.Fatalf("round trip mismatch: %+v", decoded)
	}
}
