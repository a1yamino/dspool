package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dspool/internal/pool"
)

const maxRelayBytes = 128 << 20

type rewrittenBody struct {
	Body           []byte
	RequestedModel string
	Model          string
	Stream         bool
}

func (s *Server) readRequestBody(w http.ResponseWriter, r *http.Request, format errorFormat) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, format, http.StatusRequestEntityTooLarge, requestType(format),
				fmt.Sprintf("request body exceeds %d bytes", maxErr.Limit), 0)
			return nil, false
		}
		writeError(w, format, http.StatusBadRequest, requestType(format), "failed to read request body: "+err.Error(), 0)
		return nil, false
	}
	if len(bytes.TrimSpace(body)) == 0 {
		writeError(w, format, http.StatusBadRequest, requestType(format), "request body is empty", 0)
		return nil, false
	}
	return body, true
}

func requestType(format errorFormat) string {
	if format == fmtAnthropic {
		return "invalid_request_error"
	}
	return "invalid_request_error"
}

// rewriteJSONBody rewrites the model name and (for chat) normalises newer
// OpenAI max-token fields to DeepSeek's max_tokens.
func (s *Server) rewriteJSONBody(raw []byte, chat bool) (rewrittenBody, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		if err == nil {
			err = errors.New("body must be a JSON object")
		}
		return rewrittenBody{}, fmt.Errorf("invalid JSON body: %w", err)
	}

	requested := ""
	if v, ok := fields["model"]; ok {
		if err := json.Unmarshal(v, &requested); err != nil {
			return rewrittenBody{}, errors.New("model must be a string")
		}
	}
	mapped := s.mapper.Map(requested)
	modelJSON, _ := json.Marshal(mapped)
	fields["model"] = modelJSON

	stream := false
	if v, ok := fields["stream"]; ok {
		_ = json.Unmarshal(v, &stream)
	}

	if chat {
		if _, ok := fields["max_tokens"]; !ok {
			if v, ok := fields["max_completion_tokens"]; ok {
				fields["max_tokens"] = v
			}
		}
		delete(fields, "max_completion_tokens")
		normalizeDeveloperRoles(fields)
	}

	if cap := s.cfg.Upstream.MaxTokensCap; cap > 0 {
		if v, ok := fields["max_tokens"]; ok {
			var n int
			if json.Unmarshal(v, &n) == nil && n > cap {
				b, _ := json.Marshal(cap)
				fields["max_tokens"] = b
			}
		}
	}

	out, err := json.Marshal(fields)
	if err != nil {
		return rewrittenBody{}, err
	}
	return rewrittenBody{
		Body:           out,
		RequestedModel: requested,
		Model:          mapped,
		Stream:         stream,
	}, nil
}

// normalizeDeveloperRoles rewrites OpenAI's "developer" role to "system",
// which DeepSeek's chat endpoint accepts.
func normalizeDeveloperRoles(fields map[string]json.RawMessage) {
	raw, ok := fields["messages"]
	if !ok {
		return
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &messages); err != nil || messages == nil {
		return
	}
	changed := false
	for _, message := range messages {
		var role string
		if err := json.Unmarshal(message["role"], &role); err != nil {
			continue
		}
		if strings.EqualFold(role, "developer") {
			encoded, _ := json.Marshal("system")
			message["role"] = encoded
			changed = true
		}
	}
	if !changed {
		return
	}
	encoded, err := json.Marshal(messages)
	if err == nil {
		fields["messages"] = encoded
	}
}

func joinUpstreamURL(base, path, rawQuery string) string {
	u := strings.TrimRight(base, "/") + path
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	return u
}

type failureKind int

const (
	kindNone failureKind = iota
	kindAuth
	kindInsufficient
	kindRateLimit
	kindServer
	kindClient
)

func (k failureKind) String() string {
	switch k {
	case kindAuth:
		return "auth"
	case kindInsufficient:
		return "balance"
	case kindRateLimit:
		return "rate_limit"
	case kindServer:
		return "server"
	case kindClient:
		return "client"
	default:
		return "none"
	}
}

func classifyFailure(status int, body []byte) (failureKind, time.Duration) {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return kindAuth, 0
	case status == http.StatusPaymentRequired:
		return kindInsufficient, 0
	case status == http.StatusTooManyRequests:
		return kindRateLimit, 0
	case status >= 500:
		return kindServer, 0
	}
	lower := strings.ToLower(string(body))
	switch {
	case strings.Contains(lower, "insufficient balance"):
		return kindInsufficient, 0
	case strings.Contains(lower, "authentication fails"), strings.Contains(lower, "invalid api key"):
		return kindAuth, 0
	}
	return kindClient, 0
}

func parseRetryAfter(h http.Header, now time.Time) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(v); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(v); err == nil {
		if d := when.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}

