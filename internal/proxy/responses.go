package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"dspool/internal/keyutil"
)

type responsesRequest struct {
	Model             string            `json:"model"`
	Instructions      string            `json:"instructions"`
	Input             json.RawMessage   `json:"input"`
	Stream            bool              `json:"stream"`
	Tools             []json.RawMessage `json:"tools"`
	ToolChoice        json.RawMessage   `json:"tool_choice"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls"`
	MaxOutputTokens   *int              `json:"max_output_tokens"`
	Temperature       *float64          `json:"temperature"`
	TopP              *float64          `json:"top_p"`
	Text              *struct {
		Verbosity string `json:"verbosity"`
		Format    *struct {
			Type   string          `json:"type"`
			Name   string          `json:"name"`
			Strict bool            `json:"strict"`
			Schema json.RawMessage `json:"schema"`
		} `json:"format"`
	} `json:"text"`
	Reasoning *struct {
		Effort json.RawMessage `json:"effort"`
	} `json:"reasoning"`
	Store              *bool    `json:"store"`
	PromptCacheKey     *string  `json:"prompt_cache_key"`
	PreviousResponseID string   `json:"previous_response_id"`
	Include            []string `json:"include"`
}

type responseInputItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Namespace string          `json:"namespace"`
	Arguments json.RawMessage `json:"arguments"`
	Input     json.RawMessage `json:"input"`
	Output    json.RawMessage `json:"output"`
}

type chatMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type chatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function chatFunctionCall `json:"function"`
}

type chatFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatTool struct {
	Type     string      `json:"type"`
	Function chatToolDef `json:"function"`
}

type chatToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type toolInfo struct {
	Namespace string
	Name      string
	Custom    bool
}

type responsesPlan struct {
	Body           []byte
	RequestedModel string
	Model          string
	Stream         bool
	ToolInfo       map[string]toolInfo
	OrigToChat     map[string]string
	SkippedTools   []string
}

type toolConverter struct {
	tools      []chatTool
	info       map[string]toolInfo
	origToChat map[string]string
	skipped    []string
}

func newToolConverter() *toolConverter {
	return &toolConverter{
		info:       map[string]toolInfo{},
		origToChat: map[string]string{},
	}
}

func (c *toolConverter) convert(raws []json.RawMessage, namespace string) {
	for _, raw := range raws {
		var t struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
			Format      *struct {
				Type       string `json:"type"`
				Syntax     string `json:"syntax"`
				Definition string `json:"definition"`
			} `json:"format"`
			Tools []json.RawMessage `json:"tools"`
		}
		if err := json.Unmarshal(raw, &t); err != nil {
			continue
		}
		switch t.Type {
		case "namespace":
			c.convert(t.Tools, t.Name)
		case "function", "":
			if t.Name == "" {
				continue
			}
			c.addFunction(t.Name, t.Description, t.Parameters, namespace, false, "")
		case "custom":
			if t.Name == "" {
				continue
			}
			definition := ""
			if t.Format != nil {
				definition = t.Format.Definition
			}
			c.addFunction(t.Name, t.Description, nil, namespace, true, definition)
		default:
			c.skipped = append(c.skipped, t.Type)
		}
	}
}

func (c *toolConverter) addFunction(name, description string, params json.RawMessage, namespace string, custom bool, definition string) {
	chatName := c.uniqueChatName(chatToolName(namespace, name))

	info := toolInfo{Name: name, Custom: custom}
	if namespace != "" && !strings.EqualFold(namespace, "functions") {
		info.Namespace = namespace
	}
	c.info[chatName] = info
	c.origToChat[origKey(namespace, name)] = chatName

	if custom {
		var b strings.Builder
		if description != "" {
			b.WriteString(description)
			b.WriteString("\n\n")
		}
		b.WriteString(`This is a freeform tool. Call it with one JSON argument: {"input": "<raw tool input>"}.`)
		if definition != "" {
			b.WriteString("\nThe input must conform to this grammar:\n")
			b.WriteString(definition)
		}
		description = b.String()
		params = json.RawMessage(`{"type":"object","properties":{"input":{"type":"string","description":"Raw freeform input for this tool"}},"required":["input"],"additionalProperties":false}`)
	} else if len(params) == 0 || !json.Valid(params) {
		params = json.RawMessage(`{"type":"object","properties":{}}`)
	}

	c.tools = append(c.tools, chatTool{
		Type: "function",
		Function: chatToolDef{
			Name:        chatName,
			Description: description,
			Parameters:  params,
		},
	})
}

