// PicoClaw - Ultra-lightweight personal AI agent

package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/a2a/wire"
)

func regressionSigner(t *testing.T) *wire.Signer {
	t.Helper()
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i)
	}
	signer, err := wire.NewSigner(seed)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	return signer
}

// TestSendSignedRefreshesTimestampOnRetry is the regression for the retry
// chain that reused one signed payload: the nonce must stay stable (so a
// resend is idempotent) while the timestamp is refreshed (so a slow retry does
// not fall outside the platform's +/-300s window).
func TestSendSignedRefreshesTimestampOnRetry(t *testing.T) {
	var mu sync.Mutex
	var reqs []wire.SendRequest
	attempt := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempt++
		n := attempt
		mu.Unlock()
		var body wire.SendRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		reqs = append(reqs, body)
		mu.Unlock()
		if n == 1 {
			http.Error(w, "temporary", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(wire.SendResponse{
			Success: true, MessageID: "m_1", ConversationID: "c_1", Signed: true, SigState: "ok",
		})
	}))
	defer srv.Close()

	signer := regressionSigner(t)
	res, err := New(srv.URL, "token", 5*time.Second).SendSigned(
		context.Background(), "alpha", "beta", "hello", signer,
		SendOptions{MaxAttempts: 2, BaseBackoff: 1100 * time.Millisecond},
	)
	if err != nil {
		t.Fatalf("SendSigned: %v", err)
	}
	if res == nil || res.MessageID != "m_1" {
		t.Fatalf("result = %+v", res)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(reqs) != 2 {
		t.Fatalf("attempts = %d, want 2", len(reqs))
	}
	if reqs[0].Nonce != reqs[1].Nonce {
		t.Fatalf("nonce changed across retries (%q -> %q): a resend would not be idempotent",
			reqs[0].Nonce, reqs[1].Nonce)
	}
	if reqs[1].Timestamp <= reqs[0].Timestamp {
		t.Fatalf("timestamp was not refreshed on retry (%d -> %d)", reqs[0].Timestamp, reqs[1].Timestamp)
	}
	for i, req := range reqs {
		if !wire.Verify(signer.PublicKey(), "alpha", "beta", req.Timestamp, req.Nonce, req.Content, req.Sig) {
			t.Fatalf("attempt %d signature does not verify", i+1)
		}
	}
}

// TestSendSignedRetriesStaleSignature: when the platform rejects the timestamp
// as stale the client re-signs instead of failing the send outright.
func TestSendSignedRetriesStaleSignature(t *testing.T) {
	var mu sync.Mutex
	attempt := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempt++
		n := attempt
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "error": "stale signature: timestamp outside +/-300s"})
			return
		}
		_ = json.NewEncoder(w).Encode(wire.SendResponse{Success: true, MessageID: "m_2", Signed: true, SigState: "ok"})
	}))
	defer srv.Close()

	res, err := New(srv.URL, "token", 5*time.Second).SendSigned(
		context.Background(), "alpha", "beta", "hello", regressionSigner(t),
		SendOptions{MaxAttempts: 2, BaseBackoff: time.Millisecond},
	)
	if err != nil {
		t.Fatalf("SendSigned: %v", err)
	}
	if res == nil || res.MessageID != "m_2" {
		t.Fatalf("result = %+v", res)
	}
}

func TestSendSignedPropagatesRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "2")
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	_, err := New(srv.URL, "token", 5*time.Second).SendSigned(
		context.Background(), "alpha", "beta", "hello", regressionSigner(t),
		SendOptions{MaxAttempts: 1},
	)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want *APIError", err)
	}
	if apiErr.RetryAfter != 2*time.Second {
		t.Fatalf("RetryAfter = %s, want 2s", apiErr.RetryAfter)
	}
}

func TestMarkReadSendsExplicitIDs(t *testing.T) {
	var mu sync.Mutex
	var gotPath string
	var gotIDs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath = r.URL.Path
		mu.Unlock()
		var body struct {
			MessageIDs []string `json:"message_ids"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		gotIDs = body.MessageIDs
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "unread_cleared": len(body.MessageIDs)})
	}))
	defer srv.Close()

	cleared, err := New(srv.URL, "token", 5*time.Second).MarkRead(
		context.Background(), "c_1", []string{"m_1", "m_2"},
	)
	if err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	if cleared != 2 {
		t.Fatalf("cleared = %d, want 2", cleared)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/api/v1/a2a/conversations/c_1/read" {
		t.Fatalf("path = %q", gotPath)
	}
	if len(gotIDs) != 2 || gotIDs[0] != "m_1" || gotIDs[1] != "m_2" {
		t.Fatalf("message_ids = %v", gotIDs)
	}
}