func upstreamReason(status int, body []byte) string {
	s := snippet(body)
	if s == "" {
		return fmt.Sprintf("HTTP %d", status)
	}
	return fmt.Sprintf("HTTP %d: %s", status, s)
}

// upstreamWithFailover acquires a key, performs one upstream attempt, and
// retries with other keys on key-specific failures. On success it returns a 2xx
// response whose body the caller must consume. On failure it has already
// written the error response.
func (s *Server) upstreamWithFailover(
	w http.ResponseWriter,
	r *http.Request,
	url string,
	body []byte,
	isAnthropic bool,
	format errorFormat,
) (*http.Response, bool) {
	attempts := s.cfg.Pool.MaxAttempts
	if attempts < 1 {
		attempts = 1
	}
	tried := make(map[string]struct{}, attempts)
	var (
		lastKind       failureKind
		lastStatus     int
		lastReason     string
		lastRetryAfter time.Duration
		noKeyReason    string
	)

	for attempt := 0; attempt < attempts; attempt++ {
		lease, err := s.pool.Acquire(tried)
		if err != nil {
			var nk *pool.NoKeysError
			if errors.As(err, &nk) && nk.HasCold() && s.cfg.Pool.RefreshColdOnExhaustion {
				ctx, cancel := contextWithTimeout(r, 6*time.Second)
				s.monitor.MaybeRefreshCold(ctx)
				cancel()
				lease, err = s.pool.Acquire(tried)
			}
			if err != nil {
				noKeyReason = err.Error()
				break
			}
		}

		tried[lease.KeyID()] = struct{}{}
		req, err := s.newUpstreamRequest(r, url, body, isAnthropic, lease.Secret())
		if err != nil {
			lease.Cancel()
			typ := openAITypeForStatus(http.StatusBadGateway)
			if format == fmtAnthropic {
				typ = anthropicTypeForStatus(http.StatusBadGateway)
			}
			writeError(w, format, http.StatusBadGateway, typ, "failed to build upstream request: "+err.Error(), 0)
			return nil, false
		}

		start := time.Now()
		resp, err := s.client.Do(req)
		if err != nil {
			if r.Context().Err() != nil {
				lease.Cancel()
				s.log.Debug("client canceled while waiting for upstream",
					"id", requestID(r.Context()), "key", lease.Label())
				return nil, false
			}
			reason := err.Error()
			lease.Fail(pool.FailNetwork, 0, reason, 0)
			lastKind = kindNone
			lastReason = reason
			s.log.Warn("upstream network error, trying another key",
				"id", requestID(r.Context()),
				"key", lease.Label(), "mask", lease.Mask(),
				"error", err,
			)
			continue
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			lease.Success(resp.StatusCode)
			s.log.Debug("upstream request succeeded",
				"id", requestID(r.Context()),
				"key", lease.Label(), "mask", lease.Mask(),
				"status", resp.StatusCode,
				"attempt", attempt+1,
				"duration_ms", time.Since(start).Milliseconds(),
			)
			return resp, true
		}

		errBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			errBody = nil
		}
		kind, _ := classifyFailure(resp.StatusCode, errBody)
		retryAfter := parseRetryAfter(resp.Header, time.Now())
		reason := upstreamReason(resp.StatusCode, errBody)
		lastKind = kind
		lastStatus = resp.StatusCode
		lastReason = reason
		lastRetryAfter = retryAfter

		switch kind {
		case kindAuth:
			lease.Fail(pool.FailAuth, resp.StatusCode, reason, 0)
			s.log.Warn("upstream key rejected; removed from pool",
				"id", requestID(r.Context()),
				"key", lease.Label(), "mask", lease.Mask(),
				"status", resp.StatusCode, "detail", snippet(errBody),
			)
			continue
		case kindInsufficient:
			lease.Fail(pool.FailInsufficient, resp.StatusCode, reason, 0)
			s.log.Warn("upstream key out of balance; moved to cold pool",
				"id", requestID(r.Context()),
				"key", lease.Label(), "mask", lease.Mask(),
				"status", resp.StatusCode,
			)
			continue
		case kindRateLimit:
			lease.Fail(pool.FailRateLimit, resp.StatusCode, reason, retryAfter)
			s.log.Debug("upstream rate limited; trying another key",
				"id", requestID(r.Context()),
				"key", lease.Label(), "mask", lease.Mask(),
				"retry_after", retryAfter.String(),
			)
			continue
		case kindServer:
			lease.Fail(pool.FailServer, resp.StatusCode, reason, 0)
			s.log.Warn("upstream server error; trying another key",
				"id", requestID(r.Context()),
				"key", lease.Label(), "mask", lease.Mask(),
				"status", resp.StatusCode,
			)
			continue
		default:
			// The client's request is invalid; retrying with another key will
			// not help. Relay DeepSeek's response untouched.
			lease.Neutral(resp.StatusCode)
			relayUpstreamError(w, resp, errBody)
			return nil, false
		}
	}

	s.writePoolExhausted(w, format, noKeyReason, lastKind, lastStatus, lastReason, lastRetryAfter)
	return nil, false
}

