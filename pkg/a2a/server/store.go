package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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

// Message is one private message. Read is meaningful for the recipient (To).
type Message struct {
	ID        string    `json:"id"`
	ConvID    string    `json:"conversation_id"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	Read      bool      `json:"read"`
	Signed    bool      `json:"signed,omitempty"`
	SigState  string    `json:"sig_state,omitempty"`
}

// diskState is the on-disk representation. Agents come from allowlist.json,
// only messages need persistence.
type diskState struct {
	Version  int        `json:"version"`
	Messages []*Message `json:"messages"`
}

// Store is a tiny JSON-backed message store guarded by one mutex.
// Volume is deliberately small (8 agents, 24h retention), so full rewrites
// are cheap and keep the implementation dependency-free.
type Store struct {
	mu       sync.Mutex
	path     string
	messages []*Message
	retain   time.Duration
}

func NewStore(path string, retain time.Duration) (*Store, error) {
	s := &Store{path: path, retain: retain}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, err
	}
	var st diskState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("data file corrupted: %w", err)
	}
	s.messages = st.Messages
	s.gcLocked(time.Now())
	return s, nil
}

func (s *Store) saveLocked() error {
	st := diskState{Version: 1, Messages: s.messages}
	if st.Messages == nil {
		st.Messages = []*Message{}
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// gcLocked drops messages older than the retention window.
func (s *Store) gcLocked(now time.Time) int {
	if s.retain <= 0 {
		return 0
	}
	cutoff := now.Add(-s.retain)
	kept := s.messages[:0]
	removed := 0
	for _, m := range s.messages {
		if m.CreatedAt.Before(cutoff) {
			removed++
			continue
		}
		kept = append(kept, m)
	}
	s.messages = kept
	return removed
}

func (s *Store) GC() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.gcLocked(time.Now())
	if n == 0 {
		return 0, nil
	}
	return n, s.saveLocked()
}

func randID(prefix string, n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is fatal for tokens but not for an ID; fall back
		// to the nanosecond clock (still unique enough for this workload).
		return fmt.Sprintf("%s%d", prefix, time.Now().UnixNano())
	}
	return prefix + hex.EncodeToString(b)
}

// ConversationID is deterministic for a pair of agents so both sides agree.
func ConversationID(a, b string) string {
	if a > b {
		a, b = b, a
	}
	sum := sha256.Sum256([]byte(a + "\x00" + b))
	return "c_" + hex.EncodeToString(sum[:8])
}

func convMessages(msgs []*Message, id string) []*Message {
	out := make([]*Message, 0, 8)
	for _, m := range msgs {
		if m.ConvID == id {
			out = append(out, m)
		}
	}
	return out
}

func (s *Store) AddMessage(from, to, body string) (*Message, error) {
	return s.AddMessageSigned(from, to, body, false, "")
}

// AddMessageSigned stores a message together with its signature verdict.
func (s *Store) AddMessageSigned(from, to, body string, signed bool, sigState string) (*Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	s.gcLocked(now)
	m := &Message{
		ID:        randID("m_", 8),
		ConvID:    ConversationID(from, to),
		From:      from,
		To:        to,
		Body:      body,
		CreatedAt: now,
		Signed:    signed,
		SigState:  sigState,
	}
	s.messages = append(s.messages, m)
	if err := s.saveLocked(); err != nil {
		s.messages = s.messages[:len(s.messages)-1]
		return nil, err
	}
	return m, nil
}

func (s *Store) messagesFor(agent string) []*Message {
	out := make([]*Message, 0, 16)
	for _, m := range s.messages {
		if m.From == agent || m.To == agent {
			out = append(out, m)
		}
	}
	return out
}

// Conversation is an internal aggregate.
type Conversation struct {
	ID           string
	Other        string
	Messages     []*Message
	Unread       int
	YouInitiated bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
	Last         *Message
}

func (s *Store) Conversations(agent string) []*Conversation {
	s.mu.Lock()
	defer s.mu.Unlock()
	byID := map[string]*Conversation{}
	for _, m := range s.messagesFor(agent) {
		c := byID[m.ConvID]
		if c == nil {
			other := m.From
			if other == agent {
				other = m.To
			}
			c = &Conversation{ID: m.ConvID, Other: other, CreatedAt: m.CreatedAt}
			byID[m.ConvID] = c
		}
		c.Messages = append(c.Messages, m)
		if m.To == agent && !m.Read {
			c.Unread++
		}
		c.UpdatedAt = m.CreatedAt
		c.Last = m
	}
	out := make([]*Conversation, 0, len(byID))
	for _, c := range byID {
		c.YouInitiated = len(c.Messages) > 0 && c.Messages[0].From == agent
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out
}

func (s *Store) UnreadTotal(agent string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, m := range s.messages {
		if m.To == agent && !m.Read {
			n++
		}
	}
	return n
}

// Conversation returns a copy of messages for a conversation and, unless peek,
// marks inbound messages as read.
func (s *Store) Conversation(agent, id string, peek bool) ([]*Message, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Message
	participant := false
	for _, m := range s.messages {
		if m.ConvID != id {
			continue
		}
		if m.From == agent || m.To == agent {
			participant = true
		}
		cp := *m
		out = append(out, &cp)
	}
	if len(out) == 0 || !participant {
		return nil, 0, os.ErrNotExist
	}
	cleared := 0
	if !peek {
		for _, m := range s.messages {
			if m.ConvID == id && m.To == agent && !m.Read {
				m.Read = true
				cleared++
			}
		}
		if cleared > 0 {
			if err := s.saveLocked(); err != nil {
				return out, 0, err
			}
		}
	}
	// reflect read state in the copy
	if !peek {
		for i, m := range out {
			if m.To == agent {
				out[i].Read = true
			}
		}
	}
	return out, cleared, nil
}

// MarkRead marks only the enumerated inbound message IDs as read for the
// agent. Messages the caller did not enumerate are left untouched, so a
// message that arrives between the client's peek and this call stays unread
// and is delivered on the next poll instead of being silently swallowed.
func (s *Store) MarkRead(agent, id string, msgIDs []string) (int, error) {
	if agent == "" || id == "" || len(msgIDs) == 0 {
		return 0, nil
	}
	want := make(map[string]struct{}, len(msgIDs))
	for _, mid := range msgIDs {
		if mid = strings.TrimSpace(mid); mid != "" {
			want[mid] = struct{}{}
		}
	}
	if len(want) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cleared := 0
	for _, m := range s.messages {
		if m.ConvID != id || m.To != agent || m.Read {
			continue
		}
		if _, ok := want[m.ID]; !ok {
			continue
		}
		m.Read = true
		cleared++
	}
	if cleared == 0 {
		return 0, nil
	}
	if err := s.saveLocked(); err != nil {
		return 0, err
	}
	return cleared, nil
}

func (s *Store) Stats() (messages int, oldest, newest time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	messages = len(s.messages)
	for _, m := range s.messages {
		if oldest.IsZero() || m.CreatedAt.Before(oldest) {
			oldest = m.CreatedAt
		}
		if m.CreatedAt.After(newest) {
			newest = m.CreatedAt
		}
	}
	return
}
