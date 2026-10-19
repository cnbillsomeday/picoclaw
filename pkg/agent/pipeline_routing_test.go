// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// newRoutingGuardChatServer starts an OpenAI-compatible chat completions mock.
// respond is called with the 1-based call index and must return the full
// response payload; the request's model field is asserted against expectedModel.
func newRoutingGuardChatServer(
	t *testing.T,
	label string,
	expectedModel string,
	calls *int,
	respond func(n int) map[string]any,
) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		*calls = *calls + 1
		defer r.Body.Close()

		var req struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode %s request: %v", label, err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if req.Model != expectedModel {
			t.Errorf("%s server model = %q, want %q", label, req.Model, expectedModel)
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(respond(*calls)); err != nil {
			t.Errorf("encode %s response: %v", label, err)
		}
	}))
}

// toolCallCompletionResponse builds a non-streaming chat completion carrying n
// tool calls with identical arguments (the degenerate-loop signature).
func toolCallCompletionResponse(toolName, argsJSON string, usage map[string]any) map[string]any {
	resp := map[string]any{
		"choices": []map[string]any{
			{
				"message": map[string]any{
					"content": "",
					"tool_calls": []map[string]any{
						{
							"id":   "call_guard_test",
							"type": "function",
							"function": map[string]any{
								"name":      toolName,
								"arguments": argsJSON,
							},
						},
					},
				},
				"finish_reason": "tool_calls",
			},
		},
	}
	if usage != nil {
		resp["usage"] = usage
	}
	return resp
}

func plainCompletionResponse(content string, usage map[string]any) map[string]any {
	resp := map[string]any{
		"choices": []map[string]any{
			{
				"message":       map[string]any{"content": content},
				"finish_reason": "stop",
			},
		},
	}
	if usage != nil {
		resp["usage"] = usage
	}
	return resp
}

func newRoutingGuardConfig(
	t *testing.T,
	workspace string,
	lightServer, heavyServer *httptest.Server,
	routing *config.RoutingConfig,
) *config.Config {
	return &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         workspace,
				ModelName:         "gemini-main",
				MaxTokens:         4096,
				MaxToolIterations: 10,
				Routing:           routing,
			},
		},
		ModelList: []*config.ModelConfig{
			{
				ModelName: "gemini-main",
				Model:     "openrouter/gemini-2.5-flash",
				APIBase:   heavyServer.URL,
				APIKeys:   config.SimpleSecureStrings("heavy-key"),
			},
			{
				ModelName: "qwen-light",
				Model:     "ollama/qwen2.5:0.5b",
				APIBase:   lightServer.URL,
				APIKeys:   config.SimpleSecureStrings("light-key"),
			},
		},
	}
}

func newRoutingGuardLoop(t *testing.T, cfg *config.Config) *AgentLoop {
	t.Helper()

	msgBus := bus.NewMessageBus()
	provider, _, err := providers.CreateProvider(cfg)
	if err != nil {
		t.Fatalf("CreateProvider() error = %v", err)
	}
	al := NewAgentLoop(cfg, msgBus, provider)
	al.RegisterTool(&toolLimitTestTool{})
	return al
}

// TestRoutingGuards_RepeatBreakerStopsIdenticalToolLoop verifies the breaker
// stops a light model that repeats the exact same tool call forever, instead
// of burning tokens until the iteration cap.
func TestRoutingGuards_RepeatBreakerStopsIdenticalToolLoop(t *testing.T) {
	lightCalls := 0
	lightServer := newRoutingGuardChatServer(
		t, "light", "qwen2.5:0.5b", &lightCalls,
		func(n int) map[string]any {
			return toolCallCompletionResponse(
				"tool_limit_test_tool", `{"value":"x"}`, nil,
			)
		},
	)
	defer lightServer.Close()

	heavyCalls := 0
	heavyServer := newStrictChatCompletionTestServer(
		t, "heavy", "gemini-2.5-flash", "heavy reply", &heavyCalls,
	)
	defer heavyServer.Close()

	cfg := newRoutingGuardConfig(t, t.TempDir(), lightServer, heavyServer, &config.RoutingConfig{
		Enabled:                 true,
		LightModel:              "qwen-light",
		Threshold:               0.35,
		EscalatePromptTokens:    -1, // isolate the breaker
		EscalateToolIterations:  -1,
		RepeatBreakerIterations: 5,
	})
	al := newRoutingGuardLoop(t, cfg)
	helper := testHelper{al: al}

	resp := helper.executeAndGetResponse(t, context.Background(), bus.InboundMessage{
		Channel:  "telegram",
		SenderID: "user1",
		ChatID:   "chat1",
		Content:  "hi",
	})

	if !strings.Contains(resp, "repeating tool-call loop") {
		t.Fatalf("response = %q, want repeat breaker message", resp)
	}
	if heavyCalls != 0 {
		t.Fatalf("heavy calls = %d, want 0", heavyCalls)
	}
	if lightCalls < 5 || lightCalls > 6 {
		t.Fatalf("light calls = %d, want 5..6 (breaker at the 5th identical call)", lightCalls)
	}
}