func (c *toolConverter) uniqueChatName(base string) string {
	name := base
	for i := 2; ; i++ {
		if _, exists := c.info[name]; !exists {
			return name
		}
		suffix := fmt.Sprintf("_%d", i)
		b := base
		if len(b)+len(suffix) > 128 {
			b = b[:128-len(suffix)]
		}
		name = b + suffix
	}
}

func (c *toolConverter) historyName(namespace, name string) string {
	if v, ok := c.origToChat[origKey(namespace, name)]; ok {
		return v
	}
	return sanitizeToolName(chatToolName(namespace, name))
}

func origKey(namespace, name string) string {
	if strings.EqualFold(namespace, "functions") {
		namespace = ""
	}
	return namespace + "\x00" + name
}

func chatToolName(namespace, name string) string {
	if namespace != "" && !strings.EqualFold(namespace, "functions") {
		return sanitizeToolName(namespace + "__" + name)
	}
	return sanitizeToolName(name)
}

func sanitizeToolName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		out = "tool"
	}
	if len(out) > 128 {
		out = out[:128]
	}
	return out
}

type respContentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	ImageURL json.RawMessage `json:"image_url"`
	Detail   string          `json:"detail"`
}

func extractRespContent(raw json.RawMessage) (text string, images []any) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", nil
	}
	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(trimmed, &s) == nil {
			return s, nil
		}
		return "", nil
	}
	var parts []respContentPart
	if err := json.Unmarshal(trimmed, &parts); err != nil {
		return "", nil
	}
	var b strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text", "text", "summary_text":
			b.WriteString(p.Text)
		case "input_image":
			if url := imageURL(p.ImageURL); url != "" {
				img := map[string]any{"url": url}
				if p.Detail != "" {
					img["detail"] = p.Detail
				}
				images = append(images, map[string]any{"type": "image_url", "image_url": img})
			}
		}
	}
	return b.String(), images
}

func imageURL(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(trimmed, &s) == nil {
		return s
	}
	var obj struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(trimmed, &obj) == nil {
		return obj.URL
	}
	return ""
}

func extractToolOutput(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(trimmed, &s) == nil {
			return s
		}
		return ""
	}
	var parts []respContentPart
	if json.Unmarshal(trimmed, &parts) == nil {
		var texts []string
		for _, p := range parts {
			if p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
		return strings.Join(texts, "\n")
	}
	return string(trimmed)
}

func rawText(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(trimmed, &s) == nil {
		return s
	}
	return string(trimmed)
}

func effortValue(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.ToLower(s)
	}
	var n float64
	if json.Unmarshal(raw, &n) == nil {
		return "high"
	}
	return ""
}

func mapEffort(effort string) string {
	switch effort {
	case "none":
		return "none"
	case "minimal", "low":
		return "low"
	case "medium", "high":
		return "high"
	case "xhigh", "max":
		return "max"
	default:
		return "high"
	}
}

func applyThinking(chatReq map[string]any, req *responsesRequest, mode string) {
	effort := ""
	if req.Reasoning != nil {
		effort = effortValue(req.Reasoning.Effort)
	}
	switch mode {
	case "disabled":
		chatReq["thinking"] = map[string]any{"type": "disabled"}
	case "enabled":
		chatReq["thinking"] = map[string]any{"type": "enabled"}
		if effort != "" && effort != "none" {
			chatReq["reasoning_effort"] = mapEffort(effort)
		}
	default:
		if effort == "none" {
			chatReq["thinking"] = map[string]any{"type": "disabled"}
		} else if effort != "" {
			chatReq["thinking"] = map[string]any{"type": "enabled"}
			chatReq["reasoning_effort"] = mapEffort(effort)
		}
	}
}

