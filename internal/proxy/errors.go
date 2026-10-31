package proxy

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type errorFormat int

const (
	fmtAnthropic errorFormat = iota
	fmtOpenAI
)

func formatForPath(path string) errorFormat {
	if strings.HasPrefix(path, "/v1/messages") || strings.HasPrefix(path, "/messages") {
		return fmtAnthropic
	}
	return fmtOpenAI
}

func writeError(w http.ResponseWriter, format errorFormat, status int, typ, message string, retryAfter time.Duration) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds()+0.5)))
	}
	var payload any
	if format == fmtAnthropic {
		payload = map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    typ,
				"message": message,
			},
		}
	} else {
		payload = map[string]any{
			"error": map[string]any{
				"message": message,
				"type":    typ,
				"param":   nil,
				"code":    nil,
			},
		}
	}
	body, _ := json.Marshal(payload)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func anthropicTypeForStatus(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusForbidden:
		return "permission_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status >= 400 && status < 500:
		return "invalid_request_error"
	default:
		return "api_error"
	}
}

func openAITypeForStatus(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusForbidden:
		return "permission_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status >= 400 && status < 500:
		return "invalid_request_error"
	default:
		return "server_error"
	}
}

var hopByHopHeaders = map[string]struct{}{
	"connection":          {},
	"proxy-connection":    {},
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"te":                  {},
	"trailer":             {},
	"transfer-encoding":   {},
	"upgrade":             {},
}

func isHopByHop(name string) bool {
	_, ok := hopByHopHeaders[strings.ToLower(name)]
	return ok
}

func copyResponseHeaders(dst, src http.Header) {
	connectionHeaders := map[string]struct{}{}
	for _, v := range src.Values("Connection") {
		for _, part := range strings.Split(v, ",") {
			connectionHeaders[strings.ToLower(strings.TrimSpace(part))] = struct{}{}
		}
	}
	for name, values := range src {
		lower := strings.ToLower(name)
		if isHopByHop(lower) || lower == "content-length" {
			continue
		}
		if _, listed := connectionHeaders[lower]; listed {
			continue
		}
		for _, v := range values {
			dst.Add(name, v)
		}
	}
}

// relayUpstreamError forwards a non-retryable upstream error untouched.
func relayUpstreamError(w http.ResponseWriter, resp *http.Response, body []byte) {
	copyResponseHeaders(w.Header(), resp.Header)
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	if len(body) > 0 {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}