// TestRoutingGuards_EscalatesToPrimaryOnPromptTokens verifies a ballooned
// prompt on a light-model turn upgrades the rest of the turn to the primary
// model pool.
func TestRoutingGuards_EscalatesToPrimaryOnPromptTokens(t *testing.T) {
	lightCalls := 0
	lightServer := newRoutingGuardChatServer(
		t, "light", "qwen2.5:0.5b", &lightCalls,
		func(n int) map[string]any {
			return toolCallCompletionResponse("tool_limit_test_tool", `{"value":"x"}`, map[string]any{
				"prompt_tokens":     50000,
				"completion_tokens": 75,
				"total_tokens":      50075,
			})
		},
	)
	defer lightServer.Close()

	heavyCalls := 0
	heavyServer := newStrictChatCompletionTestServer(
		t, "heavy", "gemini-2.5-flash", "heavy reply", &heavyCalls,
	)
	defer heavyServer.Close()

	al := newRoutingGuardLoop(t, newRoutingGuardConfig(t, t.TempDir(), lightServer, heavyServer, &config.RoutingConfig{
		Enabled:    true,
		LightModel: "qwen-light",
		Threshold:  0.35,
	}))
	helper := testHelper{al: al}

	resp := helper.executeAndGetResponse(t, context.Background(), bus.InboundMessage{
		Channel:  "telegram",
		SenderID: "user1",
		ChatID:   "chat1",
		Content:  "hi",
	})

	if resp != "heavy reply" {
		t.Fatalf("response = %q, want %q", resp, "heavy reply")
	}
	if lightCalls != 1 {
		t.Fatalf("light calls = %d, want 1 (escalated after the first ballooned response)", lightCalls)
	}
	if heavyCalls < 1 {
		t.Fatalf("heavy calls = %d, want >=1", heavyCalls)
	}
}

// TestRoutingGuards_EscalatesToPrimaryOnToolIntensity verifies that repeated
// tool-calling iterations on the light model hand the turn to the primary pool.
func TestRoutingGuards_EscalatesToPrimaryOnToolIntensity(t *testing.T) {
	lightCalls := 0
	lightServer := newRoutingGuardChatServer(
		t, "light", "qwen2.5:0.5b", &lightCalls,
		func(n int) map[string]any {
			// Varying args keep the repeat breaker out of the picture; tool
			// intensity (2 consecutive tool iterations) is what escalates.
			return toolCallCompletionResponse(
				"tool_limit_test_tool", fmt.Sprintf(`{"value":"q-%d"}`, n), map[string]any{
					"prompt_tokens":     1000,
					"completion_tokens": 75,
					"total_tokens":      1075,
				},
			)
		},
	)
	defer lightServer.Close()

	heavyCalls := 0
	heavyServer := newStrictChatCompletionTestServer(
		t, "heavy", "gemini-2.5-flash", "heavy reply", &heavyCalls,
	)
	defer heavyServer.Close()

	al := newRoutingGuardLoop(t, newRoutingGuardConfig(t, t.TempDir(), lightServer, heavyServer, &config.RoutingConfig{
		Enabled:                true,
		LightModel:             "qwen-light",
		Threshold:              0.35,
		EscalatePromptTokens:   -1, // isolate tool-intensity escalation
		EscalateToolIterations: 2,
	}))
	helper := testHelper{al: al}

	resp := helper.executeAndGetResponse(t, context.Background(), bus.InboundMessage{
		Channel:  "telegram",
		SenderID: "user1",
		ChatID:   "chat1",
		Content:  "hi",
	})

	if resp != "heavy reply" {
		t.Fatalf("response = %q, want %q", resp, "heavy reply")
	}
	if lightCalls != 2 {
		t.Fatalf("light calls = %d, want 2 (escalated after two tool iterations)", lightCalls)
	}
	if heavyCalls < 1 {
		t.Fatalf("heavy calls = %d, want >=1", heavyCalls)
	}
}

