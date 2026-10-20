// PicoClaw - Ultra-lightweight personal AI agent

package wire

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// explicitSendTTL bounds how long an explicit send suppresses the automatic
// final reply for the same session/peer.
const explicitSendTTL = 5 * time.Minute

// maxExplicitSends bounds the process-wide map so a busy agent cannot grow it
// without limit. Keys are session-scoped (session keys embed agent and
// channel), so one conversation never suppresses another conversation's reply.
const maxExplicitSends = 1024

type explicitSendKey struct {
	session string
	peer    string
}

var explicitSends = struct {
	sync.Mutex
	m map[explicitSendKey]time.Time
}{m: map[explicitSendKey]time.Time{}}

// pruneExplicitLocked drops expired markers and, if the map is still above the
// cap, the oldest ones. Callers must hold the lock.
func pruneExplicitLocked(now time.Time) {
	for key, at := range explicitSends.m {
		if now.Sub(at) > explicitSendTTL {
			delete(explicitSends.m, key)
		}
	}
	if len(explicitSends.m) <= maxExplicitSends {
		return
	}
	type entry struct {
		key explicitSendKey
		at  time.Time
	}
	entries := make([]entry, 0, len(explicitSends.m))
	for key, at := range explicitSends.m {
		entries = append(entries, entry{key: key, at: at})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].at.Before(entries[j].at) })
	for _, e := range entries[:len(entries)-maxExplicitSends] {
		delete(explicitSends.m, e.key)
	}
}

// MarkExplicitSend records that the agent already sent a message to peer via
// the explicit send path in this session. The next automatic outbound to the
// same peer is suppressed once to avoid a duplicate reply.
func MarkExplicitSend(sessionKey, peer string) {
	sessionKey = strings.TrimSpace(sessionKey)
	peer = NormalizeAgentSlug(peer)
	if sessionKey == "" || peer == "" {
		return
	}
	now := time.Now()
	explicitSends.Lock()
	defer explicitSends.Unlock()
	pruneExplicitLocked(now)
	explicitSends.m[explicitSendKey{session: sessionKey, peer: peer}] = now
}

// ClearExplicitSendsForSession drops any pending explicit-send markers for a
// completed turn so they cannot suppress a later genuine reply.
func ClearExplicitSendsForSession(sessionKey string) {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return
	}
	explicitSends.Lock()
	defer explicitSends.Unlock()
	pruneExplicitLocked(time.Now())
	for key := range explicitSends.m {
		if key.session == sessionKey {
			delete(explicitSends.m, key)
		}
	}
}

// ClearExplicitSendsForPeer drops markers for a peer when the turn result has
// no session key attached.
func ClearExplicitSendsForPeer(peer string) {
	peer = NormalizeAgentSlug(peer)
	if peer == "" {
		return
	}
	explicitSends.Lock()
	defer explicitSends.Unlock()
	pruneExplicitLocked(time.Now())
	for key := range explicitSends.m {
		if key.peer == peer {
			delete(explicitSends.m, key)
		}
	}
}

// ConsumeExplicitSend reports whether an explicit send was recorded for this
// session/peer and removes the marker.
func ConsumeExplicitSend(sessionKey, peer string) bool {
	sessionKey = strings.TrimSpace(sessionKey)
	peer = NormalizeAgentSlug(peer)
	if sessionKey == "" || peer == "" {
		return false
	}
	now := time.Now()
	explicitSends.Lock()
	defer explicitSends.Unlock()
	pruneExplicitLocked(now)
	key := explicitSendKey{session: sessionKey, peer: peer}
	if _, ok := explicitSends.m[key]; !ok {
		return false
	}
	delete(explicitSends.m, key)
	return true
}
