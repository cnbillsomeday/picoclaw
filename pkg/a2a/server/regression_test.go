// PicoClaw - Ultra-lightweight personal AI agent

package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/a2a/wire"
)

// sendRaw posts a pre-built body so tests can control the exact bytes that
// were signed versus the bytes that are stored.
func (s *testSrv) sendRaw(t *testing.T, from, to string, body any) (int, []byte) {
	t.Helper()
	code, data, _ := s.request(t, http.MethodPost, "/a2a/"+to, s.tokens[from], body)
	return code, data
}

// TestMarkReadOnlyClearsEnumeratedMessages is the regression for the
// clear-unread race: a message that arrives between the client's peek and its
// clear call must stay unread, never be marked read without being delivered.
func TestMarkReadOnlyClearsEnumeratedMessages(t *testing.T) {
	s := newTestSrv(t, "strict", "alpha", "beta")
	seed := s.redeem(t, "alpha")
	signer, err := wire.NewSigner(seed)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}

	var convID string
	for _, content := range []string{"first", "second"} {
		code, data := s.send(t, "alpha", "beta", content, signer)
		if code != http.StatusOK {
			t.Fatalf("send %q: status=%d body=%s", content, code, data)
		}
		var out struct {
			ConversationID string `json:"conversation_id"`
		}
		_ = json.Unmarshal(data, &out)
		convID = out.ConversationID
	}

	// Peek: the client learns which messages exist before clearing.
	code, body, _ := s.request(t, http.MethodGet,
		"/api/v1/a2a/conversations/"+convID+"?peek=1", s.tokens["beta"], nil)
	if code != http.StatusOK {
		t.Fatalf("peek: status=%d body=%s", code, body)
	}
	var detail struct {
		Messages []struct {
			ID   string `json:"id"`
			Body string `json:"body"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if len(detail.Messages) != 2 {
		t.Fatalf("peek messages = %d, want 2", len(detail.Messages))
	}
	enumerated := make([]string, 0, len(detail.Messages))
	for _, m := range detail.Messages {
		enumerated = append(enumerated, m.ID)
	}

	// A third message arrives after the peek: it was never seen by the caller.
	if code, data := s.send(t, "alpha", "beta", "third", signer); code != http.StatusOK {
		t.Fatalf("send third: status=%d body=%s", code, data)
	}

	code, data, _ := s.request(t, http.MethodPost,
		"/api/v1/a2a/conversations/"+convID+"/read", s.tokens["beta"],
		map[string]any{"message_ids": enumerated})
	if code != http.StatusOK {
		t.Fatalf("mark read: status=%d body=%s", code, data)
	}
	var marked struct {
		UnreadCleared int `json:"unread_cleared"`
	}
	_ = json.Unmarshal(data, &marked)
	if marked.UnreadCleared != 2 {
		t.Fatalf("unread_cleared = %d, want 2", marked.UnreadCleared)
	}

	// The late message must still be unread, otherwise it is lost forever.
	code, body, _ = s.request(t, http.MethodGet, "/api/v1/home", s.tokens["beta"], nil)
	if code != http.StatusOK {
		t.Fatalf("home: status=%d", code)
	}
	var home wire.Home
	if err := json.Unmarshal(body, &home); err != nil {
		t.Fatalf("decode home: %v", err)
	}
	if home.UnreadTotal != 1 {
		t.Fatalf("unread_total = %d, want 1 (only the late message)", home.UnreadTotal)
	}
}

func TestMarkReadRequiresExplicitIDs(t *testing.T) {
	s := newTestSrv(t, "strict", "alpha", "beta")
	seed := s.redeem(t, "alpha")
	signer, err := wire.NewSigner(seed)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	code, data := s.send(t, "alpha", "beta", "hello", signer)
	if code != http.StatusOK {
		t.Fatalf("send: status=%d body=%s", code, data)
	}
	var out struct {
		ConversationID string `json:"conversation_id"`
	}
	_ = json.Unmarshal(data, &out)

	code, _, _ = s.request(t, http.MethodPost,
		"/api/v1/a2a/conversations/"+out.ConversationID+"/read", s.tokens["beta"], map[string]any{})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("empty message_ids status = %d, want 422", code)
	}
}

// TestRedeemIsOneTime: a leaked bearer token must not be able to pull the
// platform-held private key over and over.
func TestRedeemIsOneTime(t *testing.T) {
	s := newTestSrv(t, "strict", "alpha")
	_ = s.redeem(t, "alpha")

	code, _, _ := s.request(t, http.MethodPost, "/a2a/keys/redeem", s.tokens["alpha"], "{}")
	if code != http.StatusConflict {
		t.Fatalf("second redeem status = %d, want 409", code)
	}

	// The operator escape hatch still works when it is switched on.
	t.Setenv("A2A_ALLOW_RESEDEEM", "1")
	code, data, _ := s.request(t, http.MethodPost, "/a2a/keys/redeem", s.tokens["alpha"], "{}")
	if code != http.StatusOK {
		t.Fatalf("redeem with A2A_ALLOW_RESEDEEM: status=%d body=%s", code, data)
	}
	var out struct {
		Seed string `json:"seed_base64url"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.Seed == "" {
		t.Fatalf("redeem body has no seed: %v %s", err, data)
	}
}

// TestDefaultSignModeIsStrict: an unsigned message must be refused rather than
// stored and then silently dropped by every client (require_sign defaults on).
func TestDefaultSignModeIsStrict(t *testing.T) {
	t.Setenv("A2A_SIGN_MODE", "")
	s := newTestSrv(t, "", "alpha", "beta")
	code, data := s.send(t, "alpha", "beta", "unsigned", nil)
	if code != http.StatusForbidden {
		t.Fatalf("unsigned send status = %d body=%s, want 403", code, data)
	}
}

// TestTrimmedSignatureRejectedByDefault: appending whitespace to a captured
// message must not keep a valid signature.
func TestTrimmedSignatureRejectedByDefault(t *testing.T) {
	t.Setenv("A2A_ALLOW_TRIMMED_SIG", "")
	s := newTestSrv(t, "strict", "alpha", "beta")
	seed := s.redeem(t, "alpha")
	signer, err := wire.NewSigner(seed)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}

	// Sign the trimmed body but transmit it with surrounding whitespace.
	ts, nonce, sig, err := signer.Sign("alpha", "beta", "hello")
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	body := wire.SendRequest{Content: "  hello  ", Timestamp: ts, Nonce: nonce, Sig: sig}
	code, data := s.sendRaw(t, "alpha", "beta", body)
	if code != http.StatusForbidden {
		t.Fatalf("padded signature status = %d body=%s, want 403", code, data)
	}

	// The documented compatibility opt-in still accepts it.
	t.Setenv("A2A_ALLOW_TRIMMED_SIG", "1")
	ts, nonce, sig, err = signer.Sign("alpha", "beta", "hello")
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	body = wire.SendRequest{Content: "  hello  ", Timestamp: ts, Nonce: nonce, Sig: sig}
	if code, data := s.sendRaw(t, "alpha", "beta", body); code != http.StatusOK {
		t.Fatalf("padded signature with opt-in status = %d body=%s, want 200", code, data)
	}
}

