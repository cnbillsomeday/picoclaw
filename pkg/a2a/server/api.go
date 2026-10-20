package server

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	Version         = "1.0.0"
	maxMessageRunes = 8000
	maxBodyBytes    = 64 << 10
)

type Server struct {
	cfg      *Config
	store    *Store
	started  time.Time
	log      *log.Logger
	nonces   *nonceCache
	presence *presenceStore
	signMode string
}

func NewServer(cfg *Config, store *Store, logger *log.Logger, signModeOverride string) *Server {
	ttl := 10 * time.Minute
	if v := os.Getenv("A2A_NONCE_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			ttl = d
		}
	}
	return &Server{
		cfg:      cfg,
		store:    store,
		started:  time.Now(),
		log:      logger,
		nonces:   newNonceCache(ttl),
		presence: newPresenceStore(cfg.Dir),
		signMode: effectiveSignMode(signModeOverride),
	}
}

// ---------- views ----------

type AgentView struct {
	ID             string `json:"id"`
	AgentID        string `json:"agent_id"`
	Slug           string `json:"slug"`
	Name           string `json:"name"`
	DisplayName    string `json:"display_name"`
	DID            string `json:"did"`
	AvatarURL      string `json:"avatar_url"`
	Bio            string `json:"bio"`
	Karma          int    `json:"karma"`
	PostCount      int    `json:"post_count"`
	CommentCount   int    `json:"comment_count"`
	FollowerCount  int    `json:"follower_count"`
	FollowingCount int    `json:"following_count"`
	CreatedAt      string `json:"created_at"`
}

func (c *Config) agentView(slug string) AgentView {
	a := c.BySlug[slug]
	if a == nil {
		return AgentView{}
	}
	return AgentView{
		ID:          a.Slug,
		AgentID:     a.Slug,
		Slug:        a.Slug,
		Name:        a.Name,
		DisplayName: a.DisplayName,
		DID:         c.DID(a.Slug),
		AvatarURL:   "",
		CreatedAt:   a.CreatedAt.UTC().Format(time.RFC3339),
	}
}

type MessageView struct {
	ID             string `json:"id"`
	ConversationID string `json:"conversation_id"`
	From           string `json:"from"`
	FromAgent      string `json:"from_agent"`
	To             string `json:"to"`
	ToAgent        string `json:"to_agent"`
	Body           string `json:"body"`
	Content        string `json:"content"`
	Text           string `json:"text"`
	CreatedAt      string `json:"created_at"`
	Signed         bool   `json:"signed"`
	SigState       string `json:"sig_state,omitempty"`
	Read           bool   `json:"read"`
}

func messageView(m *Message) MessageView {
	ts := m.CreatedAt.UTC().Format(time.RFC3339)
	return MessageView{
		ID:             m.ID,
		ConversationID: m.ConvID,
		From:           m.From,
		FromAgent:      m.From,
		To:             m.To,
		ToAgent:        m.To,
		Body:           m.Body,
		Content:        m.Body,
		Text:           m.Body,
		CreatedAt:      ts,
		Read:           m.Read,
		Signed:         m.Signed,
		SigState:       m.SigState,
	}
}

type SummaryView struct {
	LastMessage   string `json:"last_message"`
	LastMessageAt string `json:"last_message_at"`
	MessageCount  int    `json:"message_count"`
	UnreadCount   int    `json:"unread_count"`
}

type ConversationView struct {
	ID           string        `json:"id"`
	WithAgent    AgentView     `json:"with_agent"`
	Status       string        `json:"status"`
	YouInitiated bool          `json:"you_initiated"`
	UnreadCount  int           `json:"unread_count"`
	MessageCount int           `json:"message_count"`
	CreatedAt    string        `json:"created_at"`
	UpdatedAt    string        `json:"updated_at"`
	LastMessage  *MessageView  `json:"last_message,omitempty"`
	Summary      SummaryView   `json:"summary"`
	Messages     []MessageView `json:"messages,omitempty"`
}

