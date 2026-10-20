// PicoClaw - Ultra-lightweight personal AI agent

package config

import (
	"encoding/json"
	"testing"
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
	if err := json.Unmarshal([]byte(`{"version":3,"a2a_server":{"enabled":true,"listen":"127.0.0.1:21220","data_dir":"/root/a2a","domain":"a2a.example","retention_hours":24,"sign_mode":"strict","allow_default_agents":false}}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.A2AServer.Enabled || cfg.A2AServer.DataDir != "/root/a2a" || cfg.A2AServer.SignMode != "strict" {
		t.Fatalf("a2a_server = %+v", cfg.A2AServer)
	}
	if cfg.A2AServer.AllowDefaultAgents {
		t.Fatal("allow_default_agents must default to false")
	}
}