// TestRoutingGuards_LightPoolFailureEscalatesToPrimary verifies a dead light
// pool (auth failure) does not fail the turn: the primary pool takes over.
func TestRoutingGuards_LightPoolFailureEscalatesToPrimary(t *testing.T) {
	lightCalls := 0
	lightServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lightCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": "invalid api key",
				"type":    "authentication_error",
			},
		})
	}))
	defer lightServer.Close()

	heavyCalls := 0
	heavyServer := newStrictChatCompletionTestServer(
		t, "heavy", "gemini-2.5-flash", "heavy reply", &heavyCalls,
	)
	defer heavyServer.Close()

	cfg := newRoutingGuardConfig(t, t.TempDir(), lightServer, heavyServer, &config.RoutingConfig{
		Enabled:    true,
		LightModel: "qwen-light",
		Threshold:  0.35,
	})
	cfg.Agents.Defaults.MaxLLMRetries = 1 // keep the failure path quick
	al := newRoutingGuardLoop(t, cfg)
	helper := testHelper{al: al}

	resp := helper.executeAndGetResponse(t, context.Background(), bus.InboundMessage{
		Channel:  "telegram",
		SenderID: "user1",
		ChatID:   "chat1",
		Content:  "hi",
	})

	if resp != "heavy reply" {
		t.Fatalf("response = %q, want %q", resp, "heavy reply")
	}
	if lightCalls < 1 {
		t.Fatalf("light calls = %d, want >=1", lightCalls)
	}
	if heavyCalls < 1 {
		t.Fatalf("heavy calls = %d, want >=1", heavyCalls)
	}
}

func TestRoutingGuards_SteeringHardCap(t *testing.T) {
	if got := steeringHardCap(50); got != 150 {
		t.Fatalf("steeringHardCap(50) = %d, want 150", got)
	}
	if got := steeringHardCap(1); got != 21 {
		t.Fatalf("steeringHardCap(1) = %d, want 21 (factor floor)", got)
	}
	if got := steeringHardCap(0); got != 0 {
		t.Fatalf("steeringHardCap(0) = %d, want 0 (unlimited config)", got)
	}
}

func TestRoutingGuards_ToolCallSignature(t *testing.T) {
	a := providers.ToolCall{
		ID:   "call_1",
		Name: "shell",
		Function: &providers.FunctionCall{
			Name:      "shell",
			Arguments: `{"cmd":"ls","dir":"/tmp"}`,
		},
		Arguments: map[string]any{"cmd": "ls", "dir": "/tmp"},
	}
	b := providers.ToolCall{
		ID:   "call_999", // different ID must not change the signature
		Name: "shell",
		Function: &providers.FunctionCall{
			Name:      "shell",
			Arguments: `{"dir":"/tmp","cmd":"ls"}`, // key order differs
		},
		Arguments: map[string]any{"cmd": "ls", "dir": "/tmp"},
	}
	c := providers.ToolCall{
		ID:   "call_1",
		Name: "shell",
		Function: &providers.FunctionCall{
			Name:      "shell",
			Arguments: `{"cmd":"ls -la","dir":"/tmp"}`,
		},
		Arguments: map[string]any{"cmd": "ls -la", "dir": "/tmp"},
	}

	if toolCallSignature([]providers.ToolCall{a}) != toolCallSignature([]providers.ToolCall{b}) {
		t.Fatal("identical calls with reordered args/other IDs must share a signature")
	}
	if toolCallSignature([]providers.ToolCall{a}) == toolCallSignature([]providers.ToolCall{c}) {
		t.Fatal("different args must not share a signature")
	}
	if toolCallSignature(nil) != "" || toolCallSignature([]providers.ToolCall{{}}) == "" {
		t.Fatal("empty input must be empty signature; name-only call must hash")
	}
}
