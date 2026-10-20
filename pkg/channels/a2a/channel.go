// PicoClaw - Ultra-lightweight personal AI agent

// Package a2a implements the client channel for the private A2A direct-message
// platform (Bearer token + Ed25519). It is NOT the standard a2a-protocol.org
// protocol.
package a2a

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/a2a/client"
	"github.com/sipeed/picoclaw/pkg/a2a/wire"
	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
)

const (
	defaultPollInterval  = 30 * time.Second
	minPollInterval      = 5 * time.Second
	defaultHTTPTimeout   = 20 * time.Second
	defaultPeersRefresh  = 300 * time.Second
	minPeersRefresh      = 30 * time.Second
	defaultMaxMessage    = wire.MaxMessageRunes
	authBackoff          = 5 * time.Minute
	clockWarnThreshold   = 60 * time.Second
	clockFailThreshold   = 240 * time.Second
	defaultStateFileName = ".a2a-channel-state.json"
	defaultKeyFile       = "~/.a2a/ed25519.seed"
	sendMaxAttempts      = 5
	sendBaseBackoff      = time.Second

	noReplySentinel = "@@NO_REPLY@@"

	// defaultAckPattern matches pure acknowledgements/closures. It is anchored
	// so only an entire short message is suppressed.
	defaultAckPattern = `(?i)^[（(\s]*(收到|好的|好|嗯|ok|明白|了解|已收到|已闭环|不回复|不回|沉默即确认|谢谢|多谢|再见|晚安)[）)\s。！!，,.]*$`

	defaultReplyPolicy = "回复规矩：有新信息量才回（提问/求助/正事/表态）；客套、确认、道别、已闭环不回，沉默即确认。拿不准 → 不回。\n" +
		"不需要回复时，最终输出只写 " + noReplySentinel + "，不要写任何其他内容（不要写“收到”“不回复”“已闭环”）。\n" +
		"不要调用旧的 clawdchat/heartbeat 脚本；收发由频道负责。\n" +
		"不要向对方发送工具执行进度、命令输出、内部核对过程。\n" +
		"回答技术问题只用已知事实，不要 grep 本机脚本/日志/配置；不确定就说不知道。\n" +
		"写完即止：不要发确认句、不要复述、不要再次核对。"
)

// closureMarkers catch short compound closures such as
// "（不回复，已闭环。）" and "收到，测试确认完毕。收工。" that the anchored
// pattern alone would miss.
var closureMarkers = []string{"不回复", "已闭环", "沉默即确认", "已回复", "测试确认", "收工", "不继续"}

// Channel is the picoclaw A2A client channel.
type Channel struct {
	*channels.BaseChannel

	settings *config.A2ASettings
	client   *client.Client
	signer   *wire.Signer
	state    *channelState

	agentSlug    string
	pollInterval time.Duration
	peersRefresh time.Duration
	cooldown     time.Duration
	maxChars     int
	dryRun       bool
	signEnabled  bool

	replyPolicy   string
	ackFilter     *regexp.Regexp
	customFilters []*regexp.Regexp
	replyWindow   time.Duration
	maxReplies    int
	now           func() time.Time

	requireSign        bool
	inboundRetry       time.Duration
	inboundMaxAttempts int
	inboundTimeout     time.Duration
	httpTimeout        time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	clockMu        sync.Mutex
	clockSkew      time.Duration
	clockCheckedAt time.Time

	peersMu sync.RWMutex
	peers   map[string]wire.Agent
	peersAt time.Time

	drySeenMu sync.Mutex
	drySeen   map[string]struct{}
}

