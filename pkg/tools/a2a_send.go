// PicoClaw - Ultra-lightweight personal AI agent

package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/a2a/client"
	"github.com/sipeed/picoclaw/pkg/a2a/proactive"
	"github.com/sipeed/picoclaw/pkg/a2a/wire"
	"github.com/sipeed/picoclaw/pkg/config"
)

const defaultA2AKeyFile = "~/.a2a/ed25519.seed"

// A2ASendTool sends a proactive signed message to an allowlisted A2A peer.
type A2ASendTool struct {
	cfg *config.Config
}

// NewA2ASendTool builds the a2a_send tool. It is only registered when an
// enabled A2A channel exists.
func NewA2ASendTool(cfg *config.Config) *A2ASendTool {
	return &A2ASendTool{cfg: cfg}
}

func (t *A2ASendTool) Name() string { return "a2a_send" }

func (t *A2ASendTool) Description() string {
	return "Send a signed private A2A message to an allowlisted peer proactively; do not use it for normal replies (the channel replies automatically)"
}

func (t *A2ASendTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"to": map[string]any{
				"type":        "string",
				"description": "Recipient agent slug (must be in allow_from)",
			},
			"text": map[string]any{
				"type":        "string",
				"description": "Message body; long messages are split automatically",
			},
		},
		"required": []string{"to", "text"},
	}
}

