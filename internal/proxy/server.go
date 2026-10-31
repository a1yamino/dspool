package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"dspool/internal/config"
	"dspool/internal/keyutil"
	"dspool/internal/monitor"
	"dspool/internal/pool"
	"dspool/internal/state"
)

type Server struct {
	cfg     *config.Config
	pool    *pool.Pool
	monitor *monitor.Monitor
	state   *state.Store
	log     *slog.Logger
	version string
	started time.Time

	mapper       *ModelMapper
	client       *http.Client
	clientHashes map[string]struct{}
}

func New(
	cfg *config.Config,
	p *pool.Pool,
	mon *monitor.Monitor,
	st *state.Store,
	version string,
	log *slog.Logger,
) (*Server, error) {
	mapper, err := NewModelMapper(cfg.Models)
	if err != nil {
		return nil, err
	}

	base := http.DefaultTransport.(*http.Transport).Clone()
	base.MaxIdleConns = 200
	base.MaxIdleConnsPerHost = 64
	base.IdleConnTimeout = 90 * time.Second
	base.TLSHandshakeTimeout = 15 * time.Second
	base.ResponseHeaderTimeout = cfg.Upstream.RequestHeaderTimeout.D()
	base.ForceAttemptHTTP2 = true

	s := &Server{
		cfg:          cfg,
		pool:         p,
		monitor:      mon,
		state:        st,
		log:          log,
		version:      version,
		started:      time.Now(),
		mapper:       mapper,
		clientHashes: map[string]struct{}{},
		client: &http.Client{
			Transport: base,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	for _, key := range cfg.ClientKeys {
		s.clientHashes[keyutil.Hash(key)] = struct{}{}
	}
	return s, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Local, unauthenticated dashboard.
	mux.HandleFunc("GET /{$}", s.handleDashboard)
	mux.HandleFunc("GET /dashboard", s.handleDashboard)
	mux.HandleFunc("GET /dashboard/{$}", s.handleDashboard)
	mux.HandleFunc("GET /dashboard/data", s.handleDashboardData)
	mux.HandleFunc("POST /dashboard/refresh", s.handleDashboardRefresh)
	mux.HandleFunc("POST /dashboard/keys/{id}/restore", s.handleDashboardRestore)

	// Proxy endpoints.
	mux.HandleFunc("POST /v1/messages", s.protect(fmtAnthropic, s.handleMessages))
	mux.HandleFunc("POST /messages", s.protect(fmtAnthropic, s.handleMessages))
	mux.HandleFunc("POST /v1/messages/count_tokens", s.protect(fmtAnthropic, s.handleCountTokens))
	mux.HandleFunc("POST /messages/count_tokens", s.protect(fmtAnthropic, s.handleCountTokens))
	mux.HandleFunc("POST /v1/chat/completions", s.protect(fmtOpenAI, s.handleChatCompletions))
	mux.HandleFunc("POST /chat/completions", s.protect(fmtOpenAI, s.handleChatCompletions))
	mux.HandleFunc("POST /v1/responses", s.protect(fmtOpenAI, s.handleResponses))
	mux.HandleFunc("POST /responses", s.protect(fmtOpenAI, s.handleResponses))
	mux.HandleFunc("GET /v1/models", s.protect(fmtOpenAI, s.handleModels))
	mux.HandleFunc("GET /models", s.protect(fmtOpenAI, s.handleModels))

	mux.HandleFunc("GET /healthz", s.handleHealth)

	// Admin endpoints (token-gated when configured).
	mux.HandleFunc("GET /admin/status", s.admin(s.handleAdminStatus))
	mux.HandleFunc("POST /admin/refresh", s.admin(s.handleAdminRefresh))
	mux.HandleFunc("POST /admin/keys/{id}/restore", s.admin(s.handleAdminRestore))

	mux.HandleFunc("/", s.handleNotFound)

	var h http.Handler = mux
	h = s.browserGuardMiddleware(h)
	h = s.recoverMiddleware(h)
	h = s.accessLogMiddleware(h)
	return h
}

type ctxKey int

const reqIDKey ctxKey = 0

func requestID(ctx context.Context) string {
	if v, ok := ctx.Value(reqIDKey).(string); ok {
		return v
	}
	return ""
}

func newRequestID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req"
	}
	return hex.EncodeToString(b[:])
}

func (s *Server) accessLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := newRequestID()
		w.Header().Set("X-Dspool-Request-Id", rid)
		r = r.WithContext(context.WithValue(r.Context(), reqIDKey, rid))

		sw := &statusWriter{ResponseWriter: w}
		start := time.Now()
		defer func() {
			s.log.Info("http",
				"id", rid,
				"method", r.Method,
				"path", r.URL.Path,
				"status", sw.statusCode(),
				"bytes", sw.bytes,
				"duration_ms", time.Since(start).Milliseconds(),
				"remote", r.RemoteAddr,
			)
		}()
		next.ServeHTTP(sw, r)
	})
}

func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic while serving request",
					"id", requestID(r.Context()),
					"path", r.URL.Path,
					"panic", rec,
				)
				writeError(w, formatForPath(r.URL.Path), http.StatusInternalServerError, "server_error", "internal proxy panic", 0)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// browserGuardMiddleware blocks cross-site browser requests (CSRF / DNS