// NewA2AChannel validates settings and builds the channel. It does not touch
// the network; Start begins polling.
func NewA2AChannel(bc *config.Channel, settings *config.A2ASettings, b *bus.MessageBus) (*Channel, error) {
	if bc == nil || settings == nil {
		return nil, errors.New("a2a: settings are required")
	}
	allow := []string(bc.AllowFrom)
	if len(allow) == 0 {
		return nil, errors.New("a2a: allow_from must list the peer agent slugs (all 8 platform agents); refusing to start open")
	}

	baseURL := strings.TrimSpace(settings.BaseURL)
	if baseURL == "" {
		return nil, errors.New("a2a: base_url is required")
	}
	agentSlug := strings.TrimSpace(settings.AgentSlug)
	if agentSlug == "" {
		return nil, errors.New("a2a: agent_slug is required")
	}
	token := settings.Token.String()
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("a2a: token is required")
	}

	signEnabled := settings.SignEnabled()
	var signer *wire.Signer
	if signEnabled {
		keyFile := strings.TrimSpace(settings.KeyFile)
		if keyFile == "" {
			keyFile = defaultKeyFile
		}
		loaded, err := wire.LoadSigner(expandHome(keyFile))
		if err != nil {
			return nil, fmt.Errorf("a2a: load key_file: %w", err)
		}
		signer = loaded
	}

	poll := time.Duration(settings.PollIntervalSec) * time.Second
	if poll <= 0 {
		poll = defaultPollInterval
	}
	if poll < minPollInterval {
		poll = minPollInterval
	}
	timeout := time.Duration(settings.HTTPTimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = defaultHTTPTimeout
	}
	refresh := time.Duration(settings.PeersRefreshSec) * time.Second
	if refresh <= 0 {
		refresh = defaultPeersRefresh
	}
	if refresh < minPeersRefresh {
		refresh = minPeersRefresh
	}
	maxChars := settings.MaxMessageChars
	if maxChars <= 0 || maxChars > wire.MaxMessageRunes {
		maxChars = defaultMaxMessage
	}
	statePath := strings.TrimSpace(settings.StateFile)
	if statePath == "" {
		statePath = filepath.Join(homeDir(), defaultStateFileName)
	}
	statePath = expandHome(statePath)
	state, err := loadChannelState(statePath, agentSlug)
	if err != nil {
		// A corrupt or foreign state file is backed up and reset; keep going.
		logger.WarnCF("a2a", "channel state reset", map[string]any{
			"agent": agentSlug,
			"path":  statePath,
			"error": err.Error(),
		})
	}
	if state == nil {
		// Unreadable state (e.g. permissions): start fresh instead of panicking.
		state = &channelState{path: statePath, data: stateData{Version: stateVersion, Agent: agentSlug}}
	}

	ackFilter, err := regexp.Compile(defaultAckPattern)
	if err != nil {
		return nil, fmt.Errorf("a2a: compile ack filter: %w", err)
	}
	customFilters := make([]*regexp.Regexp, 0, len(settings.SuppressPatterns))
	for _, pattern := range settings.SuppressPatterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		compiled, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("a2a: invalid suppress pattern %q: %w", pattern, err)
		}
		customFilters = append(customFilters, compiled)
	}
	replyPolicy := strings.TrimSpace(settings.ReplyPolicy)
	if file := strings.TrimSpace(settings.ReplyPolicyFile); file != "" {
		data, err := os.ReadFile(expandHome(file))
		if err != nil {
			return nil, fmt.Errorf("a2a: read reply_policy_file: %w", err)
		}
		replyPolicy = strings.TrimSpace(string(data))
	}
	if replyPolicy == "" {
		replyPolicy = defaultReplyPolicy
	}
	replyWindow := settings.GetReplyWindow()
	maxReplies := settings.GetMaxRepliesPerWindow()

	ch := &Channel{
		BaseChannel:        channels.NewBaseChannel("a2a", settings, b, allow),
		settings:           settings,
		client:             client.New(baseURL, token, timeout),
		signer:             signer,
		state:              state,
		agentSlug:          agentSlug,
		pollInterval:       poll,
		peersRefresh:       refresh,
		cooldown:           time.Duration(settings.CooldownSec) * time.Second,
		maxChars:           maxChars,
		dryRun:             settings.DryRun,
		signEnabled:        signEnabled,
		replyPolicy:        replyPolicy,
		ackFilter:          ackFilter,
		customFilters:      customFilters,
		replyWindow:        replyWindow,
		maxReplies:         maxReplies,
		now:                time.Now,
		requireSign:        settings.RequireSignEnabled(),
		inboundRetry:       settings.GetInboundRetry(),
		inboundMaxAttempts: settings.GetInboundMaxAttempts(),
		inboundTimeout:     settings.GetInboundTimeout(),
		httpTimeout:        timeout,
		peers:              map[string]wire.Agent{},
		drySeen:            map[string]struct{}{},
	}
	// Never trust a persisted 304 across restarts (nor a dry-run ETag when
	// switching to live). Deduplication uses last_seen, not the ETag.
	state.SetETag("")
	if err := state.Save(); err != nil {
		logger.WarnCF("a2a", "failed to persist startup state", map[string]any{
			"agent": agentSlug,
			"error": err.Error(),
		})
	}
	ch.SetOwner(ch)
	return ch, nil
}

// Start begins the polling loop.
func (c *Channel) Start(ctx context.Context) error {
	if c.IsRunning() {
		return nil
	}
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.SetRunning(true)
	c.wg.Add(1)
	go c.pollLoop()
	logger.InfoCF("a2a", "channel started", map[string]any{
		"agent":         c.agentSlug,
		"poll_interval": c.pollInterval.String(),
		"sign":          c.signEnabled,
		"dry_run":       c.dryRun,
		"max_chars":     c.maxChars,
		"state":         c.state.Path(),
	})
	return nil
}

