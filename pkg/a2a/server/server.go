// PicoClaw - Ultra-lightweight personal AI agent

package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Options configures a private A2A server instance.
type Options struct {
	Listen             string
	DataDir            string
	Domain             string
	Retention          time.Duration
	SignMode           string
	AllowDefaultAgents bool
	Logger             *log.Logger
}

// Instance is a running (or runnable) A2A server.
type Instance struct {
	opts    Options
	cfg     *Config
	store   *Store
	srv     *Server
	handler http.Handler
	lock    *dataLock

	httpSrv *http.Server
	ln      net.Listener

	stopOnce sync.Once
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

// New loads the on-disk state and prepares the HTTP handler. It acquires an
// exclusive lock on the data directory so two servers can never write the same
// files. Call Start to listen, and Stop to release resources.
func New(opts Options) (*Instance, error) {
	if strings.TrimSpace(opts.DataDir) == "" {
		return nil, errors.New("a2a server: data_dir is required")
	}
	if opts.Listen == "" {
		opts.Listen = "127.0.0.1:21220"
	}
	if err := validateLoopbackListen(opts.Listen); err != nil {
		return nil, err
	}
	if opts.Retention == 0 {
		opts.Retention = 24 * time.Hour
	}
	if opts.Retention < 0 {
		opts.Retention = 0
	}
	if opts.Logger == nil {
		opts.Logger = log.New(io.Discard, "", 0)
	}

	lock, err := acquireDataLock(opts.DataDir)
	if err != nil {
		return nil, err
	}
	cleanup := func() {
		if lock != nil {
			lock.Release()
		}
	}

	cfg, err := LoadConfig(opts.DataDir, opts.Domain, opts.AllowDefaultAgents)
	if err != nil {
		cleanup()
		return nil, err
	}
	store, err := NewStore(filepath.Join(opts.DataDir, "data.json"), opts.Retention)
	if err != nil {
		cleanup()
		return nil, err
	}
	srv := NewServer(cfg, store, opts.Logger, opts.SignMode)

	inst := &Instance{
		opts:    opts,
		cfg:     cfg,
		store:   store,
		srv:     srv,
		handler: srv.logging(buildMux(srv)),
		lock:    lock,
		stopCh:  make(chan struct{}),
	}
	return inst, nil
}

// Handler returns the fully assembled HTTP handler (useful for tests).
func (i *Instance) Handler() http.Handler { return i.handler }

// Config exposes the loaded configuration.
func (i *Instance) Config() *Config { return i.cfg }

// Store exposes the message store.
func (i *Instance) Store() *Store { return i.store }

// Addr returns the bound address after Start, or the configured one before.
func (i *Instance) Addr() string {
	if i.ln != nil {
		return i.ln.Addr().String()
	}
	return i.opts.Listen
}

// Start binds the listener and begins serving. It returns once the listener is
// bound; serving happens in the background.
func (i *Instance) Start() error {
	if i.httpSrv != nil {
		return errors.New("a2a server: already started")
	}
	ln, err := net.Listen("tcp", i.opts.Listen)
	if err != nil {
		return fmt.Errorf("a2a server: listen %s: %w", i.opts.Listen, err)
	}
	i.ln = ln
	i.httpSrv = &http.Server{
		Handler:           i.handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	i.wg.Add(1)
	go func() {
		defer i.wg.Done()
		if err := i.httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			i.opts.Logger.Printf("http: %v", err)
		}
	}()

	i.wg.Add(1)
	go i.gcLoop()

	i.logStartup(ln.Addr().String())
	return nil
}

// Stop gracefully shuts the server down and releases the data directory lock.
func (i *Instance) Stop(ctx context.Context) error {
	var err error
	if i.httpSrv != nil {
		err = i.httpSrv.Shutdown(ctx)
	}
	i.stopOnce.Do(func() { close(i.stopCh) })
	i.wg.Wait()
	if i.srv != nil && i.srv.presence != nil {
		i.srv.presence.Close()
	}
	if i.lock != nil {
		i.lock.Release()
		i.lock = nil
	}
	return err
}

func (i *Instance) gcLoop() {
	defer i.wg.Done()
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-i.stopCh:
			return
		case <-t.C:
			if n, err := i.store.GC(); err != nil {
				i.opts.Logger.Printf("gc error: %v", err)
			} else if n > 0 {
				i.opts.Logger.Printf("gc removed %d expired messages", n)
			}
		}
	}
}

func (i *Instance) logStartup(addr string) {
	pySum := sha256Sum(signPySource)
	pub := i.cfg.PublicKey()
	fp := sha256.Sum256(pub)
	i.opts.Logger.Printf("listening on %s (domain=%s, agents=%d, retention=%s, data=%s)",
		addr, i.cfg.Domain, len(i.cfg.Agents), i.opts.Retention.String(),
		filepath.Join(i.opts.DataDir, "data.json"))
	i.opts.Logger.Printf("did:web:%s key ed25519 sha256:%s", i.cfg.Domain, hex.EncodeToString(fp[:]))
	i.opts.Logger.Printf("sign mode=%s payload=%s sign.py sha256:%s",
		i.srv.signMode, signPayloadVersion, pySum)
	i.opts.Logger.Printf("agent keys: %d (redeemed=%d)", len(i.cfg.agentPubAll), i.cfg.redeemedCount())
}

