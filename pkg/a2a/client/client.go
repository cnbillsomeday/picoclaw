// PicoClaw - Ultra-lightweight personal AI agent

// Package client is the HTTP client for the private A2A messaging platform
// (Bearer token + Ed25519 signed messages). It is not the standard
// a2a-protocol.org protocol.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/a2a/wire"
)

// APIError is a non-2xx platform response.
type APIError struct {
	Status int
	Body   string
	// RetryAfter carries the platform's Retry-After hint (0 when absent), so
	// callers back off as instructed instead of hammering a rate-limited
	// endpoint with their own exponential schedule.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	body := strings.TrimSpace(e.Body)
	if len(body) > 512 {
		body = body[:512] + "…"
	}
	return fmt.Sprintf("a2a API error: status=%d body=%s", e.Status, body)
}

// ErrReplayedNonce means the platform consumed this nonce before: a retry of an
// ambiguous send most likely arrived the first time. Callers must not retry
// with a new nonce (that would deliver the message twice).
var ErrReplayedNonce = errors.New("a2a: nonce already used (message may have been delivered)")

// Client talks to one A2A platform base URL with one agent token.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New creates a client. baseURL may include a path prefix (e.g. behind a
// reverse proxy) and a trailing slash is ignored.
func New(baseURL, token string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	return &Client{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:   token,
		http:    &http.Client{Timeout: timeout},
	}
}

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "picoclaw-a2a-client/1")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (c *Client) do(req *http.Request, out any) (*http.Response, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if readErr != nil {
		return resp, readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp, &APIError{
			Status:     resp.StatusCode,
			Body:       string(body),
			RetryAfter: retryAfterDuration(resp),
		}
	}
	if out != nil && len(body) > 0 {
		if err := json.Unmarshal(body, out); err != nil {
			return resp, fmt.Errorf("decode response: %w", err)
		}
	}
	return resp, nil
}

// Home polls the inbox. A 304 response returns (nil, etag, true, nil).
func (c *Client) Home(ctx context.Context, etag string) (*wire.Home, string, bool, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/v1/home", nil)
	if err != nil {
		return nil, etag, false, err
	}
	if strings.TrimSpace(etag) != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, etag, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		next := resp.Header.Get("ETag")
		if next == "" {
			next = etag
		}
		return nil, next, true, nil
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if readErr != nil {
		return nil, etag, false, readErr
	}
	if resp.StatusCode != http.StatusOK {
		return nil, etag, false, &APIError{Status: resp.StatusCode, Body: string(body)}
	}
	var home wire.Home
	if err := json.Unmarshal(body, &home); err != nil {
		return nil, etag, false, fmt.Errorf("decode home: %w", err)
	}
	next := resp.Header.Get("ETag")
	if next == "" {
		next = etag
	}
	return &home, next, false, nil
}

// Conversations lists conversations.
func (c *Client) Conversations(ctx context.Context) ([]wire.Conversation, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/v1/a2a/conversations", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Conversations []wire.Conversation `json:"conversations"`
	}
	if _, err := c.do(req, &out); err != nil {
		return nil, err
	}
	return out.Conversations, nil
}

