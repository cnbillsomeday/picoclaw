// PicoClaw - Ultra-lightweight personal AI agent

package a2a

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	stateVersion      = 1
	maxSeenPerConv    = 200
	maxTrackedConvs   = 64
	stateBackupSuffix = ".corrupt."
)

type stateData struct {
	Version    int                 `json:"version"`
	Agent      string              `json:"agent"`
	ETag       string              `json:"etag,omitempty"`
	LastSeen   map[string][]string `json:"last_seen,omitempty"`
	LastTickAt string              `json:"last_tick_at,omitempty"`
	LastOKAt   string              `json:"last_ok_at,omitempty"`
	FailStreak int                 `json:"fail_streak,omitempty"`
	// Outbound tracks recent automatic outbound timestamps per peer (unix
	// seconds) for the per-conversation rate limiter.
	Outbound map[string][]int64 `json:"outbound,omitempty"`
	// Inbound tracks delivery/retry state per conversation and message.
	Inbound map[string]map[string]*inboundEntry `json:"inbound,omitempty"`
}

// inboundEntry is the per-message delivery state. A message is only added to
// last_seen once its agent turn completes (or it is dead-lettered).
type inboundEntry struct {
	Status      string `json:"status"` // pending|failed|dead
	Attempts    int    `json:"attempts"`
	LastAttempt int64  `json:"last_attempt,omitempty"`
	NextAttempt int64  `json:"next_attempt,omitempty"`
	LastError   string `json:"last_error,omitempty"`
	BodyPreview string `json:"body_preview,omitempty"`
}

type channelState struct {
	mu     sync.Mutex
	saveMu sync.Mutex
	path   string
	data   stateData
}

func loadChannelState(path, agent string) (*channelState, error) {
	st := &channelState{path: path, data: stateData{Version: stateVersion, Agent: agent}}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return st, nil
		}
		return nil, err
	}
	var data stateData
	if err := json.Unmarshal(raw, &data); err != nil {
		backup := fmt.Sprintf("%s%s%d", path, stateBackupSuffix, time.Now().Unix())
		_ = os.Rename(path, backup)
		return st, fmt.Errorf("state file was corrupt (backed up to %s): %w", backup, err)
	}
	if data.Agent != "" && data.Agent != agent {
		backup := fmt.Sprintf("%s.agent-mismatch.%d", path, time.Now().Unix())
		_ = os.Rename(path, backup)
		return st, fmt.Errorf(
			"state file belonged to agent %q (backed up to %s); starting fresh for %q",
			data.Agent, backup, agent,
		)
	}
	if data.Version == 0 {
		data.Version = stateVersion
	}
	if data.LastSeen == nil {
		data.LastSeen = map[string][]string{}
	}
	if data.Outbound == nil {
		data.Outbound = map[string][]int64{}
	}
	if data.Inbound == nil {
		data.Inbound = map[string]map[string]*inboundEntry{}
	}
	st.data = data
	st.data.Agent = agent
	return st, nil
}

func (s *channelState) Path() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.path
}

func (s *channelState) Seen(convID, msgID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.data.LastSeen[convID] {
		if id == msgID {
			return true
		}
	}
	return false
}

func (s *channelState) MarkSeen(convID, msgID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addSeenLocked(convID, msgID)
}

func (s *channelState) addSeenLocked(convID, msgID string) {
	if s.data.LastSeen == nil {
		s.data.LastSeen = map[string][]string{}
	}
	ids := append(s.data.LastSeen[convID], msgID)
	if len(ids) > maxSeenPerConv {
		ids = ids[len(ids)-maxSeenPerConv:]
	}
	s.data.LastSeen[convID] = ids
	s.pruneLocked()
}