func validateLoopbackListen(listen string) error {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("a2a server: invalid listen address %q: %w", listen, err)
	}
	if strings.TrimSpace(port) == "" {
		return fmt.Errorf("a2a server: listen address %q has no port", listen)
	}
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "127.0.0.1", "::1", "localhost":
		return nil
	default:
		return fmt.Errorf(
			"a2a server: refusing non-loopback listen address %q; "+
				"bind 127.0.0.1 and expose it through a tunnel/reverse proxy",
			listen,
		)
	}
}

func buildMux(s *Server) *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /{$}", s.handleRoot)

	// 签名工具下发（无秘密；sha256 由 /sign.py.sha256 公布）
	mux.HandleFunc("GET /sign.py", s.handleSignPy)
	mux.HandleFunc("GET /tools/sign.py", s.handleSignPy)
	mux.HandleFunc("GET /sign.py.sha256", s.handleSignPyHash)
	mux.HandleFunc("GET /signing.md", s.handleSigningDoc)
	mux.HandleFunc("GET /tools/signing.md", s.handleSigningDoc)

	// 密钥：一次性 redeem / 自持公钥登记 / 状态查询
	mux.HandleFunc("POST /a2a/keys/redeem", s.auth(s.handleRedeem))
	mux.HandleFunc("POST /api/v1/keys/redeem", s.auth(s.handleRedeem))
	mux.HandleFunc("GET /a2a/keys/redeem", s.auth(s.handleRedeem))
	mux.HandleFunc("POST /a2a/redeem", s.auth(s.handleRedeem))
	mux.HandleFunc("POST /api/v1/redeem", s.auth(s.handleRedeem))
	mux.HandleFunc("POST /a2a/keys/register", s.auth(s.handleRegisterKey))
	mux.HandleFunc("POST /api/v1/keys/register", s.auth(s.handleRegisterKey))
	mux.HandleFunc("POST /a2a/keys/retire-seed", s.auth(s.handleRetireSeed))
	mux.HandleFunc("POST /api/v1/keys/retire-seed", s.auth(s.handleRetireSeed))
	mux.HandleFunc("GET /a2a/keys", s.auth(s.handleMyKeys))
	mux.HandleFunc("GET /api/v1/keys", s.auth(s.handleMyKeys))

	mux.HandleFunc("GET /heartbeat.md", s.handleHeartbeat)
	mux.HandleFunc("GET /skill.md", s.handleSkill)
	mux.HandleFunc("GET /style-guide.md", s.handleStyle)

	// Public: peer migration script (no secrets; token is passed at run time).
	mux.HandleFunc("GET /tools/a2a-switch.sh", s.handleSwitchScript)
	mux.HandleFunc("GET /migrate.sh", s.handleSwitchScript)
	mux.HandleFunc("GET /switch.sh", s.handleSwitchScript)

	// 签名接入脚本（给对端跑；token 由对端运行时传入）
	mux.HandleFunc("GET /sign-setup.sh", s.handleSignSetup)
	mux.HandleFunc("GET /a2a-sign-setup.sh", s.handleSignSetup)
	mux.HandleFunc("GET /tools/a2a-sign-setup.sh", s.handleSignSetup)

	mux.HandleFunc("GET /.well-known/did.json", s.handleDID)
	mux.HandleFunc("GET /agents/{slug}/did.json", s.handleAgentDID)
	mux.HandleFunc("GET /agents/{slug}/agent-card.json", s.handleAgentCard)
	mux.HandleFunc("GET /api/v1/presence", s.auth(s.handlePresence))
	mux.HandleFunc("GET /a2a/presence", s.auth(s.handlePresence))
	mux.HandleFunc("GET /api/v1/agents", s.handleAgents)
	mux.HandleFunc("GET /api/v1/agents/{slug}", s.handleProfile)
	mux.HandleFunc("GET /u/{slug}", s.handleProfile)

	mux.HandleFunc("GET /home", s.auth(s.handleHome))
	mux.HandleFunc("GET /api/v1/home", s.auth(s.handleHome))
	mux.HandleFunc("GET /api/v1/me", s.auth(s.handleMe))
	mux.HandleFunc("GET /api/v1/stats", s.auth(s.handleStats))

	mux.HandleFunc("GET /a2a/conversations", s.auth(s.handleConversations))
	mux.HandleFunc("GET /api/v1/a2a/conversations", s.auth(s.handleConversations))
	mux.HandleFunc("GET /a2a/conversations/{id}", s.auth(s.handleConversation))
	mux.HandleFunc("GET /api/v1/a2a/conversations/{id}", s.auth(s.handleConversation))
	mux.HandleFunc("GET /a2a/conversations/{id}/messages", s.auth(s.handleConversation))
	mux.HandleFunc("GET /api/v1/a2a/conversations/{id}/messages", s.auth(s.handleConversation))
	mux.HandleFunc("POST /a2a/conversations/{id}/read", s.auth(s.handleMarkRead))
	mux.HandleFunc("POST /api/v1/a2a/conversations/{id}/read", s.auth(s.handleMarkRead))

	mux.HandleFunc("POST /a2a/{name}", s.auth(s.handleSend))
	mux.HandleFunc("POST /api/v1/a2a/{name}", s.auth(s.handleSend))
	mux.HandleFunc("POST /a2a/{name}/messages", s.auth(s.handleSend))
	mux.HandleFunc("POST /api/v1/a2a/{name}/messages", s.auth(s.handleSend))

	mux.HandleFunc("/", s.handleNotFound)
	return mux
}

func sha256Sum(s string) string {
	return sha256SumBytes([]byte(s))
}

func sha256SumBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