func (s *Server) buildResponsesPlan(req *responsesRequest) (*responsesPlan, error) {
	mapped := s.mapper.Map(req.Model)
	conv := newToolConverter()
	conv.convert(req.Tools, "")

	var msgs []chatMessage
	var systemParts []string
	if strings.TrimSpace(req.Instructions) != "" {
		systemParts = append(systemParts, req.Instructions)
	}

	input := bytes.TrimSpace(req.Input)
	if len(input) == 0 {
		return nil, errors.New("input is required")
	}

	lastAssistant := -1
	nonSystem := 0

	if input[0] == '"' {
		var text string
		if err := json.Unmarshal(input, &text); err != nil {
			return nil, errors.New("input must be a string or an array of items")
		}
		msgs = append(msgs, chatMessage{Role: "user", Content: text})
		nonSystem++
	} else {
		var items []responseInputItem
		if err := json.Unmarshal(input, &items); err != nil {
			return nil, errors.New("input must be a string or an array of items")
		}
		for _, item := range items {
			typ := item.Type
			if typ == "" && item.Role != "" {
				typ = "message"
			}
			switch typ {
			case "message":
				role := strings.ToLower(item.Role)
				text, images := extractRespContent(item.Content)
				switch role {
				case "system", "developer":
					if text != "" {
						systemParts = append(systemParts, text)
					}
					lastAssistant = -1
				case "assistant":
					msg := chatMessage{Role: "assistant"}
					if text != "" {
						msg.Content = text
					}
					msgs = append(msgs, msg)
					lastAssistant = len(msgs) - 1
					nonSystem++
				default:
					content := any(text)
					if len(images) > 0 {
						parts := make([]any, 0, len(images)+1)
						if text != "" {
							parts = append(parts, map[string]any{"type": "text", "text": text})
						}
						parts = append(parts, images...)
						content = parts
					}
					msgs = append(msgs, chatMessage{Role: "user", Content: content})
					lastAssistant = -1
					nonSystem++
				}

			case "function_call", "custom_tool_call":
				idx := lastAssistant
				if idx < 0 || idx >= len(msgs) || msgs[idx].Role != "assistant" {
					msgs = append(msgs, chatMessage{Role: "assistant"})
					idx = len(msgs) - 1
				}
				callID := item.CallID
				if callID == "" {
					callID = "call_" + newRequestID()
				}
				name := conv.historyName(item.Namespace, item.Name)
				var args string
				if typ == "custom_tool_call" {
					input := rawText(item.Input)
					b, _ := json.Marshal(map[string]string{"input": input})
					args = string(b)
				} else {
					args = rawText(item.Arguments)
					if args == "" {
						args = "{}"
					}
				}
				msgs[idx].ToolCalls = append(msgs[idx].ToolCalls, chatToolCall{
					ID:   callID,
					Type: "function",
					Function: chatFunctionCall{
						Name:      name,
						Arguments: args,
					},
				})
				lastAssistant = idx

			case "function_call_output", "custom_tool_call_output":
				msgs = append(msgs, chatMessage{
					Role:       "tool",
					ToolCallID: item.CallID,
					Content:    extractToolOutput(item.Output),
				})
				lastAssistant = -1
				nonSystem++

			case "reasoning", "local_shell_call", "web_search_call", "image_generation_call", "other":
				// Not representable in chat completions; drop.
			default:
				// Ignore unknown item types rather than failing the whole turn.
			}
		}
	}

	if nonSystem == 0 {
		return nil, errors.New("input contained no user, assistant or tool messages")
	}

	systemExtra := ""
	if req.Text != nil && req.Text.Format != nil {
		switch req.Text.Format.Type {
		case "json_schema":
			systemExtra = "\n\nRespond with a single valid JSON object."
			if len(bytes.TrimSpace(req.Text.Format.Schema)) > 0 {
				systemExtra += " It must conform to this JSON Schema:\n" + string(req.Text.Format.Schema)
			}
		case "json_object":
			systemExtra = "\n\nRespond with valid JSON."
		}
	}
	system := strings.Join(systemParts, "\n\n")
	if systemExtra != "" {
		system += systemExtra
	}
	if strings.TrimSpace(system) != "" {
		msgs = append([]chatMessage{{Role: "system", Content: system}}, msgs...)
	}

	chatReq := map[string]any{
		"model":    mapped,
		"messages": msgs,
	}
	if req.Stream {
		chatReq["stream"] = true
		chatReq["stream_options"] = map[string]any{"include_usage": true}
	}
	if req.MaxOutputTokens != nil {
		n := *req.MaxOutputTokens
		if n > 393216 {
			n = 393216
		}
		if cap := s.cfg.Upstream.MaxTokensCap; cap > 0 && n > cap {
			n = cap
		}
		if n > 0 {
			chatReq["max_tokens"] = n
		}
	}
	if req.Temperature != nil {
		chatReq["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		chatReq["top_p"] = *req.TopP
	}

	if len(conv.tools) > 0 {
		chatReq["tools"] = conv.tools
		choice := "auto"
		var tc string
		if len(req.ToolChoice) > 0 && json.Unmarshal(req.ToolChoice, &tc) == nil {
			switch strings.ToLower(tc) {
			case "none":
				choice = "none"
			case "auto":
				choice = "auto"
			case "required":
				// DeepSeek rejects required/named tool choices in thinking mode.
				choice = "auto"
			}
		}
		chatReq["tool_choice"] = choice
	}

	applyThinking(chatReq, req, s.cfg.Responses.Thinking)

	if req.Text != nil && req.Text.Format != nil {
		switch req.Text.Format.Type {
		case "json_schema", "json_object":
			chatReq["response_format"] = map[string]any{"type": "json_object"}
		}
	}

	if req.PromptCacheKey != nil && *req.PromptCacheKey != "" {
		chatReq["user_id"] = "dspool-" + keyutil.Fingerprint(*req.PromptCacheKey)
	}

	body, err := json.Marshal(chatReq)
	if err != nil {
		return nil, err
	}
	return &responsesPlan{
		Body:           body,
		RequestedModel: req.Model,
		Model:          mapped,
		Stream:         req.Stream,
		ToolInfo:       conv.info,
		OrigToChat:     conv.origToChat,
		SkippedTools:   conv.skipped,
	}, nil
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readRequestBody(w, r, fmtOpenAI)
	if !ok {
		return
	}
	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, fmtOpenAI, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error(), 0)
		return
	}
	if strings.TrimSpace(req.PreviousResponseID) != "" {
		writeError(w, fmtOpenAI, http.StatusBadRequest, "invalid_request_error",
			"previous_response_id is not supported; send the full conversation in input", 0)
		return
	}
	plan, err := s.buildResponsesPlan(&req)
	if err != nil {
		writeError(w, fmtOpenAI, http.StatusBadRequest, "invalid_request_error", err.Error(), 0)
		return
	}
	if len(plan.SkippedTools) > 0 {
		s.log.Debug("ignoring tools unsupported by chat completions",
			"id", requestID(r.Context()), "types", plan.SkippedTools)
	}
	if len(plan.ToolInfo) > 0 {
		s.log.Debug("responses translation",
			"id", requestID(r.Context()),
			"requested_model", plan.RequestedModel,
			"upstream_model", plan.Model,
			"tools", len(plan.ToolInfo),
			"stream", req.Stream,
		)
	}
	url := joinUpstreamURL(s.cfg.Upstream.OpenAIBase, "/chat/completions", r.URL.RawQuery)
	resp, ok := s.upstreamWithFailover(w, r, url, plan.Body, false, fmtOpenAI)
	if !ok {
		return
	}
	if req.Stream {
		s.relayResponsesStream(w, r, resp, plan)
	} else {
		s.relayResponsesJSON(w, resp, plan, &req)
	}
}