// ApplyInboundResult atomically applies a turn result if it belongs to the
// current delivery attempt. It returns the resulting status and whether the
// result was applied.
func (s *channelState) ApplyInboundResult(
	convID, msgID string,
	attempt int,
	now time.Time,
	resultStatus, errText string,
	retryAfter time.Duration,
	maxAttempts int,
) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	convs := s.data.Inbound[convID]
	entry := convs[msgID]
	if entry == nil || entry.Attempts != attempt || entry.Status != "pending" {
		return "", false
	}
	status := "completed"
	if resultStatus != "completed" {
		status = "failed"
		if resultStatus == "aborted" || entry.Attempts >= maxAttempts {
			status = "dead"
		}
	}
	switch status {
	case "completed":
		s.addSeenLocked(convID, msgID)
		delete(convs, msgID)
		if len(convs) == 0 {
			delete(s.data.Inbound, convID)
		}
	case "dead":
		s.addSeenLocked(convID, msgID)
		entry.Status = "dead"
		entry.LastAttempt = now.Unix()
		entry.NextAttempt = 0
		entry.LastError = truncateRunes(strings.TrimSpace(errText), 300)
	default:
		entry.Status = "failed"
		entry.LastAttempt = now.Unix()
		entry.NextAttempt = now.Add(retryAfter).Unix()
		entry.LastError = truncateRunes(strings.TrimSpace(errText), 300)
	}
	return status, true
}

func (s *channelState) pruneLocked() {
	if len(s.data.LastSeen) <= maxTrackedConvs {
		return
	}
	// Deterministic: drop lexicographically smallest conversation IDs.
	keys := make([]string, 0, len(s.data.LastSeen))
	for k := range s.data.LastSeen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys[:len(keys)-maxTrackedConvs] {
		delete(s.data.LastSeen, k)
	}
}

// AllowOutbound reports whether the peer has room in the sliding window.
func (s *channelState) AllowOutbound(peer string, now time.Time, window time.Duration, limit int) bool {
	if limit <= 0 || window <= 0 {
		return true
	}
	cutoff := now.Add(-window).Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	times := s.pruneOutboundLocked(peer, cutoff)
	return len(times) < limit
}

// RecordOutbound records a successful automatic outbound message.
func (s *channelState) RecordOutbound(peer string, now time.Time, window time.Duration) {
	if window <= 0 {
		return
	}
	cutoff := now.Add(-window).Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.Outbound == nil {
		s.data.Outbound = map[string][]int64{}
	}
	times := s.pruneOutboundLocked(peer, cutoff)
	s.data.Outbound[peer] = append(times, now.Unix())
}

func (s *channelState) pruneOutboundLocked(peer string, cutoff int64) []int64 {
	times := s.data.Outbound[peer]
	kept := times[:0]
	for _, ts := range times {
		if ts >= cutoff {
			kept = append(kept, ts)
		}
	}
	if len(kept) == 0 {
		delete(s.data.Outbound, peer)
		return nil
	}
	s.data.Outbound[peer] = kept
	return kept
}

// InboundEntry returns a copy of the inbound state for a message.
func (s *channelState) InboundEntry(convID, msgID string) *inboundEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.data.Inbound[convID][msgID]
	if entry == nil {
		return nil
	}
	copy := *entry
	return &copy
}

// MarkInboundPending records a delivery attempt and returns the attempt number.
func (s *channelState) MarkInboundPending(convID, msgID string, now time.Time, preview string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.Inbound == nil {
		s.data.Inbound = map[string]map[string]*inboundEntry{}
	}
	if s.data.Inbound[convID] == nil {
		s.data.Inbound[convID] = map[string]*inboundEntry{}
	}
	entry := s.data.Inbound[convID][msgID]
	if entry == nil {
		entry = &inboundEntry{}
		s.data.Inbound[convID][msgID] = entry
	}
	entry.Status = "pending"
	entry.Attempts++
	entry.LastAttempt = now.Unix()
	entry.NextAttempt = 0
	entry.BodyPreview = truncateRunes(strings.TrimSpace(preview), 200)
	return entry.Attempts
}

// MarkInboundFailed records a failed turn and schedules a retry.
func (s *channelState) MarkInboundFailed(convID, msgID string, now time.Time, retryAfter time.Duration, errText string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.inboundLocked(convID, msgID)
	entry.Status = "failed"
	entry.LastAttempt = now.Unix()
	entry.NextAttempt = now.Add(retryAfter).Unix()
	entry.LastError = truncateRunes(strings.TrimSpace(errText), 300)
}