// Stop cancels polling and persists state.
func (c *Channel) Stop(ctx context.Context) error {
	if c.cancel != nil {
		c.cancel()
	}
	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	c.SetRunning(false)
	if err := c.state.Save(); err != nil {
		logger.WarnCF("a2a", "failed to save channel state", map[string]any{"error": err.Error()})
	}
	logger.InfoCF("a2a", "channel stopped", map[string]any{"agent": c.agentSlug})
	return nil
}

// Send delivers one outbound message, splitting it to the platform limit.
func (c *Channel) Send(ctx context.Context, msg bus.OutboundMessage) ([]string, error) {
	if !c.IsRunning() {
		return nil, channels.ErrNotRunning
	}
	peer := normalizePeer(msg.ChatID)
	if peer == "" {
		return nil, fmt.Errorf("%w: empty peer chat id", channels.ErrSendFailed)
	}
	content := msg.Content
	trimmed := strings.TrimSpace(content)
	if reason := suppressedOutboundReason(msg); reason != "" {
		logger.DebugCF("a2a", "reply_suppressed", map[string]any{
			"reason": reason,
			"agent":  c.agentSlug,
			"peer":   peer,
		})
		return nil, nil
	}
	if trimmed == "" {
		return nil, nil
	}
	if trimmed == noReplySentinel {
		wire.ConsumeExplicitSend(msg.SessionKey, peer)
		logger.DebugCF("a2a", "reply_suppressed", map[string]any{
			"reason": "no_reply_sentinel",
			"agent":  c.agentSlug,
			"peer":   peer,
		})
		return nil, nil
	}
	if c.isAckOrClosure(trimmed) {
		wire.ConsumeExplicitSend(msg.SessionKey, peer)
		logger.InfoCF("a2a", "reply_suppressed", map[string]any{
			"reason": "ack_filter",
			"agent":  c.agentSlug,
			"peer":   peer,
			"text":   previewText(trimmed, 40),
		})
		return nil, nil
	}
	if wire.ConsumeExplicitSend(msg.SessionKey, peer) {
		logger.InfoCF("a2a", "reply_suppressed", map[string]any{
			"reason": "explicit_send",
			"agent":  c.agentSlug,
			"peer":   peer,
			"text":   previewText(trimmed, 40),
		})
		return nil, nil
	}
	if c.dryRun {
		logger.InfoCF("a2a", "outbound suppressed (dry_run)", map[string]any{
			"agent": c.agentSlug,
			"peer":  peer,
			"chars": len([]rune(content)),
		})
		return nil, nil
	}
	if c.maxReplies > 0 && c.replyWindow > 0 &&
		!c.state.AllowOutbound(peer, c.currentTime(), c.replyWindow, c.maxReplies) {
		logger.InfoCF("a2a", "reply_suppressed", map[string]any{
			"reason": "rate_limit",
			"agent":  c.agentSlug,
			"peer":   peer,
			"window": c.replyWindow.String(),
			"cap":    c.maxReplies,
			"text":   previewText(trimmed, 40),
		})
		return nil, nil
	}
	if c.signer == nil {
		return nil, fmt.Errorf("%w: signing is disabled but the platform requires signatures", channels.ErrSendFailed)
	}
	if err := c.ensureClock(ctx, true); err != nil {
		return nil, fmt.Errorf("%w: %v", channels.ErrSendFailed, err)
	}

	chunks := splitRunes(content, c.maxChars)
	ids := make([]string, 0, len(chunks))
	for i, chunk := range chunks {
		result, err := c.client.SendSigned(
			ctx, c.agentSlug, peer, chunk, c.signer,
			client.SendOptions{MaxAttempts: sendMaxAttempts, BaseBackoff: sendBaseBackoff},
		)
		if err != nil {
			if errors.Is(err, client.ErrReplayedNonce) {
				logger.WarnCF("a2a", "outbound possibly delivered before retry (nonce replay)", map[string]any{
					"agent": c.agentSlug,
					"peer":  peer,
					"chunk": i + 1,
				})
				continue
			}
			logger.ErrorCF("a2a", "outbound failed", map[string]any{
				"agent": c.agentSlug,
				"peer":  peer,
				"chunk": i + 1,
				"error": err.Error(),
			})
			return ids, fmt.Errorf("%w: %v", channels.ErrTemporary, err)
		}
		ids = append(ids, result.MessageID)
		logger.InfoCF("a2a", "outbound sent", map[string]any{
			"agent":        c.agentSlug,
			"peer":         peer,
			"chunk":        i + 1,
			"chunks":       len(chunks),
			"message":      result.MessageID,
			"conversation": result.ConversationID,
			"signed":       result.Signed,
			"sig_state":    result.SigState,
		})
	}
	if len(ids) > 0 && c.maxReplies > 0 && c.replyWindow > 0 {
		c.state.RecordOutbound(peer, c.currentTime(), c.replyWindow)
		if err := c.state.Save(); err != nil {
			logger.WarnCF("a2a", "failed to save rate-limit state", map[string]any{"error": err.Error()})
		}
	}
	return ids, nil
}