type chatUsage struct {
	PromptTokens         int64 `json:"prompt_tokens"`
	CompletionTokens     int64 `json:"completion_tokens"`
	TotalTokens          int64 `json:"total_tokens"`
	PromptCacheHitTokens int64 `json:"prompt_cache_hit_tokens"`
	PromptTokensDetails  *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

type chatError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

type chatToolCallResp struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatCompletion struct {
	ID      string `json:"id"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Role             string             `json:"role"`
			Content          *string            `json:"content"`
			ReasoningContent *string            `json:"reasoning_content"`
			ToolCalls        []chatToolCallResp `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage `json:"usage"`
	Error *chatError `json:"error"`
}

type chatStreamChunk struct {
	ID      string `json:"id"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string             `json:"role"`
			Content          *string            `json:"content"`
			ReasoningContent *string            `json:"reasoning_content"`
			ToolCalls        []chatToolCallResp `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage `json:"usage"`
	Error *chatError `json:"error"`
}

func usageMap(u *chatUsage) map[string]any {
	cached := int64(0)
	reasoning := int64(0)
	var input, output, total int64
	if u != nil {
		input = u.PromptTokens
		output = u.CompletionTokens
		total = u.TotalTokens
		cached = u.PromptCacheHitTokens
		if u.PromptTokensDetails != nil {
			cached = u.PromptTokensDetails.CachedTokens
		}
		if u.CompletionTokensDetails != nil {
			reasoning = u.CompletionTokensDetails.ReasoningTokens
		}
	}
	if total == 0 {
		total = input + output
	}
	return map[string]any{
		"input_tokens":          input,
		"input_tokens_details":  map[string]any{"cached_tokens": cached},
		"output_tokens":         output,
		"output_tokens_details": map[string]any{"reasoning_tokens": reasoning},
		"total_tokens":          total,
	}
}

func extractCustomInput(args string) string {
	args = strings.TrimSpace(args)
	if args == "" {
		return ""
	}
	var obj struct {
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal([]byte(args), &obj); err == nil && len(obj.Input) > 0 {
		var s string
		if json.Unmarshal(obj.Input, &s) == nil {
			return s
		}
		return string(obj.Input)
	}
	return args
}

func (p *responsesPlan) toolInfoFor(call chatToolCallResp) toolInfo {
	if info, ok := p.ToolInfo[call.Function.Name]; ok {
		return info
	}
	return toolInfo{Name: call.Function.Name}
}

type outputItemBuilder struct {
	kind      string
	index     int
	id        string
	callID    string
	name      string
	namespace string
	text      strings.Builder
	args      strings.Builder
}

func (b *outputItemBuilder) doneJSON() map[string]any {
	switch b.kind {
	case "message":
		return map[string]any{
			"id":      b.id,
			"type":    "message",
			"status":  "completed",
			"role":    "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": b.text.String()}},
		}
	case "function_call":
		m := map[string]any{
			"id":        b.id,
			"type":      "function_call",
			"status":    "completed",
			"name":      b.name,
			"call_id":   b.callID,
			"arguments": b.args.String(),
		}
		if b.namespace != "" {
			m["namespace"] = b.namespace
		}
		return m
	case "custom_tool_call":
		return map[string]any{
			"id":      b.id,
			"type":    "custom_tool_call",
			"status":  "completed",
			"name":    b.name,
			"call_id": b.callID,
			"input":   extractCustomInput(b.args.String()),
		}
	default:
		return map[string]any{"type": "other"}
	}
}