func (s *Server) convView(agent string, c *Conversation, withMessages bool) ConversationView {
	v := ConversationView{
		ID:           c.ID,
		WithAgent:    s.cfg.agentView(c.Other),
		Status:       "active",
		YouInitiated: c.YouInitiated,
		UnreadCount:  c.Unread,
		MessageCount: len(c.Messages),
		CreatedAt:    c.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:    c.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if c.Last != nil {
		lm := messageView(c.Last)
		v.LastMessage = &lm
		v.Summary.LastMessage = c.Last.Body
		v.Summary.LastMessageAt = lm.CreatedAt
	}
	v.Summary.MessageCount = len(c.Messages)
	v.Summary.UnreadCount = c.Unread
	if withMessages {
		v.Messages = make([]MessageView, 0, len(c.Messages))
		for _, m := range c.Messages {
			v.Messages = append(v.Messages, messageView(m))
		}
	}
	return v
}

type NotificationsView struct {
	UnreadTotal    int   `json:"unread_total"`
	UnreadMessages int   `json:"unread_messages"`
	UnreadPosts    int   `json:"unread_posts"`
	UnreadComments int   `json:"unread_comments"`
	UnreadFollows  int   `json:"unread_follows"`
	Items          []any `json:"items"`
}

type HomeView struct {
	Success       bool               `json:"success"`
	Agent         AgentView          `json:"agent"`
	Notifications NotificationsView  `json:"notifications"`
	NewPosts      []any              `json:"new_posts"`
	Conversations []ConversationView `json:"conversations"`
	UnreadTotal   int                `json:"unread_total"`
	ServerTime    string             `json:"server_time,omitempty"`
}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"success": false, "error": msg})
}

type authHandler func(http.ResponseWriter, *http.Request, string)

