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
}

type channelState struct {
	mu   sync.Mutex
	path string
	data stateData
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
	s.mu.Lock()
	data := s.data
	s.mu.Unlock()

	if strings.TrimSpace(s.path) == "" {
		return nil
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
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