type responsesStreamer struct {
	w  http.ResponseWriter
	rc *http.ResponseController

	plan      *responsesPlan
	respID    string
	createdAt int64
	seq       int
	err       error
	failed    bool

	output      []*outputItemBuilder
	textItem    *outputItemBuilder
	toolItems   map[int]*outputItemBuilder
	pendingArgs map[int]string
	usage       *chatUsage
}

func newResponsesStreamer(w http.ResponseWriter, plan *responsesPlan, createdAt int64) *responsesStreamer {
	return &responsesStreamer{
		w:           w,
		rc:          http.NewResponseController(w),
		plan:        plan,
		respID:      "resp_" + newRequestID(),
		createdAt:   createdAt,
		toolItems:   map[int]*outputItemBuilder{},
		pendingArgs: map[int]string{},
	}
}

func (st *responsesStreamer) emit(kind string, payload map[string]any) {
	if st.err != nil {
		return
	}
	payload["type"] = kind
	payload["sequence_number"] = st.seq
	st.seq++
	data, err := json.Marshal(payload)
	if err != nil {
		st.err = err
		return
	}
	if _, err := fmt.Fprintf(st.w, "event: %s\ndata: %s\n\n", kind, data); err != nil {
		st.err = err
		return
	}
	if err := st.rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		st.err = err
	}
}

