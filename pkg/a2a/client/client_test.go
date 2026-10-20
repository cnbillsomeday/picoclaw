// PicoClaw - Ultra-lightweight personal AI agent

package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/a2a/wire"
)

func testSigner(t *testing.T) *wire.Signer {
	t.Helper()
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i)
	}
	signer, err := wire.NewSigner(seed)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func TestHome304(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != `W/"abc"` {
			t.Errorf("If-None-Match = %q", r.Header.Get("If-None-Match"))
		}
		w.Header().Set("ETag", `W/"abc"`)
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()

	c := New(srv.URL, "token", 5*time.Second)
	home, etag, notModified, err := c.Home(context.Background(), `W/"abc"`)
	if err != nil {
		t.Fatalf("Home: %v", err)
	}
	if !notModified || home != nil {
		t.Fatalf("notModified=%v home=%v", notModified, home)
	}
	if etag != `W/"abc"` {
		t.Fatalf("etag = %q", etag)
	}
}

func TestSendSignedRetriesReuseExactBody(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(data))
		calls++
		attempt := calls
		mu.Unlock()
		if attempt == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true, "message_id": "m_1", "conversation_id": "c_1",
			"delivered": true, "signed": true, "sig_state": "ok",
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "token", 5*time.Second)
	res, err := c.SendSigned(context.Background(), "fei", "vu", "hello", testSigner(t), SendOptions{
		MaxAttempts: 3,
		BaseBackoff: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("SendSigned: %v", err)
	}
	if res.MessageID != "m_1" {
		t.Fatalf("message id = %q", res.MessageID)
	}
	if len(bodies) != 2 {
		t.Fatalf("attempts = %d, want 2", len(bodies))
	}
	if bodies[0] != bodies[1] {
		t.Fatalf("retry body changed:\n%s\n%s", bodies[0], bodies[1])
	}
}

func TestSendSignedNonceReplayNotRetried(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"success":false,"error":"replayed nonce"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "token", 5*time.Second)
	_, err := c.SendSigned(context.Background(), "fei", "vu", "hello", testSigner(t), SendOptions{
		MaxAttempts: 5,
		BaseBackoff: time.Millisecond,
	})
	if !errors.Is(err, ErrReplayedNonce) {
		t.Fatalf("err = %v, want ErrReplayedNonce", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestSendSignedPermanentErrorNotRetried(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"success":false,"error":"signature verification failed"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "token", 5*time.Second)
	_, err := c.SendSigned(context.Background(), "fei", "vu", "hello", testSigner(t), SendOptions{
		MaxAttempts: 5,
		BaseBackoff: time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (403 must not be retried)", calls)
	}
}

func TestSendSignedRetries429(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "message_id": "m_2"})
	}))
	defer srv.Close()

	c := New(srv.URL, "token", 5*time.Second)
	if _, err := c.SendSigned(context.Background(), "fei", "vu", "hi", testSigner(t), SendOptions{
		MaxAttempts: 2,
		BaseBackoff: time.Millisecond,
	}); err != nil {
		t.Fatalf("SendSigned: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestConversationPathEscapesID(t *testing.T) {
	var gotURI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI = r.RequestURI
		_, _ = w.Write([]byte(`{"success":true,"conversation":{},"messages":[]}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "token", 5*time.Second)
	if _, err := c.Conversation(context.Background(), "c_1/../x", true); err != nil {
		t.Fatalf("Conversation: %v", err)
	}
	if strings.Contains(gotURI, "../") {
		t.Fatalf("path traversal not escaped: %q", gotURI)
	}
}