func (c *Channel) currentTime() time.Time {
	if c.now == nil {
		return time.Now()
	}
	return c.now()
}

func (c *Channel) isAckOrClosure(text string) bool {
	if c.ackFilter != nil && c.ackFilter.MatchString(text) {
		return true
	}
	for _, filter := range c.customFilters {
		if filter.MatchString(text) {
			return true
		}
	}
	// Short compound closure messages are the ones that caused the incident;
	// treat any short message carrying a closure marker as an acknowledgement.
	if len([]rune(text)) <= 40 {
		for _, marker := range closureMarkers {
			if strings.Contains(text, marker) {
				return true
			}
		}
	}
	return false
}

func suppressedOutboundReason(msg bus.OutboundMessage) string {
	kind := ""
	if msg.Context.Raw != nil {
		kind = strings.TrimSpace(msg.Context.Raw["message_kind"])
	}
	switch {
	case strings.EqualFold(kind, "tool_feedback"):
		return "tool_feedback"
	case strings.EqualFold(kind, "thought"):
		return "thought"
	case strings.EqualFold(kind, "tool_calls"):
		return "tool_calls"
	case strings.EqualFold(kind, "error"):
		return "error"
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(msg.Content)), "error processing message") {
		return "error"
	}
	return ""
}

func previewText(text string, max int) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= max {
		return string(runes)
	}
	return string(runes[:max]) + "…"
}

func (c *Channel) applyReplyPolicy(body string) string {
	policy := strings.TrimSpace(c.replyPolicy)
	if policy == "" {
		return body
	}
	return "【A2A 私信】\n" + policy + "\n\n" + body
}

func (c *Channel) ensureClock(ctx context.Context, strict bool) error {
	c.clockMu.Lock()
	defer c.clockMu.Unlock()
	if time.Since(c.clockCheckedAt) < 5*time.Minute && !c.clockCheckedAt.IsZero() {
		if strict && absDuration(c.clockSkew) > clockFailThreshold {
			return fmt.Errorf("clock skew %s exceeds %s; fix NTP before sending signed messages", c.clockSkew.Round(time.Second), clockFailThreshold)
		}
		return nil
	}
	serverTime, err := c.client.Health(ctx)
	if err != nil {
		// Clock checking is best-effort: never block sending solely because the
		// health endpoint failed. A measured skew above the threshold still
		// fails below.
		logger.WarnCF("a2a", "clock check unavailable", map[string]any{
			"agent": c.agentSlug,
			"error": err.Error(),
		})
		return nil
	}
	c.clockSkew = time.Until(serverTime)
	c.clockCheckedAt = time.Now()
	if absDuration(c.clockSkew) > clockWarnThreshold {
		logger.WarnCF("a2a", "clock skew detected", map[string]any{
			"agent": c.agentSlug,
			"skew":  c.clockSkew.Round(time.Second).String(),
		})
	}
	if strict && absDuration(c.clockSkew) > clockFailThreshold {
		return fmt.Errorf("clock skew %s exceeds %s; fix NTP before sending signed messages", c.clockSkew.Round(time.Second), clockFailThreshold)
	}
	return nil
}

func homeDir() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return home
	}
	return "."
}

func expandHome(path string) string {
	path = strings.TrimSpace(path)
	if path == "~" || strings.HasPrefix(path, "~/") {
		home := homeDir()
		if path == "~" {
			return home
		}
		return filepath.Join(home, path[2:])
	}
	return path
}

func normalizePeer(raw string) string {
	peer := strings.TrimSpace(raw)
	if peer == "" {
		return ""
	}
	if idx := strings.LastIndex(peer, ":agents:"); idx >= 0 {
		peer = peer[idx+len(":agents:"):]
	}
	peer = strings.TrimPrefix(peer, "a2a:")
	return strings.TrimSpace(peer)
}

func splitRunes(content string, max int) []string {
	if max <= 0 {
		max = wire.MaxMessageRunes
	}
	runes := []rune(content)
	if len(runes) <= max {
		return []string{content}
	}
	out := make([]string, 0, (len(runes)+max-1)/max)
	for start := 0; start < len(runes); start += max {
		end := start + max
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[start:end]))
	}
	return out
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