func (st *responsesStreamer) responseObject(status string, includeOutput bool) map[string]any {
	obj := map[string]any{
		"id":                 st.respID,
		"object":             "response",
		"created_at":         st.createdAt,
		"status":             status,
		"model":              st.plan.Model,
		"error":              nil,
		"incomplete_details": nil,
		"output":             []any{},
	}
	if includeOutput {
		out := make([]any, 0, len(st.output))
		for _, item := range st.output {
			out = append(out, item.doneJSON())
		}
		obj["output"] = out
	}
	if st.usage != nil {
		obj["usage"] = usageMap(st.usage)
	}
	return obj
}

func (st *responsesStreamer) start() {
	st.emit("response.created", map[string]any{"response": st.responseObject("in_progress", false)})
	st.emit("response.in_progress", map[string]any{"response": st.responseObject("in_progress", false)})
}

func (st *responsesStreamer) emitError(msg string) {
	if st.failed {
		return
	}
	st.failed = true
	st.emit("error", map[string]any{
		"error": map[string]any{
			"type":    "server_error",
			"code":    nil,
			"message": msg,
		},
	})
}

func (st *responsesStreamer) appendText(delta string) {
	if delta == "" {
		return
	}
	if st.textItem == nil {
		item := &outputItemBuilder{
			kind:  "message",
			index: len(st.output),
			id:    "msg_" + newRequestID(),
			name:  "assistant",
		}
		st.output = append(st.output, item)
		st.textItem = item
		st.emit("response.output_item.added", map[string]any{
			"output_index": item.index,
			"item": map[string]any{
				"id":      item.id,
				"type":    "message",
				"status":  "in_progress",
				"role":    "assistant",
				"content": []any{},
			},
		})
		st.emit("response.content_part.added", map[string]any{
			"item_id":       item.id,
			"output_index":  item.index,
			"content_index": 0,
			"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
	}
	st.textItem.text.WriteString(delta)
	st.emit("response.output_text.delta", map[string]any{
		"item_id":       st.textItem.id,
		"output_index":  st.textItem.index,
		"content_index": 0,
		"delta":         delta,
		"logprobs":      []any{},
	})
}

func (st *responsesStreamer) appendToolCall(call chatToolCallResp) {
	item := st.toolItems[call.Index]
	if item == nil {
		name := call.Function.Name
		if name == "" {
			st.pendingArgs[call.Index] += call.Function.Arguments
			return
		}
		info := st.plan.toolInfoFor(call)
		callID := call.ID
		if callID == "" {
			callID = "call_" + newRequestID()
		}
		kind := "function_call"
		idPrefix := "fc_"
		if info.Custom {
			kind = "custom_tool_call"
			idPrefix = "ctc_"
		}
		item = &outputItemBuilder{
			kind:      kind,
			index:     len(st.output),
			id:        idPrefix + newRequestID(),
			callID:    callID,
			name:      info.Name,
			namespace: info.Namespace,
		}
		st.output = append(st.output, item)
		st.toolItems[call.Index] = item
		if kind == "function_call" {
			added := map[string]any{
				"id":        item.id,
				"type":      "function_call",
				"status":    "in_progress",
				"name":      item.name,
				"call_id":   item.callID,
				"arguments": "",
			}
			if item.namespace != "" {
				added["namespace"] = item.namespace
			}
			st.emit("response.output_item.added", map[string]any{
				"output_index": item.index,
				"item":         added,
			})
		}
		if pre := st.pendingArgs[call.Index]; pre != "" {
			item.args.WriteString(pre)
			delete(st.pendingArgs, call.Index)
			if item.kind == "function_call" {
				st.emit("response.function_call_arguments.delta", map[string]any{
					"item_id":      item.id,
					"output_index": item.index,
					"delta":        pre,
				})
			}
		}
	}
	if call.ID != "" && item.callID == "" {
		item.callID = call.ID
	}
	if call.Function.Arguments != "" {
		item.args.WriteString(call.Function.Arguments)
		if item.kind == "function_call" {
			st.emit("response.function_call_arguments.delta", map[string]any{
				"item_id":      item.id,
				"output_index": item.index,
				"delta":        call.Function.Arguments,
			})
		}
	}
}

func (st *responsesStreamer) finish() {
	if st.failed {
		return
	}
	for _, item := range st.output {
		switch item.kind {
		case "message":
			text := item.text.String()
			st.emit("response.output_text.done", map[string]any{
				"item_id":       item.id,
				"output_index":  item.index,
				"content_index": 0,
				"text":          text,
				"logprobs":      []any{},
			})
			st.emit("response.content_part.done", map[string]any{
				"item_id":       item.id,
				"output_index":  item.index,
				"content_index": 0,
				"part":          map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
			})
			st.emit("response.output_item.done", map[string]any{
				"output_index": item.index,
				"item":         item.doneJSON(),
			})
		case "function_call":
			st.emit("response.function_call_arguments.done", map[string]any{
				"item_id":      item.id,
				"output_index": item.index,
				"arguments":    item.args.String(),
			})
			st.emit("response.output_item.done", map[string]any{
				"output_index": item.index,
				"item":         item.doneJSON(),
			})
		case "custom_tool_call":
			// Emitting only the completed item keeps freeform input handling
			// simple; Codex executes custom tools from output_item.done.
			st.emit("response.output_item.done", map[string]any{
				"output_index": item.index,
				"item":         item.doneJSON(),
			})
		}
	}
	st.emit("response.completed", map[string]any{
		"response": st.responseObject("completed", true),
	})
}

func (st *responsesStreamer) handleChunk(chunk *chatStreamChunk) {
	if chunk.Usage != nil {
		st.usage = chunk.Usage
	}
	if len(chunk.Choices) == 0 {
		return
	}
	choice := chunk.Choices[0]
	if choice.Delta.Content != nil {
		st.appendText(*choice.Delta.Content)
	}
	for _, call := range choice.Delta.ToolCalls {
		st.appendToolCall(call)
	}
}

func (s *Server) relayResponsesStream(w http.ResponseWriter, r *http.Request, resp *http.Response, plan *responsesPlan) {
	defer resp.Body.Close()

	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	isSSE := strings.Contains(contentType, "text/event-stream")

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	st := newResponsesStreamer(w, plan, time.Now().Unix())
	st.start()

	if !isSSE {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		st.emitError("upstream did not return an event stream: " + snippet(body))
		return
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 32<<20)
	var data []byte
	done := false

	handleData := func() bool {
		if len(data) == 0 {
			return false
		}
		payload := bytes.TrimSpace(data)
		data = data[:0]
		if bytes.Equal(payload, []byte("[DONE]")) {
			return true
		}
		var chunk chatStreamChunk
		if err := json.Unmarshal(payload, &chunk); err != nil {
			st.emitError("invalid upstream stream event: " + err.Error())
			return true
		}
		if chunk.Error != nil {
			msg := chunk.Error.Message
			if msg == "" {
				msg = "upstream stream error"
			}
			st.emitError(msg)
			return true
		}
		st.handleChunk(&chunk)
		return st.err != nil
	}

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			if handleData() {
				done = true
				break
			}
			continue
		}
		if line[0] == ':' {
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			piece := bytes.TrimSpace(line[5:])
			if len(piece) > 0 {
				if len(data) > 0 {
					data = append(data, '\n')
				}
				data = append(data, piece...)
			}
		}
		if st.err != nil {
			return
		}
	}
	if !done && st.err == nil {
		handleData()
	}
	if err := scanner.Err(); err != nil && st.err == nil && !st.failed {
		st.emitError("upstream stream read error: " + err.Error())
	}
	if st.err != nil {
		s.log.Debug("responses stream terminated", "error", st.err)
		return
	}
	st.finish()
}

