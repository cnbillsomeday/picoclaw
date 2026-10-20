// PicoClaw - Ultra-lightweight personal AI agent

// Package proactive lets a tool hand a proactive message to the live channel
// instead of opening a second, ungated send path.
//
// A proactive message must obey the same gates as an automatic reply: the
// per-peer rate limit, dry-run suppression, signing and the clock-skew check.
// Sending straight from the tool bypassed all of them, so an agent that called
// a2a_send in a loop could flood a peer while its ordinary replies were capped.
package proactive

import (
	"context"
	"reflect"
	"sync"
)

// Sender is implemented by a channel that can send an agent-initiated message
// on demand.
type Sender interface {
	SendProactive(ctx context.Context, peer, text, sessionKey string) ([]string, error)
}

var (
	mu      sync.RWMutex
	senders = map[string]Sender{}
)

// Register publishes a sender under a channel name.
func Register(name string, s Sender) {
	if name == "" || s == nil {
		return
	}
	mu.Lock()
	senders[name] = s
	mu.Unlock()
}

// Unregister removes a sender if it is still the registered one.
func Unregister(name string, s Sender) {
	mu.Lock()
	defer mu.Unlock()
	current, ok := senders[name]
	if !ok {
		return
	}
	if s == nil || sameSender(current, s) {
		delete(senders, name)
	}
}

// sameSender compares two senders without panicking on uncomparable dynamic
// types (a func-typed sender is not comparable with ==).
func sameSender(a, b Sender) bool {
	if a == nil || b == nil {
		return false
	}
	ta, tb := reflect.TypeOf(a), reflect.TypeOf(b)
	if ta != tb {
		return false
	}
	if ta.Comparable() {
		return a == b
	}
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}

// Lookup returns the sender registered for name.
func Lookup(name string) (Sender, bool) {
	mu.RLock()
	defer mu.RUnlock()
	s, ok := senders[name]
	return s, ok
}

// LookupAny returns the sole registered sender. It is the fallback for configs
// whose A2A channel key is not literally "a2a".
func LookupAny() (Sender, bool) {
	mu.RLock()
	defer mu.RUnlock()
	if len(senders) != 1 {
		return nil, false
	}
	for _, s := range senders {
		return s, true
	}
	return nil, false
}
