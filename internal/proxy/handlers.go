package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readRequestBody(w, r, fmtAnthropic)
	if !ok {
		return
	}
	rw, err := s.rewriteJSONBody(body, false)
	if err != nil {
		writeError(w, fmtAnthropic, http.StatusBadRequest, "invalid_request_error", err.Error(), 0)
		return
	}
	url := joinUpstreamURL(s.cfg.Upstream.AnthropicBase, "/v1/messages", r.URL.RawQuery)
	s.log.Debug("proxying Anthropic messages",
		"id", requestID(r.Context()),
		"requested_model", rw.RequestedModel,
		"upstream_model", rw.Model,
		"stream", rw.Stream,
	)
	resp, ok := s.upstreamWithFailover(w, r, url, rw.Body, true, fmtAnthropic)
	if !ok {
		return
	}
	s.relayResponse(w, resp, rw.Stream, fmtAnthropic)
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readRequestBody(w, r, fmtOpenAI)
	if !ok {
		return
	}
	rw, err := s.rewriteJSONBody(body, true)
	if err != nil {
		writeError(w, fmtOpenAI, http.StatusBadRequest, "invalid_request_error", err.Error(), 0)
		return
	}
	url := joinUpstreamURL(s.cfg.Upstream.OpenAIBase, "/chat/completions", r.URL.RawQuery)
	s.log.Debug("proxying chat completions",
		"id", requestID(r.Context()),
		"requested_model", rw.RequestedModel,
		"upstream_model", rw.Model,
		"stream", rw.Stream,
	)
	resp, ok := s.upstreamWithFailover(w, r, url, rw.Body, false, fmtOpenAI)
	if !ok {
		return
	}
	s.relayResponse(w, resp, rw.Stream, fmtOpenAI)
}

func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readRequestBody(w, r, fmtAnthropic)
	if !ok {
		return
	}
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		writeError(w, fmtAnthropic, http.StatusBadRequest, "invalid_request_error", "invalid JSON body", 0)
		return
	}
	tokens := estimateTokens(payload)
	if tokens < 1 {
		tokens = 1
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": tokens})
}

// estimateTokens is a fast local approximation used only for Claude Code's
// count_tokens calls. DeepSeek does not expose a tokenizer endpoint.
func estimateTokens(v any) int {
	var ascii, nonASCII, other int
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			for _, r := range x {
				if r < 128 {
					ascii++
				} else {
					nonASCII++
				}
			}
		case []any:
			for _, item := range x {
				walk(item)
			}
		case map[string]any:
			for _, item := range x {
				walk(item)
			}
		case float64, bool, nil:
			other++
		}
	}
	walk(v)
	// ~4 ASCII characters per token, ~1 token per non-ASCII rune, plus a small
	// per-item JSON overhead.
	return (ascii+3)/4 + nonASCII + other/4 + 16
}
