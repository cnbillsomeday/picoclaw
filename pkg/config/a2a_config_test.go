// PicoClaw - Ultra-lightweight personal AI agent

package config

import (
	"encoding/json"
	"testing"
	"time"
)

func TestA2AChannelDefaults(t *testing.T) {
	cfg := DefaultConfig()
	bc := cfg.Channels.Get(ChannelA2A)
	if bc == nil {
		t.Fatal("DefaultConfig() has no a2a channel")
	}
	if bc.Type != ChannelA2A {
		t.Fatalf("a2a type = %q", bc.Type)
	}
	decoded, err := bc.GetDecoded()
	if err != nil {
		t.Fatalf("GetDecoded: %v", err)
	}
	settings, ok := decoded.(*A2ASettings)
	if !ok {
		t.Fatalf("decoded type = %T", decoded)
	}
	if !settings.SignEnabled() {
		t.Fatal("signing must default to enabled")
	}
	if settings.PollIntervalSec != 30 {
		t.Fatalf("poll_interval_sec = %d, want 30", settings.PollIntervalSec)
	}
	if settings.MaxMessageChars != 8000 {
		t.Fatalf("max_message_chars = %d, want 8000", settings.MaxMessageChars)
	}
	if !IsSingletonChannel(ChannelA2A) {
		t.Fatal("a2a must be a singleton channel")
	}
}

func TestA2ASettingsSignCanBeDisabled(t *testing.T) {
	var settings A2ASettings
	if err := json.Unmarshal([]byte(`{"sign":false}`), &settings); err != nil {
		t.Fatal(err)
	}
	if settings.SignEnabled() {
		t.Fatal("explicit sign=false must disable signing")
	}
	var omitted A2ASettings
	if err := json.Unmarshal([]byte(`{}`), &omitted); err != nil {
		t.Fatal(err)
	}
	if !omitted.SignEnabled() {
		t.Fatal("omitted sign must default to enabled")
	}
}

func TestA2AServerConfigRoundTrip(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal([]byte(`{"version":3,"a2a_server":{"enabled":true,"listen":"127.0.0.1:21220","data_dir":"/var/lib/picoclaw/a2a","domain":"a2a.example","retention_hours":24,"sign_mode":"strict","allow_default_agents":false}}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.A2AServer.Enabled || cfg.A2AServer.DataDir != "/var/lib/picoclaw/a2a" || cfg.A2AServer.SignMode != "strict" {
		t.Fatalf("a2a_server = %+v", cfg.A2AServer)
	}
	if cfg.A2AServer.AllowDefaultAgents {
		t.Fatal("allow_default_agents must default to false")
	}
}

func TestA2ALoopGuardDefaults(t *testing.T) {
	var settings A2ASettings
	if got := settings.GetReplyWindow(); got != 10*time.Minute {
		t.Fatalf("default reply window = %s, want 10m", got)
	}
	if got := settings.GetMaxRepliesPerWindow(); got != 3 {
		t.Fatalf("default max replies = %d, want 3", got)
	}
}

func TestA2ALoopGuardExplicitValues(t *testing.T) {
	window := 120
	maxReplies := 5
	settings := A2ASettings{ReplyWindowSec: &window, MaxRepliesPerWindow: &maxReplies}
	if got := settings.GetReplyWindow(); got != 2*time.Minute {
		t.Fatalf("reply window = %s, want 2m", got)
	}
	if got := settings.GetMaxRepliesPerWindow(); got != 5 {
		t.Fatalf("max replies = %d, want 5", got)
	}

	zero := 0
	disabled := A2ASettings{MaxRepliesPerWindow: &zero}
	if got := disabled.GetMaxRepliesPerWindow(); got != 0 {
		t.Fatalf("explicit zero max replies = %d, want 0 (disabled)", got)
	}
	negative := -1
	clamped := A2ASettings{MaxRepliesPerWindow: &negative}
	if got := clamped.GetMaxRepliesPerWindow(); got != 0 {
		t.Fatalf("negative max replies = %d, want 0", got)
	}
}

func TestA2AInboundReliabilityDefaults(t *testing.T) {
	cfg := DefaultConfig()
	bc := cfg.Channels.Get(ChannelA2A)
	if bc == nil {
		t.Fatal("DefaultConfig() has no a2a channel")
	}
	decoded, err := bc.GetDecoded()
	if err != nil {
		t.Fatalf("GetDecoded: %v", err)
	}
	settings := decoded.(*A2ASettings)
	if !settings.RequireSignEnabled() {
		t.Fatal("require_sign must default to true")
	}
	if got := settings.GetInboundRetry(); got != time.Minute {
		t.Fatalf("inbound retry = %s, want 1m", got)
	}
	if got := settings.GetInboundMaxAttempts(); got != 3 {
		t.Fatalf("inbound max attempts = %d, want 3", got)
	}
	if got := settings.GetInboundTimeout(); got != 5*time.Minute {
		t.Fatalf("inbound timeout = %s, want 5m", got)
	}

	off := false
	explicit := A2ASettings{RequireSign: &off, InboundRetrySec: 10, InboundMaxAttempts: 5, InboundTimeoutSec: 30}
	if explicit.RequireSignEnabled() {
		t.Fatal("explicit require_sign=false was ignored")
	}
	if got := explicit.GetInboundRetry(); got != 10*time.Second {
		t.Fatalf("inbound retry = %s, want 10s", got)
	}
	if got := explicit.GetInboundMaxAttempts(); got != 5 {
		t.Fatalf("inbound max attempts = %d, want 5", got)
	}
	if got := explicit.GetInboundTimeout(); got != 30*time.Second {
		t.Fatalf("inbound timeout = %s, want 30s", got)
	}
}

func TestA2ANotifyDefaultsAndModes(t *testing.T) {
	var settings A2ASettings
	if got := settings.GetNotifyChannel(); got != "" {
		t.Fatalf("notify channel = %q, want empty", got)
	}
	if got := settings.GetNotifyChatID(); got != "" {
		t.Fatalf("notify chat id = %q, want empty", got)
	}
	if got := settings.GetNotifyMode(); got != "off" {
		t.Fatalf("notify mode = %q, want off", got)
	}
	if got := settings.GetNotifyMaxChars(); got != 400 {
		t.Fatalf("notify max chars = %d, want 400", got)
	}

	explicit := A2ASettings{
		NotifyChannel:  " telegram ",
		NotifyChatID:   " owner ",
		NotifyMode:     " ALL ",
		NotifyMaxChars: 5000,
	}
	if got := explicit.GetNotifyChannel(); got != "telegram" {
		t.Fatalf("notify channel = %q", got)
	}
	if got := explicit.GetNotifyChatID(); got != "owner" {
		t.Fatalf("notify chat id = %q", got)
	}
	if got := explicit.GetNotifyMode(); got != "all" {
		t.Fatalf("notify mode = %q, want all", got)
	}
	if got := explicit.GetNotifyMaxChars(); got != 4000 {
		t.Fatalf("notify max chars = %d, want 4000 cap", got)
	}

	for _, mode := range []string{"reply", "all", "failure", "off", "unknown", ""} {
		settings := A2ASettings{NotifyMode: mode}
		got := settings.GetNotifyMode()
		switch mode {
		case "reply", "all", "failure":
			if got != mode {
				t.Fatalf("mode %q normalized to %q", mode, got)
			}
		default:
			if got != "off" {
				t.Fatalf("mode %q normalized to %q, want off", mode, got)
			}
		}
	}
}
