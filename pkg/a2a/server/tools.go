package server

import (
	"net/http"
	"os"
	"path/filepath"
)

// handleSwitchScript serves the peer migration script.
//
// Public by design: the script contains no secrets — each peer passes its own
// token at run time. Only this single fixed filename is served, so there is no
// path traversal surface.
func (s *Server) handleSwitchScript(w http.ResponseWriter, r *http.Request) {
	s.serveTool(w, "a2a-switch.sh")
}

// handleSignSetup serves the peer-side signing bootstrap script (no secrets:
// the peer passes its own token at run time).
func (s *Server) handleSignSetup(w http.ResponseWriter, r *http.Request) {
	s.serveTool(w, "a2a-sign-setup.sh")
}

// serveTool serves a fixed, allowlisted script filename from <dir>/tools.
// Only these exact names are readable, so there is no path traversal surface.
func (s *Server) serveTool(w http.ResponseWriter, name string) {
	p := filepath.Join(s.cfg.Dir, "tools", name)
	b, err := os.ReadFile(p)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Content-Disposition", `inline; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(b)
}