// TestRecipientSlugIsCaseInsensitive: "Alice" must resolve to the allowlisted
// "alice" instead of becoming an unreachable peer.
func TestRecipientSlugIsCaseInsensitive(t *testing.T) {
	s := newTestSrv(t, "strict", "alpha", "beta")
	seed := s.redeem(t, "alpha")
	signer, err := wire.NewSigner(seed)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	ts, nonce, sig, err := signer.Sign("alpha", "Beta", "hello")
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	body := wire.SendRequest{Content: "hello", Timestamp: ts, Nonce: nonce, Sig: sig}
	code, data, _ := s.request(t, http.MethodPost, "/a2a/Beta", s.tokens["alpha"], body)
	if code != http.StatusOK {
		t.Fatalf("case-variant recipient status = %d body=%s", code, data)
	}

	// The message is stored under the canonical slug, so beta's inbox sees it.
	code, homeBody, _ := s.request(t, http.MethodGet, "/api/v1/home", s.tokens["beta"], nil)
	if code != http.StatusOK {
		t.Fatalf("beta home: status=%d", code)
	}
	var home wire.Home
	if err := json.Unmarshal(homeBody, &home); err != nil {
		t.Fatalf("decode home: %v", err)
	}
	if home.UnreadTotal != 1 {
		t.Fatalf("beta unread = %d, want 1", home.UnreadTotal)
	}
}

// TestConversationDetailOrdersByTimestamp makes the derived fields independent
// of store insertion order.
func TestConversationDetailOrdersByTimestamp(t *testing.T) {
	s := newTestSrv(t, "strict", "alpha", "beta")
	seed := s.redeem(t, "alpha")
	signer, err := wire.NewSigner(seed)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	code, data := s.send(t, "alpha", "beta", "first", signer)
	if code != http.StatusOK {
		t.Fatalf("send: status=%d body=%s", code, data)
	}
	var out struct {
		ConversationID string `json:"conversation_id"`
	}
	_ = json.Unmarshal(data, &out)
	if code, data := s.send(t, "beta", "alpha", "reply", signer); code != http.StatusOK {
		_ = data
	}

	code, body, _ := s.request(t, http.MethodGet,
		"/api/v1/a2a/conversations/"+out.ConversationID+"?peek=1", s.tokens["alpha"], nil)
	if code != http.StatusOK {
		t.Fatalf("detail status = %d body=%s", code, body)
	}
	var detail struct {
		Conversation struct {
			YouInitiated bool   `json:"you_initiated"`
			UpdatedAt    string `json:"updated_at"`
		} `json:"conversation"`
	}
	if err := json.Unmarshal(body, &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if !detail.Conversation.YouInitiated {
		t.Fatal("alpha started the conversation but you_initiated is false")
	}
	if detail.Conversation.UpdatedAt == "" {
		t.Fatal("updated_at is empty")
	}
	_ = time.Now()
}