// Conversation fetches one conversation. peek=true leaves unread state alone;
// peek=false marks the caller's inbound messages as read.
func (c *Client) Conversation(ctx context.Context, id string, peek bool) (*wire.ConversationDetail, error) {
	path := "/api/v1/a2a/conversations/" + url.PathEscape(id)
	if peek {
		path += "?peek=1"
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var out wire.ConversationDetail
	if _, err := c.do(req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Agents lists allowlisted agents.
func (c *Client) Agents(ctx context.Context) ([]wire.Agent, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/v1/agents", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Agents []wire.Agent `json:"agents"`
	}
	if _, err := c.do(req, &out); err != nil {
		return nil, err
	}
	return out.Agents, nil
}

// MarkRead marks exactly the enumerated inbound message IDs as read.
//
// Clearing by ID (instead of "clear the whole conversation") is what keeps a
// message that arrives between the client's peek and this call unread, so the
// next poll still delivers it.
func (c *Client) MarkRead(ctx context.Context, conversationID string, messageIDs []string) (int, error) {
	if strings.TrimSpace(conversationID) == "" || len(messageIDs) == 0 {
		return 0, nil
	}
	payload, err := json.Marshal(map[string]any{"message_ids": messageIDs})
	if err != nil {
		return 0, err
	}
	path := "/api/v1/a2a/conversations/" + url.PathEscape(conversationID) + "/read"
	req, err := c.newRequest(ctx, http.MethodPost, path, bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	var out struct {
		UnreadCleared int `json:"unread_cleared"`
	}
	if _, err := c.do(req, &out); err != nil {
		return 0, err
	}
	return out.UnreadCleared, nil
}

// Health returns the platform's clock (UTC), used for skew detection.
func (c *Client) Health(ctx context.Context) (time.Time, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/healthz", nil)
	if err != nil {
		return time.Time{}, err
	}
	var out struct {
		Time string `json:"time"`
	}
	if _, err := c.do(req, &out); err != nil {
		return time.Time{}, err
	}
	if out.Time != "" {
		if ts, err := time.Parse(time.RFC3339, out.Time); err == nil {
			return ts.UTC(), nil
		}
	}
	return time.Time{}, errors.New("a2a: health response has no usable time")
}

// SendResult is the outcome of one signed send attempt.
type SendResult struct {
	MessageID      string
	ConversationID string
	Signed         bool
	SigState       string
}

// SendOptions tunes the outbound retry policy.
type SendOptions struct {
	MaxAttempts int
	BaseBackoff time.Duration
}

// SendSigned signs and sends content as one message, retrying transient
// failures.
//
// The nonce is generated once and reused across attempts: the platform
// consumes nonces, so a resend either lands exactly once or answers
// ErrReplayedNonce (meaning an earlier attempt did land). The timestamp and
// signature are refreshed on every attempt, because a retry chain with backoff
// would otherwise drift past the platform's +/-300s acceptance window and be
// rejected as stale each time.
func (c *Client) SendSigned(
	ctx context.Context,
	from, to, content string,
	signer *wire.Signer,
	opts SendOptions,
) (*SendResult, error) {
	if signer == nil {
		return nil, errors.New("a2a: signer is required")
	}
	attempts := opts.MaxAttempts
	if attempts <= 0 {
		attempts = 5
	}
	backoff := opts.BaseBackoff
	if backoff <= 0 {
		backoff = time.Second
	}
	nonce, err := wire.NewNonce()
	if err != nil {
		return nil, err
	}

	var lastErr error
	var lastRetryAfter time.Duration
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			delay := backoff * time.Duration(1<<(attempt-1))
			if delay > 15*time.Second {
				delay = 15 * time.Second
			}
			if lastRetryAfter > delay {
				delay = lastRetryAfter
			}
			if err := sleepCtx(ctx, delay); err != nil {
				return nil, err
			}
		}
		lastRetryAfter = 0
		ts, sig, err := signer.SignWithNonce(from, to, content, nonce)
		if err != nil {
			return nil, err
		}
		payload, err := json.Marshal(wire.SendRequest{Content: content, Timestamp: ts, Nonce: nonce, Sig: sig})
		if err != nil {
			return nil, err
		}
		result, retry, retryAfter, err := c.sendOnce(ctx, to, payload)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if errors.Is(err, ErrReplayedNonce) {
			return nil, err
		}
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			switch {
			case apiErr.Status == http.StatusTooManyRequests:
				lastRetryAfter = retryAfter
				continue
			case apiErr.Status >= 500:
				continue
			case apiErr.Status == http.StatusForbidden && isStaleSignature(apiErr.Body):
				// The clock moved past the platform window (long backoff or a
				// fresh NTP step); re-sign with a new timestamp and retry.
				continue
			default:
				return nil, err
			}
		}
		if !retry {
			return nil, err
		}
	}
	return nil, lastErr
}

func isStaleSignature(body string) bool {
	return strings.Contains(strings.ToLower(body), "stale signature")
}

func (c *Client) sendOnce(ctx context.Context, to string, payload []byte) (*SendResult, bool, time.Duration, error) {
	path := "/a2a/" + url.PathEscape(to)
	req, err := c.newRequest(ctx, http.MethodPost, path, bytes.NewReader(payload))
	if err != nil {
		return nil, false, 0, err
	}
	var out wire.SendResponse
	if _, err := c.do(req, &out); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			if apiErr.Status == http.StatusForbidden && strings.Contains(apiErr.Body, "replayed nonce") {
				return nil, false, 0, ErrReplayedNonce
			}
			return nil, false, apiErr.RetryAfter, err
		}
		// Network errors and timeouts are retryable: the request may not have
		// reached the platform at all.
		return nil, true, 0, err
	}
	return &SendResult{
		MessageID:      out.MessageID,
		ConversationID: out.ConversationID,
		Signed:         out.Signed,
		SigState:       out.SigState,
	}, false, 0, nil
}

func retryAfterDuration(resp *http.Response) time.Duration {
	if resp == nil {
		return 0
	}
	raw := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if raw == "" {
		return 0
	}
	if secs, err := strconv.Atoi(raw); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if ts, err := http.ParseTime(raw); err == nil {
		if d := time.Until(ts); d > 0 {
			return d
		}
	}
	return 0
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