// rebinding) without affecting CLI clients, which do not send Origin.
func (s *Server) browserGuardMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if origin := r.Header.Get("Origin"); origin != "" {
			if !sameOriginHost(origin, r.Host) {
				writeError(w, formatForPath(r.URL.Path), http.StatusForbidden, "permission_error",
					"cross-origin browser requests are not allowed", 0)
				return
			}
		}
		if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
			writeError(w, formatForPath(r.URL.Path), http.StatusForbidden, "permission_error",
				"cross-site browser requests are not allowed", 0)
			return
		}
		if len(s.clientHashes) == 0 && !hostIsLoopback(r.Host) {
			writeError(w, formatForPath(r.URL.Path), http.StatusForbidden, "permission_error",
				"client_keys is empty: only loopback Host headers are accepted", 0)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func sameOriginHost(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, host)
}

func hostIsLoopback(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) protect(format errorFormat, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			writeError(w, format, http.StatusUnauthorized, "authentication_error", "invalid proxy client token", 0)
			return
		}
		next(w, r)
	}
}

func (s *Server) authorized(r *http.Request) bool {
	if len(s.clientHashes) == 0 {
		return true
	}
	for _, token := range clientTokens(r) {
		if _, ok := s.clientHashes[keyutil.Hash(token)]; ok {
			return true
		}
	}
	return false
}

func clientTokens(r *http.Request) []string {
	var out []string
	if v := r.Header.Get("Authorization"); v != "" {
		v = strings.TrimSpace(v)
		if len(v) > 7 && strings.EqualFold(v[:7], "bearer ") {
			v = strings.TrimSpace(v[7:])
		}
		if v != "" {
			out = append(out, v)
		}
	}
	if v := strings.TrimSpace(r.Header.Get("x-api-key")); v != "" {
		out = append(out, v)
	}
	if v := strings.TrimSpace(r.Header.Get("api-key")); v != "" {
		out = append(out, v)
	}
	return out
}

func (s *Server) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AdminToken == "" {
			if !config.ListenIsLoopback(s.cfg.Listen) {
				writeError(w, fmtOpenAI, http.StatusForbidden, "permission_error",
					"admin endpoints require admin_token when listening on a non-loopback interface", 0)
				return
			}
		} else {
			token := r.Header.Get("X-Admin-Token")
			if token == "" {
				for _, t := range clientTokens(r) {
					token = t
					break
				}
			}
			if token == "" || !constantTimeEqual(token, s.cfg.AdminToken) {
				writeError(w, fmtOpenAI, http.StatusUnauthorized, "authentication_error", "invalid admin token", 0)
				return
			}
		}
		next(w, r)
	}
}

func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status != 0 {
		return
	}
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

type flushWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func (f *flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err == nil {
		if ferr := f.rc.Flush(); ferr != nil && !errors.Is(ferr, http.ErrNotSupported) {
			return n, ferr
		}
	}
	return n, err
}

func (s *Server) statusPayload(includeTombstones bool) map[string]any {
	counts := s.pool.Counts()
	payload := map[string]any{
		"version":         s.version,
		"uptime_seconds":  int64(time.Since(s.started).Seconds()),
		"generated_at":    time.Now().UTC(),
		"listen":          s.cfg.Listen,
		"strategy":        s.cfg.Pool.Strategy,
		"counts":          counts,
		"keys":            s.pool.Snapshots(),
		"model_default":   s.cfg.Models.Default,
		"model_small":     s.cfg.Models.Small,
		"responses_think": s.cfg.Responses.Thinking,
		"intervals": map[string]any{
			"balance":  s.cfg.Pool.BalanceInterval.D().String(),
			"cold":     s.cfg.Pool.ColdCheckInterval.D().String(),
			"attempts": s.cfg.Pool.MaxAttempts,
		},
	}
	if includeTombstones {
		payload["tombstones"] = s.state.InvalidKeys()
	}
	return payload
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(payload)
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, formatForPath(r.URL.Path), http.StatusNotFound, "invalid_request_error", "no such endpoint: "+r.URL.Path, 0)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	counts := s.pool.Counts()
	status := http.StatusOK
	state := "ok"
	if counts.Hot-counts.Cooling <= 0 {
		status = http.StatusServiceUnavailable
		state = "degraded"
	}
	writeJSON(w, status, map[string]any{
		"status": state,
		"counts": counts,
	})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	ids := s.mapper.KnownModels()
	data := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		data = append(data, map[string]any{
			"id":       id,
			"object":   "model",
			"created":  0,
			"owned_by": "deepseek",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *Server) handleAdminStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.statusPayload(true))
}

func (s *Server) handleAdminRefresh(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	probed := s.monitor.ProbeAll(ctx)
	payload := s.statusPayload(true)
	payload["probed"] = probed
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) handleAdminRestore(w http.ResponseWriter, r *http.Request) {
	s.restoreKey(w, r)
}

func (s *Server) restoreKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	snap, ok := s.pool.Restore(id)
	if !ok {
		writeError(w, fmtOpenAI, http.StatusNotFound, "invalid_request_error", "unknown key id", 0)
		return
	}
	s.monitor.Wake()
	writeJSON(w, http.StatusOK, map[string]any{"key": snap})
}
