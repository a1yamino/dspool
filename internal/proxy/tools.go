package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"
)

// DeepSeek exposes web search and fetch as raw JSON endpoints under
// /v1/tools. dspool keeps their request and response shapes untouched and
// adds the same key selection, retry, and failover behavior as the model
// endpoints.

const (
	maxToolRequestBytes = 64 << 10
	maxSearchQueryRunes = 128
	maxFetchURLBytes    = 8192
)

func (s *Server) handleToolSearch(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readToolRequest(w, r)
	if !ok {
		return
	}

	var req struct {
		Query        string `json:"query"`
		Count        *int   `json:"count"`
		NeedContent  *bool  `json:"need_content"`
		OriQuestion  string `json:"ori_question"`
		Localization string `json:"localization"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, fmtOpenAI, http.StatusBadRequest, "invalid_request_error",
			"invalid JSON body: "+err.Error(), 0)
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		writeError(w, fmtOpenAI, http.StatusBadRequest, "invalid_request_error", "query is required", 0)
		return
	}
	if utf8.RuneCountInString(req.Query) > maxSearchQueryRunes {
		writeError(w, fmtOpenAI, http.StatusBadRequest, "invalid_request_error",
			"query must be at most 128 characters", 0)
		return
	}
	if req.Count != nil && (*req.Count < 1 || *req.Count > 50) {
		writeError(w, fmtOpenAI, http.StatusBadRequest, "invalid_request_error",
			"count must be between 1 and 50", 0)
		return
	}
	if req.OriQuestion != "" && utf8.RuneCountInString(req.OriQuestion) > maxSearchQueryRunes {
		writeError(w, fmtOpenAI, http.StatusBadRequest, "invalid_request_error",
			"ori_question must be at most 128 characters", 0)
		return
	}

	s.proxyToolRequest(w, r, "/tools/search", body)
}

func (s *Server) handleToolFetch(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readToolRequest(w, r)
	if !ok {
		return
	}

	var req struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, fmtOpenAI, http.StatusBadRequest, "invalid_request_error",
			"invalid JSON body: "+err.Error(), 0)
		return
	}
	target := strings.TrimSpace(req.URL)
	if target == "" {
		writeError(w, fmtOpenAI, http.StatusBadRequest, "invalid_request_error", "url is required", 0)
		return
	}
	if len(target) > maxFetchURLBytes {
		writeError(w, fmtOpenAI, http.StatusBadRequest, "invalid_request_error",
			"url must be at most 8192 bytes", 0)
		return
	}
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		writeError(w, fmtOpenAI, http.StatusBadRequest, "invalid_request_error",
			"url must be an http or https URL with a host and no credentials", 0)
		return
	}

	s.proxyToolRequest(w, r, "/tools/fetch", body)
}

func (s *Server) proxyToolRequest(w http.ResponseWriter, r *http.Request, path string, body []byte) {
	upstreamURL := joinUpstreamURL(s.cfg.Upstream.OpenAIBase, path, r.URL.RawQuery)
	resp, ok := s.upstreamWithFailover(w, r, upstreamURL, body, false, fmtOpenAI)
	if !ok {
		return
	}
	s.relayResponse(w, resp, false, fmtOpenAI)
}

func (s *Server) readToolRequest(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxToolRequestBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		if maxErr, ok := errors.AsType[*http.MaxBytesError](err); ok {
			writeError(w, fmtOpenAI, http.StatusRequestEntityTooLarge, "invalid_request_error",
				fmt.Sprintf("request body exceeds %d bytes", maxErr.Limit), 0)
			return nil, false
		}
		writeError(w, fmtOpenAI, http.StatusBadRequest, "invalid_request_error",
			"failed to read request body: "+err.Error(), 0)
		return nil, false
	}
	if len(bytes.TrimSpace(body)) == 0 {
		writeError(w, fmtOpenAI, http.StatusBadRequest, "invalid_request_error", "request body is empty", 0)
		return nil, false
	}
	return body, true
}