func (t *A2ASendTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	if t == nil || t.cfg == nil {
		return ErrorResult("a2a_send: config is unavailable")
	}
	to := wire.NormalizeAgentSlug(stringArg(args, "to"))
	text := stringArg(args, "text")
	if to == "" {
		return ErrorResult("a2a_send: 'to' is required")
	}
	if strings.TrimSpace(text) == "" {
		return ErrorResult("a2a_send: 'text' is required")
	}

	channelName, bc, settings, err := findEnabledA2AChannel(t.cfg)
	if err != nil {
		return ErrorResult("a2a_send: " + err.Error())
	}
	if !a2aAllowed(bc, to) {
		return ErrorResult(fmt.Sprintf("a2a_send: recipient %q is not in allow_from", to))
	}
	if settings.DryRun {
		return SilentResult(fmt.Sprintf("a2a_send: dry_run is enabled; message to %s was not sent", to))
	}
	if !settings.SignEnabled() {
		return ErrorResult("a2a_send: signing is disabled; proactive sends require signatures")
	}
	baseURL := strings.TrimSpace(settings.BaseURL)
	if baseURL == "" {
		return ErrorResult("a2a_send: base_url is required")
	}
	if err := wire.ValidateBaseURL(baseURL); err != nil {
		return ErrorResult("a2a_send: " + err.Error())
	}

	// Preferred path: hand the message to the live channel so the per-peer
	// rate limit, dry-run, signing and clock gates are exactly the ones
	// automatic replies obey. Sending straight from the tool let an agent
	// flood a peer while its replies were capped.
	sessionKey := ToolSessionKey(ctx)
	if sender, ok := lookupProactiveSender(channelName); ok {
		ids, err := sender.SendProactive(ctx, to, text, sessionKey)
		if err != nil {
			return ErrorResult("a2a_send: " + err.Error())
		}
		if len(ids) == 0 {
			return SilentResult(fmt.Sprintf(
				"a2a_send: message to %s was suppressed (rate limit, dry_run or empty content)", to))
		}
		return SilentResult(fmt.Sprintf(
			"a2a_send: sent %d message(s) to %s (first id=%s)", len(ids), to, ids[0]))
	}
	// Fallback: no running channel in this process (unit tests, or a tool
	// constructed before the channel started). The clock check is kept; the
	// per-peer rate limit cannot be shared on this path and is skipped.

	keyFile := strings.TrimSpace(settings.KeyFile)
	if keyFile == "" {
		keyFile = defaultA2AKeyFile
	}
	signer, err := wire.LoadSigner(expandA2AHome(keyFile))
	if err != nil {
		return ErrorResult("a2a_send: load key_file: " + err.Error())
	}
	token := strings.TrimSpace(settings.Token.String())
	if token == "" {
		return ErrorResult("a2a_send: token is required")
	}
	timeout := time.Duration(settings.HTTPTimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	maxChars := settings.MaxMessageChars
	if maxChars <= 0 || maxChars > wire.MaxMessageRunes {
		maxChars = wire.MaxMessageRunes
	}

	api := client.New(baseURL, token, timeout)
	if err := checkClockSkewBestEffort(ctx, api, maxClockSkewForSend); err != nil {
		return ErrorResult("a2a_send: " + err.Error())
	}
	chunks := wire.SplitMessage(text, maxChars)
	ids := make([]string, 0, len(chunks))
	for i, chunk := range chunks {
		result, err := api.SendSigned(ctx, settings.AgentSlug, to, chunk, signer, client.SendOptions{
			MaxAttempts: 5,
			BaseBackoff: time.Second,
		})
		if err != nil {
			return ErrorResult(fmt.Sprintf(
				"a2a_send: chunk %d/%d failed after %d sent: %v",
				i+1, len(chunks), len(ids), err,
			))
		}
		ids = append(ids, result.MessageID)
	}
	if len(ids) > 0 {
		wire.MarkExplicitSend(ToolSessionKey(ctx), to)
	}
	return SilentResult(fmt.Sprintf(
		"a2a_send: sent %d message(s) to %s (first id=%s)",
		len(ids), to, ids[0],
	))
}

func findEnabledA2AChannel(cfg *config.Config) (string, *config.Channel, *config.A2ASettings, error) {
	if cfg == nil {
		return "", nil, nil, errors.New("config is unavailable")
	}
	for name, bc := range cfg.Channels {
		if bc == nil || !bc.Enabled || bc.Type != config.ChannelA2A {
			continue
		}
		decoded, err := bc.GetDecoded()
		if err != nil {
			return "", nil, nil, fmt.Errorf("decode a2a settings: %w", err)
		}
		settings, ok := decoded.(*config.A2ASettings)
		if !ok {
			return "", nil, nil, fmt.Errorf("invalid a2a settings type %T", decoded)
		}
		return name, bc, settings, nil
	}
	return "", nil, nil, errors.New("no enabled a2a channel configured")
}

// lookupProactiveSender finds the live channel that owns the configured A2A
// channel name, falling back to the only registered sender.
func lookupProactiveSender(channelName string) (proactive.Sender, bool) {
	if sender, ok := proactive.Lookup(channelName); ok {
		return sender, true
	}
	return proactive.LookupAny()
}

// maxClockSkewForSend mirrors the channel's send-time threshold.
const maxClockSkewForSend = 240 * time.Second

// checkClockSkewBestEffort rejects a clearly skewed clock before signing.
// A failed health request is advisory only and never blocks the send.
func checkClockSkewBestEffort(ctx context.Context, api *client.Client, limit time.Duration) error {
	serverTime, err := api.Health(ctx)
	if err != nil {
		return nil
	}
	skew := time.Until(serverTime)
	if skew < 0 {
		skew = -skew
	}
	if skew > limit {
		return fmt.Errorf("clock skew %s exceeds %s; fix NTP before sending signed messages",
			skew.Round(time.Second), limit)
	}
	return nil
}

func a2aAllowed(bc *config.Channel, slug string) bool {
	slug = wire.NormalizeAgentSlug(slug)
	for _, allowed := range bc.AllowFrom {
		allowed = wire.NormalizeAgentSlug(allowed)
		if allowed == "*" || strings.EqualFold(allowed, slug) {
			return true
		}
	}
	return false
}

func stringArg(args map[string]any, key string) string {
	value, _ := args[key].(string)
	return strings.TrimSpace(value)
}

func expandA2AHome(path string) string {
	path = strings.TrimSpace(path)
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil && home != "" {
			if path == "~" {
				return home
			}
			return filepath.Join(home, path[2:])
		}
	}
	return path
}