// MarkInboundDead marks a message as dead-lettered.
func (s *channelState) MarkInboundDead(convID, msgID string, now time.Time, errText string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.inboundLocked(convID, msgID)
	entry.Status = "dead"
	entry.LastAttempt = now.Unix()
	entry.NextAttempt = 0
	entry.LastError = truncateRunes(strings.TrimSpace(errText), 300)
}

func (s *channelState) inboundLocked(convID, msgID string) *inboundEntry {
	if s.data.Inbound == nil {
		s.data.Inbound = map[string]map[string]*inboundEntry{}
	}
	if s.data.Inbound[convID] == nil {
		s.data.Inbound[convID] = map[string]*inboundEntry{}
	}
	entry := s.data.Inbound[convID][msgID]
	if entry == nil {
		entry = &inboundEntry{}
		s.data.Inbound[convID][msgID] = entry
	}
	return entry
}

// ClearInbound removes the retry entry after a success or a dead-letter clear.
func (s *channelState) ClearInbound(convID, msgID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if convs := s.data.Inbound[convID]; convs != nil {
		delete(convs, msgID)
		if len(convs) == 0 {
			delete(s.data.Inbound, convID)
		}
	}
}

// IsInboundDead reports whether a message was dead-lettered.
func (s *channelState) IsInboundDead(convID, msgID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.data.Inbound[convID][msgID]
	return entry != nil && entry.Status == "dead"
}

// HasPendingInbound reports whether any turn result is still pending/retryable.
func (s *channelState) HasPendingInbound() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, convs := range s.data.Inbound {
		for _, entry := range convs {
			if entry != nil && (entry.Status == "pending" || entry.Status == "failed") {
				return true
			}
		}
	}
	return false
}

// HasOtherPendingInbound reports whether another message in the conversation
// is in flight/retryable.
func (s *channelState) HasOtherPendingInbound(convID, exceptMsgID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for msgID, entry := range s.data.Inbound[convID] {
		if msgID == exceptMsgID || entry == nil {
			continue
		}
		if entry.Status == "pending" || entry.Status == "failed" {
			return true
		}
	}
	return false
}

// PendingInboundForConv reports whether a conversation already has an
// in-flight/retryable inbound message.
func (s *channelState) PendingInboundForConv(convID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, entry := range s.data.Inbound[convID] {
		if entry != nil && (entry.Status == "pending" || entry.Status == "failed") {
			return true
		}
	}
	return false
}

// ExpirePendingInbound converts stale pending entries into retryable failures.
func (s *channelState) ExpirePendingInbound(convID string, now time.Time, timeout time.Duration) int {
	if timeout <= 0 {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	expired := 0
	for _, entry := range s.data.Inbound[convID] {
		if entry == nil || entry.Status != "pending" {
			continue
		}
		if entry.LastAttempt > 0 && now.Unix()-entry.LastAttempt >= int64(timeout.Seconds()) {
			entry.Status = "failed"
			entry.NextAttempt = now.Unix()
			entry.LastError = "turn result timeout"
			expired++
		}
	}
	return expired
}

func truncateRunes(text string, max int) string {
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	return string(runes[:max])
}

func (s *channelState) ETag() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.ETag
}

func (s *channelState) SetETag(etag string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if etag != "" {
		s.data.ETag = etag
	}
}

func (s *channelState) SetTick(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.LastTickAt = now.UTC().Format(time.RFC3339)
}

func (s *channelState) SetOK(now time.Time, failStreak int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.LastOKAt = now.UTC().Format(time.RFC3339)
	s.data.FailStreak = failStreak
}

func (s *channelState) SetFail(failStreak int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.FailStreak = failStreak
}

func (s *channelState) Save() error {
	if strings.TrimSpace(s.path) == "" {
		return nil
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.Lock()
	payload, err := json.MarshalIndent(s.data, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".a2a-state-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path)
}