func (s *Server) writePoolExhausted(
	w http.ResponseWriter,
	format errorFormat,
	noKeyReason string,
	lastKind failureKind,
	lastStatus int,
	lastReason string,
	lastRetryAfter time.Duration,
) {
	counts := s.pool.Counts()
	status := http.StatusServiceUnavailable
	typ := openAITypeForStatus(status)
	retryAfter := time.Duration(0)

	if lastKind == kindRateLimit {
		status = http.StatusTooManyRequests
		typ = openAITypeForStatus(status)
		retryAfter = lastRetryAfter
		if retryAfter <= 0 {
			retryAfter = 5 * time.Second
		}
		if retryAfter > 10*time.Minute {
			retryAfter = 10 * time.Minute
		}
	}
	if format == fmtAnthropic {
		typ = anthropicTypeForStatus(status)
	}

	detail := lastReason
	if detail == "" {
		detail = noKeyReason
	}
	if detail == "" {
		detail = "no usable upstream key"
	}
	message := fmt.Sprintf("DeepSeek key pool unavailable: %s (hot=%d cold=%d invalid=%d cooling=%d)",
		detail, counts.Hot, counts.Cold, counts.Invalid, counts.Cooling)

	s.log.Warn("request failed: key pool exhausted",
		"status", status, "detail", detail,
		"hot", counts.Hot, "cold", counts.Cold,
		"invalid", counts.Invalid, "cooling", counts.Cooling,
		"last_upstream_status", lastStatus,
	)
	writeError(w, format, status, typ, message, retryAfter)
}

func (s *Server) newUpstreamRequest(
	r *http.Request,
	url string,
	body []byte,
	isAnthropic bool,
	secret string,
) (*http.Request, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyRequestHeaders(req.Header, r.Header)
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Del("x-api-key")
	req.Header.Del("api-key")
	req.Header.Del("Accept-Encoding")
	req.Header.Del("Content-Length")
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if isAnthropic && req.Header.Get("anthropic-version") == "" {
		req.Header.Set("anthropic-version", s.cfg.Upstream.AnthropicVersion)
	}
	if id := requestID(r.Context()); id != "" {
		req.Header.Set("X-Dspool-Request-Id", id)
	}
	req.ContentLength = int64(len(body))
	return req, nil
}

func copyRequestHeaders(dst, src http.Header) {
	connectionHeaders := map[string]struct{}{}
	for _, v := range src.Values("Connection") {
		for _, part := range strings.Split(v, ",") {
			connectionHeaders[strings.ToLower(strings.TrimSpace(part))] = struct{}{}
		}
	}
	for name, values := range src {
		lower := strings.ToLower(name)
		if isHopByHop(lower) {
			continue
		}
		if _, listed := connectionHeaders[lower]; listed {
			continue
		}
		switch lower {
		case "authorization", "x-api-key", "api-key", "content-length",
			"accept-encoding", "host", "cookie", "x-admin-token", "expect":
			continue
		}
		for _, v := range values {
			dst.Add(name, v)
		}
	}
}

func (s *Server) relayResponse(w http.ResponseWriter, resp *http.Response, stream bool, format errorFormat) {
	defer resp.Body.Close()
	copyResponseHeaders(w.Header(), resp.Header)

	if stream {
		w.Header().Del("Content-Length")
		w.Header().Set("X-Accel-Buffering", "no")
		if w.Header().Get("Cache-Control") == "" {
			w.Header().Set("Cache-Control", "no-cache")
		}
		w.WriteHeader(resp.StatusCode)
		rc := http.NewResponseController(w)
		fw := &flushWriter{w: w, rc: rc}
		_, err := io.CopyBuffer(fw, resp.Body, make([]byte, 32<<10))
		if err != nil {
			s.log.Debug("upstream stream ended with copy error", "error", err)
		}
		return
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRelayBytes+1))
	if err != nil {
		w.Header().Del("Content-Length")
		typ := openAITypeForStatus(http.StatusBadGateway)
		if format == fmtAnthropic {
			typ = anthropicTypeForStatus(http.StatusBadGateway)
		}
		writeError(w, format, http.StatusBadGateway, typ, "failed to read upstream response: "+err.Error(), 0)
		return
	}
	if len(body) > maxRelayBytes {
		w.Header().Del("Content-Length")
		typ := openAITypeForStatus(http.StatusBadGateway)
		if format == fmtAnthropic {
			typ = anthropicTypeForStatus(http.StatusBadGateway)
		}
		writeError(w, format, http.StatusBadGateway, typ, "upstream response too large", 0)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}
