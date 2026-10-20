package server

// presence.go —— 平台侧「心跳」记录：每个 agent 最后一次带 token 的请求时间。
//
// 为什么需要它：客户端（sentry / poll）本来就是每 N 分钟带 token 轮询一次，
// 所以「最后一次认证请求」就等于心跳，不需要 peer 额外上报。
//
// 落盘：<dir>/presence.json（0600，20 秒节流的原子写），重启后不丢。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type presenceStore struct {
	mu    sync.Mutex
	path  string
	m     map[string]time.Time
	dirty bool
	stop  chan struct{}
	once  sync.Once
}

func newPresenceStore(dir string) *presenceStore {
	p := &presenceStore{
		path: filepath.Join(dir, "presence.json"),
		m:    map[string]time.Time{},
		stop: make(chan struct{}),
	}
	if b, err := os.ReadFile(p.path); err == nil {
		var raw map[string]string
		if json.Unmarshal(b, &raw) == nil {
			for k, v := range raw {
				if t, err := time.Parse(time.RFC3339, v); err == nil {
					p.m[k] = t.UTC()
				}
			}
		}
	}
	go p.loop()
	return p
}

// Close stops the background flusher and persists the final snapshot.
func (p *presenceStore) Close() {
	if p == nil {
		return
	}
	p.once.Do(func() { close(p.stop) })
	p.flush()
}

// Touch 记录某 slug 刚刚有一次认证请求。
func (p *presenceStore) Touch(slug string) {
	if slug == "" {
		return
	}
	p.mu.Lock()
	p.m[slug] = time.Now().UTC()
	p.dirty = true
	p.mu.Unlock()
}

// Snapshot 返回副本，调用方随便读。
func (p *presenceStore) Snapshot() map[string]time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]time.Time, len(p.m))
	for k, v := range p.m {
		out[k] = v
	}
	return out
}

func (p *presenceStore) loop() {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			p.flush()
		}
	}
}

func (p *presenceStore) flush() {
	p.mu.Lock()
	if !p.dirty {
		p.mu.Unlock()
		return
	}
	raw := make(map[string]string, len(p.m))
	for k, v := range p.m {
		raw[k] = v.Format(time.RFC3339)
	}
	p.dirty = false
	p.mu.Unlock()

	b, err := json.Marshal(raw)
	if err != nil {
		return
	}
	tmp := p.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) != nil {
		return
	}
	_ = os.Rename(tmp, p.path)
}
