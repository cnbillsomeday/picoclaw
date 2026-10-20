// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
)

type completionErrorProvider struct{}

func (m *completionErrorProvider) Chat(
	_ context.Context,
	_ []providers.Message,
	_ []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	return nil, errors.New("forced model failure")
}

func (m *completionErrorProvider) GetDefaultModel() string { return "error-model" }

func runCompletionTest(t *testing.T, provider providers.LLMProvider, content string) (bus.InboundResult, int, bool) {
	t.Helper()
	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         t.TempDir(),
				ModelName:         "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 3,
			},
		},
	}
	mb := bus.NewMessageBus()
	al := NewAgentLoop(cfg, mb, provider)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = al.Run(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}()

	results := make(chan bus.InboundResult, 4)
	outboundBeforeCompletion := false
	msg := bus.InboundMessage{
		Context: bus.InboundContext{
			Channel:   "test",
			ChatID:    "chat1",
			ChatType:  "direct",
			SenderID:  "user1",
			MessageID: "m_1",
		},
		Channel:   "test",
		ChatID:    "chat1",
		SenderID:  "user1",
		MessageID: "m_1",
		Content:   content,
		Completion: func(result bus.InboundResult) {
			select {
			case <-mb.OutboundChan():
				outboundBeforeCompletion = true
			default:
			}
			results <- result
		},
	}
	if err := mb.PublishInbound(context.Background(), msg); err != nil {
		t.Fatalf("PublishInbound: %v", err)
	}

	select {
	case result := <-results:
		// Give a broken duplicate path a moment to fire; exactly-once must hold.
		time.Sleep(50 * time.Millisecond)
		return result, len(results), outboundBeforeCompletion
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for inbound completion")
		return bus.InboundResult{}, 0, false
	}
}

func TestInboundCompletionCompleted(t *testing.T) {
	result, extra, outboundSeen := runCompletionTest(t, &simpleMockProvider{response: "done"}, "hello")
	if result.Status != bus.InboundResultCompleted {
		t.Fatalf("status = %q, want completed", result.Status)
	}
	if extra != 0 {
		t.Fatalf("completion fired %d extra time(s)", extra)
	}
	if !outboundSeen {
		t.Fatal("completion fired before the final outbound was published")
	}
}

func TestInboundCompletionError(t *testing.T) {
	result, extra, _ := runCompletionTest(t, &completionErrorProvider{}, "hello")
	if result.Status != bus.InboundResultError {
		t.Fatalf("status = %q, want error", result.Status)
	}
	if extra != 0 {
		t.Fatalf("completion fired %d extra time(s)", extra)
	}
}

func TestInboundCompletionCommand(t *testing.T) {
	result, extra, _ := runCompletionTest(t, &simpleMockProvider{response: "unused"}, "/new")
	if result.Status != bus.InboundResultCompleted {
		t.Fatalf("status = %q, want completed", result.Status)
	}
	if extra != 0 {
		t.Fatalf("completion fired %d extra time(s)", extra)
	}
}

func TestA2ASendToolRegisteredWhenChannelEnabled(t *testing.T) {
	cfg := &config.Config{
		Version: config.CurrentVersion,
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace: t.TempDir(),
				ModelName: "test-model",
				MaxTokens: 1024,
			},
		},
		Channels: config.ChannelsConfig{
			"a2a": &config.Channel{
				Enabled: true,
				Type:    config.ChannelA2A,
			},
		},
	}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), &simpleMockProvider{response: "ok"})
	agent := al.registry.GetDefaultAgent()
	if agent == nil {
		t.Fatal("no default agent")
	}
	if _, ok := agent.Tools.Get("a2a_send"); !ok {
		t.Fatal("a2a_send tool was not registered for an enabled A2A channel")
	}
}

func TestA2AFinalOutboundPreservesInboundContext(t *testing.T) {
	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         t.TempDir(),
				ModelName:         "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 3,
			},
		},
	}
	mb := bus.NewMessageBus()
	al := NewAgentLoop(cfg, mb, &simpleMockProvider{response: "done"})
	completion := make(chan bus.InboundResult, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = al.Run(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}()

	msg := bus.InboundMessage{
		Context: bus.InboundContext{
			Channel:   "a2a",
			ChatID:    "alpha",
			ChatType:  "direct",
			SenderID:  "alpha",
			MessageID: "m_1",
			Raw: map[string]string{
				"a2a_conversation_id": "c_1",
				"a2a_inbound_preview": "hello",
			},
		},
		Channel:   "a2a",
		ChatID:    "alpha",
		SenderID:  "alpha",
		MessageID: "m_1",
		Content:   "hello",
		Completion: func(result bus.InboundResult) {
			completion <- result
		},
	}
	if err := mb.PublishInbound(context.Background(), msg); err != nil {
		t.Fatalf("PublishInbound: %v", err)
	}
	select {
	case out := <-mb.OutboundChan():
		if out.Context.MessageID != "m_1" {
			t.Fatalf("outbound message id = %q, want m_1", out.Context.MessageID)
		}
		if out.Context.Raw["a2a_conversation_id"] != "c_1" {
			t.Fatalf("outbound conversation metadata = %q", out.Context.Raw["a2a_conversation_id"])
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for final outbound")
	}

	select {
	case result := <-completion:
		if result.Status != bus.InboundResultCompleted {
			t.Fatalf("completion status = %q, want completed", result.Status)
		}
		if result.Reply != "done" {
			t.Fatalf("completion reply = %q, want done", result.Reply)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for completion")
	}
}