func (s *Server) auth(h authHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		tok := ""
		if h := r.Header.Get("Authorization"); h != "" {
			parts := strings.SplitN(h, " ", 2)
			if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
				tok = strings.TrimSpace(parts[1])
			}
		}
		if tok == "" {
			tok = strings.TrimSpace(r.Header.Get("X-API-Key"))
		}
		slug, ok := s.cfg.Auth(tok)
		if ok {
			s.presence.Touch(slug) // 每个带 token 的请求都算一次心跳
		}
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="a2a"`)
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		h(w, r, slug)
	}
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: 200}
		next.ServeHTTP(sw, r)
		// Never log Authorization headers or bodies.
		s.log.Printf("%s %s %d %s", r.Method, r.URL.Path, sw.code, time.Since(start).Round(time.Millisecond))
	})
}

// ---------- handlers ----------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"service":        "a2a",
		"version":        Version,
		"domain":         s.cfg.Domain,
		"uptime_seconds": int(time.Since(s.started).Seconds()),
		"time":           time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"service": "a2a",
		"version": Version,
		"domain":  s.cfg.Domain,
		"did":     "did:web:" + s.cfg.Domain,
		"endpoints": map[string]string{
			"health":        "/healthz",
			"home":          "/api/v1/home",
			"send":          "/a2a/{agent}",
			"conversations": "/api/v1/a2a/conversations",
			"agents":        "/api/v1/agents",
			"did":           "/.well-known/did.json",
			"heartbeat":     "/heartbeat.md",
		},
		"auth": "Authorization: Bearer <token>",
	})
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request, slug string) {
	// Lazy retention sweep keeps the 24h guarantee independent of the ticker.
	_, _ = s.store.GC()
	unread := s.store.UnreadTotal(slug)
	convs := s.store.Conversations(slug)
	views := make([]ConversationView, 0, len(convs))
	for _, c := range convs {
		views = append(views, s.convView(slug, c, false))
	}
	payload := HomeView{
		Success: true,
		Agent:   s.cfg.agentView(slug),
		Notifications: NotificationsView{
			UnreadTotal:    unread,
			UnreadMessages: unread,
			Items:          []any{},
		},
		NewPosts:      []any{},
		Conversations: views,
		UnreadTotal:   unread,
	}
	// ETag over content excluding server_time, so polling clients get 304.
	hashInput := payload
	hashInput.ServerTime = ""
	b, _ := json.Marshal(hashInput)
	sum := sha256.Sum256(b)
	etag := fmt.Sprintf(`W/"%x"`, sum[:12])
	w.Header().Set("ETag", etag)
	if inm := r.Header.Get("If-None-Match"); inm != "" {
		for _, cand := range strings.Split(inm, ",") {
			if strings.TrimSpace(cand) == etag || strings.TrimSpace(cand) == "*" {
				w.Header().Set("Cache-Control", "no-store")
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
	}
	payload.ServerTime = time.Now().UTC().Format(time.RFC3339)
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request, slug string) {
	writeJSON(w, http.StatusOK, map[string]any{
		"success":      true,
		"agent":        s.cfg.agentView(slug),
		"unread_total": s.store.UnreadTotal(slug),
		"did_document": s.cfg.BaseURL + "/agents/" + slug + "/did.json",
		"agent_card":   s.cfg.BaseURL + "/agents/" + slug + "/agent-card.json",
	})
}

func (s *Server) handleConversations(w http.ResponseWriter, r *http.Request, slug string) {
	convs := s.store.Conversations(slug)
	views := make([]ConversationView, 0, len(convs))
	for _, c := range convs {
		views = append(views, s.convView(slug, c, false))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":       true,
		"conversations": views,
		"unread_total":  s.store.UnreadTotal(slug),
	})
}

func (s *Server) handleConversation(w http.ResponseWriter, r *http.Request, slug string) {
	id := r.PathValue("id")
	peek := r.URL.Query().Get("peek") == "1"
	msgs, cleared, err := s.store.Conversation(slug, id, peek)
	if err != nil {
		writeErr(w, http.StatusNotFound, "conversation not found")
		return
	}
	// Do not trust store order: order by timestamp so YouInitiated/Other are
	// derived from the actual first message rather than an insertion accident.
	sorted := make([]*Message, len(msgs))
	copy(sorted, msgs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].CreatedAt.Before(sorted[j].CreatedAt) })
	first := sorted[0]
	conv := &Conversation{ID: id, CreatedAt: first.CreatedAt, UpdatedAt: first.CreatedAt, Last: first}
	conv.Other = first.From
	if conv.Other == slug {
		conv.Other = first.To
	}
	if conv.Other == slug || conv.Other == "" {
		// Self-thread or malformed first message: fall back to any counterpart.
		for _, m := range sorted {
			if m.From != slug {
				conv.Other = m.From
				break
			}
			if m.To != slug && m.To != "" {
				conv.Other = m.To
				break
			}
		}
	}
	conv.Messages = sorted
	for _, m := range sorted {
		if m.To == slug && !m.Read {
			conv.Unread++
		}
		if m.CreatedAt.After(conv.UpdatedAt) {
			conv.UpdatedAt = m.CreatedAt
		}
		conv.Last = m
	}
	conv.YouInitiated = first.From == slug
	view := s.convView(slug, conv, true)
	// In the detail response messages reflect the read state the client now has.
	if !peek {
		for i := range view.Messages {
			if view.Messages[i].To == slug {
				view.Messages[i].Read = true
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":        true,
		"conversation":   view,
		"messages":       view.Messages,
		"unread_cleared": cleared,
	})
}

// POST /api/v1/a2a/conversations/{id}/read — mark exactly these messages read.
//
// The client must enumerate the IDs it has already settled. A blanket "mark
// everything read" call is deliberately not offered: it races with new inbound
// messages and can mark an undelivered message as read.
func (s *Server) handleMarkRead(w http.ResponseWriter, r *http.Request, slug string) {
	id := r.PathValue("id")
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var in struct {
		MessageIDs []string `json:"message_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(in.MessageIDs) == 0 {
		writeErr(w, http.StatusUnprocessableEntity, "message_ids must enumerate the messages to mark read")
		return
	}
	cleared, err := s.store.MarkRead(slug, id, in.MessageIDs)
	if err != nil {
		s.log.Printf("mark read error: %v", err)
		writeErr(w, http.StatusInternalServerError, "storage error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "unread_cleared": cleared})
}

func normalizeRecipient(name, domain string) string {
	name = strings.TrimSpace(name)
	prefix := "did:web:" + domain + ":agents:"
	if strings.HasPrefix(name, prefix) {
		return strings.TrimPrefix(name, prefix)
	}
	return name
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request, slug string) {
	name := normalizeRecipient(r.PathValue("name"), s.cfg.Domain)
	if name == "" || strings.ContainsAny(name, "/: ") {
		writeErr(w, http.StatusForbidden, "recipient not in allowlist")
		return
	}
	recipient := s.cfg.ResolveAgent(name)
	if recipient == nil {
		writeErr(w, http.StatusForbidden, "recipient not in allowlist")
		return
	}
	// The peer signed the slug it typed; verify against that exact string and
	// store under the canonical allowlist slug so casing variants share one
	// conversation.
	verifyTo := name
	name = recipient.Slug
	ct := r.Header.Get("Content-Type")
	if ct == "" || !strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "application/json") {
		writeErr(w, http.StatusUnsupportedMediaType, "Content-Type: application/json is required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var in struct {
		Message   string `json:"message"`
		Content   string `json:"content"`
		Text      string `json:"text"`
		Body      string `json:"body"`
		Timestamp int64  `json:"timestamp"`
		Nonce     string `json:"nonce"`
		Sig       string `json:"sig"`
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&in); err != nil {
		if err == io.EOF {
			writeErr(w, http.StatusUnprocessableEntity, "empty request body")
			return
		}
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	body := in.Message
	if strings.TrimSpace(body) == "" {
		body = in.Content
	}
	if strings.TrimSpace(body) == "" {
		body = in.Text
	}
	if strings.TrimSpace(body) == "" {
		body = in.Body
	}
	// 验签必须用客户端签名时的**原始字节**：客户端对未 trim 的正文算 sha256。
	// 早前这里先 TrimSpace 再验签，导致正文首尾带空白/换行的请求在 strict 下必 403
	// （对端只要正文尾带一个换行就发不进来，且报错看起来像密钥/实现问题）。— Carol(carol) 2026-10-18 报
	rawBody := body
	body = strings.TrimSpace(body)
	if body == "" {
		writeErr(w, http.StatusUnprocessableEntity, "message is empty")
		return
	}
	if utf8.RuneCountInString(body) > maxMessageRunes {
		writeErr(w, http.StatusUnprocessableEntity, "message too long: max 8000 characters")
		return
	}
	// ---- 方案二：Ed25519 验签（有签必验；无签看 SIGN_MODE）----
	mode := s.signMode
	signed, sigState := false, "unsigned"
	if strings.TrimSpace(in.Sig) != "" {
		if in.Nonce == "" || in.Timestamp == 0 {
			s.log.Printf("send: slug=%s to=%s rejected: sig without timestamp/nonce", slug, name)
			writeErr(w, http.StatusUnprocessableEntity, "sig requires timestamp and nonce")
			return
		}
		skew := time.Since(time.Unix(in.Timestamp, 0))
		if skew < -300*time.Second || skew > 300*time.Second {
			s.log.Printf("send: slug=%s rejected: stale signature skew=%s", slug, skew.Truncate(time.Second))
			writeErr(w, http.StatusForbidden, "stale signature: timestamp outside +/-300s")
			return
		}
		if !s.nonces.Use(slug + "|" + in.Nonce) {
			s.log.Printf("send: slug=%s rejected: nonce replay", slug)
			writeErr(w, http.StatusForbidden, "replayed nonce")
			return
		}
		ok, reason := s.cfg.VerifyMessage(slug, verifyTo, rawBody, in.Timestamp, in.Nonce, in.Sig)
		if !ok && rawBody != body && allowTrimmedSignature() {
			// Compatibility escape hatch for peers that sign a trimmed body.
			// Off by default: otherwise anyone can append whitespace to a
			// captured message and keep a valid signature.
			if ok2, _ := s.cfg.VerifyMessage(slug, verifyTo, body, in.Timestamp, in.Nonce, in.Sig); ok2 {
				ok, reason = true, "ok-trimmed"
				s.log.Printf("send: slug=%s verified only after trim (client signed trimmed body)", slug)
			}
		}
		sigState = reason
		signed = ok
		if !ok {
			s.log.Printf("send: slug=%s to=%s sig FAILED reason=%s mode=%s", slug, name, reason, mode)
			if mode == "strict" {
				writeErr(w, http.StatusForbidden, "signature verification failed: "+reason)
				return
			}
		} else {
			s.log.Printf("send: slug=%s to=%s sig OK", slug, name)
		}
	} else {
		if mode == "strict" {
			s.log.Printf("send: slug=%s to=%s rejected: unsigned (strict)", slug, name)
			writeErr(w, http.StatusForbidden, "signature required (platform strict-sign): set timestamp/nonce/sig")
			return
		}
	}

	m, err := s.store.AddMessageSigned(slug, name, body, signed, sigState)
	if err != nil {
		s.log.Printf("store error: %v", err)
		writeErr(w, http.StatusInternalServerError, "storage error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":         true,
		"message_id":      m.ID,
		"conversation_id": m.ConvID,
		"from":            slug,
		"to":              name,
		"delivered":       true,
		"signed":          signed,
		"sig_state":       sigState,
		"sign_mode":       mode,
		"created_at":      m.CreatedAt.UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	agents := make([]any, 0, len(s.cfg.Agents))
	for _, a := range s.cfg.Agents {
		agents = append(agents, map[string]any{
			"id":           a.Slug,
			"slug":         a.Slug,
			"name":         a.Name,
			"display_name": a.DisplayName,
			"did":          s.cfg.DID(a.Slug),
			"did_document": s.cfg.BaseURL + "/agents/" + a.Slug + "/did.json",
			"agent_card":   s.cfg.BaseURL + "/agents/" + a.Slug + "/agent-card.json",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "count": len(agents), "agents": agents})
}

// handlePresence 返回每个 agent 的最后心跳。
//
// 「心跳」= 该 agent 最后一次带有效 token 的请求时间：客户端本来就每 N 分钟
// 轮询一次 /api/v1/home，所以不需要 peer 额外上报。持有任意有效 token 即可查看
// （对端想自己展示「全员在线」表就是这个接口）。
func (s *Server) handlePresence(w http.ResponseWriter, r *http.Request, slug string) {
	online := 600 * time.Second
	if v := os.Getenv("A2A_ONLINE_SECS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			online = time.Duration(n) * time.Second
		}
	}
	snap := s.presence.Snapshot()
	now := time.Now().UTC()
	list := make([]map[string]any, 0, len(s.cfg.Agents))
	fresh, stale := 0, 0
	for _, a := range s.cfg.Agents {
		entry := map[string]any{
			"slug":         a.Slug,
			"name":         a.Name,
			"display_name": a.DisplayName,
			"online":       false,
			"last_seen":    nil,
			"age_seconds":  nil,
		}
		if t, ok := snap[a.Slug]; ok {
			age := int64(now.Sub(t).Seconds())
			if age < 0 {
				age = 0
			}
			entry["last_seen"] = t.Format(time.RFC3339)
			entry["age_seconds"] = age
			if now.Sub(t) <= online {
				entry["online"] = true
				fresh++
			} else {
				stale++
			}
		}
		list = append(list, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":                  true,
		"now":                      now.Format(time.RFC3339),
		"online_threshold_seconds": int64(online.Seconds()),
		"count":                    len(list),
		"online":                   fresh,
		"offline":                  stale,
		"agents":                   list,
		"note":                     "age_seconds=null 表示自平台上线心跳统计以来从未见过该 agent 的认证请求",
	})
}

func (s *Server) handleDID(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.cfg.DomainDidDocument())
}

func (s *Server) handleAgentDID(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if s.cfg.Agent(slug) == nil {
		writeErr(w, http.StatusNotFound, "unknown agent")
		return
	}
	writeJSON(w, http.StatusOK, s.cfg.DidDocument(slug))
}

func (s *Server) handleAgentCard(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if s.cfg.Agent(slug) == nil {
		writeErr(w, http.StatusNotFound, "unknown agent")
		return
	}
	writeJSON(w, http.StatusOK, s.cfg.AgentCard(slug))
}

func (s *Server) handleProfile(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	a := s.cfg.Agent(slug)
	if a == nil {
		writeErr(w, http.StatusNotFound, "unknown agent")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":      true,
		"id":           a.Slug,
		"slug":         a.Slug,
		"name":         a.Name,
		"display_name": a.DisplayName,
		"did":          s.cfg.DID(a.Slug),
		"avatar_url":   "",
		"profile_url":  s.cfg.BaseURL + "/u/" + a.Slug,
		"agent_card":   s.cfg.BaseURL + "/agents/" + a.Slug + "/agent-card.json",
	})
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = io.WriteString(w, heartbeatMarkdown(s.cfg))
}

func (s *Server) handleSkill(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = io.WriteString(w, skillMarkdown(s.cfg))
}

func (s *Server) handleStyle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = io.WriteString(w, styleMarkdown(s.cfg))
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeErr(w, http.StatusNotFound, "not found")
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request, slug string) {
	n, oldest, newest := s.store.Stats()
	resp := map[string]any{"success": true, "messages": n, "retention_hours": 24}
	if !oldest.IsZero() {
		resp["oldest"] = oldest.UTC().Format(time.RFC3339)
		resp["newest"] = newest.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, resp)
}

func parseBool(s string) bool {
	b, _ := strconv.ParseBool(s)
	return b
}

func init() { _ = os.Getenv("TZ") }
