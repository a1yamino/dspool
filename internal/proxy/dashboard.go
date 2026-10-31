package proxy

import (
	"context"
	_ "embed"
	"net/http"
	"time"
)

//go:embed dashboard.html
var dashboardHTML []byte

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(dashboardHTML)
}

func (s *Server) handleDashboardData(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.statusPayload(false))
}

func (s *Server) handleDashboardRefresh(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	probed := s.monitor.ProbeAll(ctx)
	payload := s.statusPayload(false)
	payload["probed"] = probed
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) handleDashboardRestore(w http.ResponseWriter, r *http.Request) {
	s.restoreKey(w, r)
}