func (s *Server) relayResponsesJSON(w http.ResponseWriter, resp *http.Response, plan *responsesPlan, req *responsesRequest) {
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRelayBytes+1))
	if err != nil {
		writeError(w, fmtOpenAI, http.StatusBadGateway, "server_error", "failed to read upstream response: "+err.Error(), 0)
		return
	}
	if len(body) > maxRelayBytes {
		writeError(w, fmtOpenAI, http.StatusBadGateway, "server_error", "upstream response too large", 0)
		return
	}
	var cc chatCompletion
	if err := json.Unmarshal(body, &cc); err != nil {
		writeError(w, fmtOpenAI, http.StatusBadGateway, "server_error", "invalid upstream response: "+err.Error(), 0)
		return
	}
	if cc.Error != nil {
		writeError(w, fmtOpenAI, http.StatusBadGateway, "server_error", cc.Error.Message, 0)
		return
	}
	if len(cc.Choices) == 0 {
		writeError(w, fmtOpenAI, http.StatusBadGateway, "server_error", "upstream returned no choices", 0)
		return
	}

	choice := cc.Choices[0]
	var output []any
	if choice.Message.Content != nil && *choice.Message.Content != "" {
		output = append(output, map[string]any{
			"id":      "msg_" + newRequestID(),
			"type":    "message",
			"status":  "completed",
			"role":    "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": *choice.Message.Content}},
		})
	}
	for _, call := range choice.Message.ToolCalls {
		output = append(output, outputItemForToolCall(call, plan))
	}
	if len(output) == 0 {
		output = append(output, map[string]any{
			"id":      "msg_" + newRequestID(),
			"type":    "message",
			"status":  "completed",
			"role":    "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": ""}},
		})
	}

	parallel := true
	if req.ParallelToolCalls != nil {
		parallel = *req.ParallelToolCalls
	}
	payload := map[string]any{
		"id":                   "resp_" + newRequestID(),
		"object":               "response",
		"created_at":           time.Now().Unix(),
		"status":               "completed",
		"model":                plan.Model,
		"output":               output,
		"usage":                usageMap(cc.Usage),
		"error":                nil,
		"incomplete_details":   nil,
		"instructions":         nil,
		"max_output_tokens":    req.MaxOutputTokens,
		"parallel_tool_calls":  parallel,
		"previous_response_id": nil,
		"reasoning":            nil,
		"store":                false,
		"temperature":          req.Temperature,
		"text":                 nil,
		"tool_choice":          "auto",
		"tools":                nil,
		"top_p":                req.TopP,
		"truncation":           "disabled",
		"metadata":             map[string]any{},
	}
	writeJSON(w, http.StatusOK, payload)
}

func outputItemForToolCall(call chatToolCallResp, plan *responsesPlan) map[string]any {
	info := plan.toolInfoFor(call)
	callID := call.ID
	if callID == "" {
		callID = "call_" + newRequestID()
	}
	if info.Custom {
		return map[string]any{
			"id":      "ctc_" + newRequestID(),
			"type":    "custom_tool_call",
			"status":  "completed",
			"name":    info.Name,
			"call_id": callID,
			"input":   extractCustomInput(call.Function.Arguments),
		}
	}
	item := map[string]any{
		"id":        "fc_" + newRequestID(),
		"type":      "function_call",
		"status":    "completed",
		"name":      info.Name,
		"call_id":   callID,
		"arguments": call.Function.Arguments,
	}
	if info.Namespace != "" {
		item["namespace"] = info.Namespace
	}
	return item
}
