package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"dspool/internal/balance"
	"dspool/internal/config"
	"dspool/internal/keyutil"
	"dspool/internal/monitor"
	"dspool/internal/pool"
	"dspool/internal/state"
)

type testEnv struct {
	cfg      *config.Config
	pool     *pool.Pool
	server   *Server
	proxy    *httptest.Server
	upstream *httptest.Server
}

func newTestEnv(t *testing.T, upstreamHandler http.HandlerFunc, balances map[string]float64) *testEnv {
	t.Helper()
	upstream := httptest.NewServer(upstreamHandler)
	t.Cleanup(upstream.Close)

	cfg := config.Default()
	cfg.Listen = "127.0.0.1:0"
	cfg.ClientKeys = []string{"local-token"}
	cfg.Upstream.OpenAIBase = upstream.URL + "/v1"
	cfg.Upstream.AnthropicBase = upstream.URL + "/anthropic"
	cfg.Upstream.BalanceURL = upstream.URL + "/user/balance"
	cfg.Upstream.RequestHeaderTimeout = config.Duration(10 * time.Second)
	cfg.Upstream.ProbeTimeout = config.Duration(2 * time.Second)
	cfg.Pool.Strategy = "highest"
	cfg.Models.Default = "deepseek-flash"
	cfg.Models.Small = "deepseek-flash"
	cfg.Models.Map = map[string]string{}
	cfg.Responses.Thinking = "auto"

	secrets := make([]string, 0, len(balances))
	for secret := range balances {
		secrets = append(secrets, secret)
	}
	sort.Strings(secrets)
	specs := make([]pool.Spec, 0, len(secrets))
	for i, secret := range secrets {
		specs = append(specs, pool.Spec{Label: fmt.Sprintf("key-%d", i+1), Secret: secret})
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := state.Open(filepath.Join(t.TempDir(), "state.json"), log)
	if err != nil {
		t.Fatal(err)
	}

	p := pool.New(pool.Options{
		Strategy:          pool.ParseStrategy(cfg.Pool.Strategy),
		InflightPenalty:   cfg.Pool.InflightPenalty,
		BalanceInterval:   time.Minute,
		ColdCheckInterval: time.Minute,
		RateLimitCooldown: time.Second,
		ErrorCooldown:     time.Second,
	}, specs, st)
	for i, secret := range secrets {
		p.ApplyProbe(keyutil.Fingerprint(secret), balance.Result{
			Available: true,
			Known:     true,
			Total:     balances[secret],
			Currency:  "CNY",
		}, nil, time.Now())
		_ = i
	}

	mon := monitor.New(p, balance.NewClient(cfg.Upstream.BalanceURL, time.Second), monitor.Options{
		ProbeTimeout: time.Second,
		Concurrency:  2,
		Tick:         time.Second,
	}, log)

	cfgPtr := &cfg
	server, err := New(cfgPtr, p, mon, st, "test", log)
	if err != nil {
		t.Fatal(err)
	}
	proxyServer := httptest.NewServer(server.Handler())
	t.Cleanup(proxyServer.Close)

	return &testEnv{cfg: cfgPtr, pool: p, server: server, proxy: proxyServer, upstream: upstream}
}

func (e *testEnv) do(t *testing.T, method, path, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, e.proxy.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestProxyAuthAndDashboard(t *testing.T) {
	env := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user/balance" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"42.00"}]}`))
			return
		}
		http.NotFound(w, r)
	}, map[string]float64{"sk-one": 100})

	resp := env.do(t, http.MethodGet, "/", "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("dashboard status = %d", resp.StatusCode)
	}
	html := readAll(t, resp)
	if !strings.Contains(html, "dspool") || !strings.Contains(html, "dashboard/data") {
		t.Fatalf("unexpected dashboard HTML")
	}

	resp = env.do(t, http.MethodGet, "/dashboard/data", "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("dashboard data status = %d", resp.StatusCode)
	}
	data := readAll(t, resp)
	if strings.Contains(data, "sk-one") {
		t.Fatalf("dashboard leaked the full secret: %s", data)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		t.Fatal(err)
	}
	counts, _ := payload["counts"].(map[string]any)
	if counts["hot"] != float64(1) {
		t.Fatalf("counts = %v", counts)
	}

	resp = env.do(t, http.MethodGet, "/healthz", "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("health status = %d", resp.StatusCode)
	}
	_ = readAll(t, resp)

	resp = env.do(t, http.MethodGet, "/admin/status", "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("admin status without token on loopback = %d", resp.StatusCode)
	}
	_ = readAll(t, resp)

	resp = env.do(t, http.MethodGet, "/v1/models", "", "")
	if resp.StatusCode != 401 {
		t.Fatalf("models without token = %d", resp.StatusCode)
	}
	_ = readAll(t, resp)

	resp = env.do(t, http.MethodGet, "/v1/models", "local-token", "")
	if resp.StatusCode != 200 {
		t.Fatalf("models status = %d", resp.StatusCode)
	}
	models := readAll(t, resp)
	if !strings.Contains(models, "deepseek-flash") {
		t.Fatalf("models = %s", models)
	}

	resp = env.do(t, http.MethodPost, "/dashboard/refresh", "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("dashboard refresh status = %d", resp.StatusCode)
	}
	refreshed := readAll(t, resp)
	if !strings.Contains(refreshed, `"probed"`) {
		t.Fatalf("refresh response = %s", refreshed)
	}

	env.server.SetKeyReloader(func() (pool.KeyDiff, error) {
		return pool.KeyDiff{Added: []pool.Snapshot{{Label: "new-key"}}}, nil
	})
	resp = env.do(t, http.MethodPost, "/dashboard/reload", "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("dashboard reload status = %d", resp.StatusCode)
	}
	reloaded := readAll(t, resp)
	if !strings.Contains(reloaded, `"added":1`) || !strings.Contains(reloaded, `"removed":0`) {
		t.Fatalf("reload response = %s", reloaded)
	}

	env.server.SetKeyReloader(func() (pool.KeyDiff, error) {
		return pool.KeyDiff{}, fmt.Errorf("config.yaml: unexpected end of file")
	})
	resp = env.do(t, http.MethodPost, "/dashboard/reload", "", "")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("failed reload status = %d", resp.StatusCode)
	}
	failed := readAll(t, resp)
	if !strings.Contains(failed, "unexpected end of file") {
		t.Fatalf("failed reload response = %s", failed)
	}
}

type upstreamLog struct {
	mu     sync.Mutex
	calls  []string
	models []string
}

func (u *upstreamLog) add(key, model string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls = append(u.calls, key)
	u.models = append(u.models, model)
}

func (u *upstreamLog) snapshot() ([]string, []string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.calls...), append([]string(nil), u.models...)
}

func TestChatFailoverAndModelRewrite(t *testing.T) {
	log := &upstreamLog{}
	upstream := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user/balance" {
			_, _ = w.Write([]byte(`{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"100"}]}`))
			return
		}
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		model, _ := req["model"].(string)
		log.add(key, model)

		if key == "sk-one" {
			w.WriteHeader(http.StatusPaymentRequired)
			_, _ = w.Write([]byte(`{"error":{"message":"Insufficient Balance"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1,"model":"deepseek-flash","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`))
	}

	env := newTestEnv(t, upstream, map[string]float64{"sk-one": 100, "sk-two": 90})
	body := `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`
	resp := env.do(t, http.MethodPost, "/v1/chat/completions", "local-token", body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d: %s", resp.StatusCode, readAll(t, resp))
	}
	if got := readAll(t, resp); !strings.Contains(got, "hello") {
		t.Fatalf("body = %s", got)
	}

	calls, models := log.snapshot()
	if len(calls) != 2 || calls[0] != "sk-one" || calls[1] != "sk-two" {
		t.Fatalf("upstream calls = %v", calls)
	}
	for _, m := range models {
		if m != "deepseek-flash" {
			t.Fatalf("model rewrite failed: %v", models)
		}
	}

	one, _ := env.pool.Snapshot(keyutil.Fingerprint("sk-one"))
	two, _ := env.pool.Snapshot(keyutil.Fingerprint("sk-two"))
	if one.State != pool.StateCold {
		t.Fatalf("key one state = %s, want cold", one.State)
	}
	if two.State != pool.StateHot || two.Stats.Successes != 1 {
		t.Fatalf("key two = %+v", two)
	}

	resp = env.do(t, http.MethodPost, "/v1/chat/completions", "local-token",
		`{"model":"deepseek-v4-pro[1m]","messages":[{"role":"user","content":"again"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("second status = %d", resp.StatusCode)
	}
	_ = readAll(t, resp)
	_, models = log.snapshot()
	if models[len(models)-1] != "deepseek-v4-pro" {
		t.Fatalf("models = %v", models)
	}
}

func TestChatRequiresClientToken(t *testing.T) {
	env := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}, map[string]float64{"sk-one": 100})
	body := `{"model":"deepseek-flash","messages":[{"role":"user","content":"hi"}]}`
	resp := env.do(t, http.MethodPost, "/v1/chat/completions", "", body)
	if resp.StatusCode != 401 {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	_ = readAll(t, resp)
	resp = env.do(t, http.MethodPost, "/v1/chat/completions", "wrong", body)
	if resp.StatusCode != 401 {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	_ = readAll(t, resp)
}

func TestAnthropicStreamFailoverAndQuery(t *testing.T) {
	var (
		mu       sync.Mutex
		queries  []string
		versions []string
	)
	upstream := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/anthropic/v1/messages" {
			http.NotFound(w, r)
			return
		}
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		queries = append(queries, r.URL.RawQuery)
		versions = append(versions, r.Header.Get("anthropic-version"))
		mu.Unlock()
		if key == "sk-one" {
			w.WriteHeader(http.StatusPaymentRequired)
			_, _ = w.Write([]byte(`{"error":{"message":"Insufficient Balance"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"deepseek-flash\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n")
		if fl != nil {
			fl.Flush()
		}
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n")
		if fl != nil {
			fl.Flush()
		}
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		if fl != nil {
			fl.Flush()
		}
	}

	env := newTestEnv(t, upstream, map[string]float64{"sk-one": 100, "sk-two": 90})
	reqBody := `{"model":"claude-sonnet-4-5","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp := env.do(t, http.MethodPost, "/v1/messages?beta=true", "local-token", reqBody)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d: %s", resp.StatusCode, readAll(t, resp))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content type = %q", ct)
	}
	body := readAll(t, resp)
	if !strings.Contains(body, `"text_delta"`) || !strings.Contains(body, "hi") {
		t.Fatalf("stream body = %s", body)
	}
	mu.Lock()
	if len(queries) != 2 || queries[0] != "beta=true" || queries[1] != "beta=true" {
		t.Fatalf("queries = %v", queries)
	}
	for _, v := range versions {
		if v == "" {
			t.Fatalf("anthropic-version not forwarded")
		}
	}
	mu.Unlock()

	one, _ := env.pool.Snapshot(keyutil.Fingerprint("sk-one"))
	if one.State != pool.StateCold {
		t.Fatalf("key one = %s", one.State)
	}
}

func TestResponsesStreamTranslation(t *testing.T) {
	type captured struct {
		mu   sync.Mutex
		body map[string]any
	}
	cap := &captured{}
	upstream := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("upstream body: %v", err)
		}
		cap.mu.Lock()
		cap.body = body
		cap.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		write := func(payload any) {
			b, _ := json.Marshal(payload)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
			if fl != nil {
				fl.Flush()
			}
		}
		chunk := func(delta map[string]any, finish any) map[string]any {
			return map[string]any{
				"id": "c1", "object": "chat.completion.chunk", "created": 1, "model": "deepseek-flash",
				"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
			}
		}
		write(chunk(map[string]any{"role": "assistant", "content": "Hello "}, nil))
		write(chunk(map[string]any{"content": "world"}, nil))
		write(chunk(map[string]any{"tool_calls": []any{
			map[string]any{"index": 0, "id": "call_a", "type": "function", "function": map[string]any{
				"name": "exec_command", "arguments": `{"cmd":`,
			}},
		}}, nil))
		write(chunk(map[string]any{"tool_calls": []any{
			map[string]any{"index": 0, "function": map[string]any{"arguments": `"ls"}`}},
		}}, nil))
		patchArgs, _ := json.Marshal(map[string]any{"input": "*** Begin Patch\n*** End Patch"})
		write(chunk(map[string]any{"tool_calls": []any{
			map[string]any{"index": 1, "id": "call_b", "type": "function", "function": map[string]any{
				"name": "apply_patch", "arguments": string(patchArgs),
			}},
		}}, nil))
		final := chunk(map[string]any{}, "tool_calls")
		final["usage"] = map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		write(final)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}

	env := newTestEnv(t, upstream, map[string]float64{"sk-one": 100})
	req := map[string]any{
		"model":  "gpt-5-codex",
		"stream": true,
		"input": []any{
			map[string]any{"type": "message", "role": "developer", "content": []any{map[string]any{"type": "input_text", "text": "sys"}}},
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}},
			map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "previous"}}},
			map[string]any{"type": "function_call", "name": "exec_command", "arguments": `{"cmd":"pwd"}`, "call_id": "call_prev"},
			map[string]any{"type": "function_call_output", "call_id": "call_prev", "output": "ok"},
			map[string]any{"type": "custom_tool_call", "name": "apply_patch", "input": "*** Begin Patch\n*** End Patch", "call_id": "call_patch"},
			map[string]any{"type": "custom_tool_call_output", "call_id": "call_patch", "output": "done"},
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "now"}}},
		},
		"tools": []any{
			map[string]any{"type": "function", "name": "exec_command", "description": "run", "parameters": map[string]any{"type": "object", "properties": map[string]any{"cmd": map[string]any{"type": "string"}}}},
			map[string]any{"type": "custom", "name": "apply_patch", "description": "patch files", "format": map[string]any{"type": "grammar", "syntax": "lark", "definition": "start: patch"}},
		},
		"tool_choice": "auto",
		"reasoning":   map[string]any{"effort": "medium"},
	}
	rawReq, _ := json.Marshal(req)
	resp := env.do(t, http.MethodPost, "/v1/responses", "local-token", string(rawReq))
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d: %s", resp.StatusCode, readAll(t, resp))
	}
	stream := readAll(t, resp)

	cap.mu.Lock()
	chatReq := cap.body
	cap.mu.Unlock()
	if chatReq == nil {
		t.Fatal("upstream request not captured")
	}
	if chatReq["model"] != "deepseek-flash" {
		t.Fatalf("model = %v", chatReq["model"])
	}
	if chatReq["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %v", chatReq["tool_choice"])
	}
	thinking, _ := chatReq["thinking"].(map[string]any)
	if thinking["type"] != "enabled" {
		t.Fatalf("thinking = %v", chatReq["thinking"])
	}
	if chatReq["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %v", chatReq["reasoning_effort"])
	}
	if so, _ := chatReq["stream_options"].(map[string]any); so["include_usage"] != true {
		t.Fatalf("stream_options = %v", chatReq["stream_options"])
	}
	tools, _ := chatReq["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools = %v", chatReq["tools"])
	}
	customTool := tools[1].(map[string]any)["function"].(map[string]any)
	desc, _ := customTool["description"].(string)
	if !strings.Contains(desc, "freeform") || !strings.Contains(desc, "start: patch") {
		t.Fatalf("custom tool description = %q", desc)
	}
	msgs, _ := chatReq["messages"].([]any)
	if len(msgs) != 7 {
		t.Fatalf("messages = %d: %v", len(msgs), msgs)
	}
	roles := make([]string, 0, len(msgs))
	for _, m := range msgs {
		roles = append(roles, m.(map[string]any)["role"].(string))
	}
	wantRoles := []string{"system", "user", "assistant", "tool", "assistant", "tool", "user"}
	for i := range wantRoles {
		if roles[i] != wantRoles[i] {
			t.Fatalf("roles = %v", roles)
		}
	}
	assistant := msgs[2].(map[string]any)
	toolCalls, _ := assistant["tool_calls"].([]any)
	if len(toolCalls) != 1 {
		t.Fatalf("assistant tool calls = %v", assistant["tool_calls"])
	}
	tc := toolCalls[0].(map[string]any)["function"].(map[string]any)
	if tc["name"] != "exec_command" || tc["arguments"] != `{"cmd":"pwd"}` {
		t.Fatalf("tool call = %v", tc)
	}
	toolMsg := msgs[3].(map[string]any)
	if toolMsg["tool_call_id"] != "call_prev" || toolMsg["content"] != "ok" {
		t.Fatalf("tool message = %v", toolMsg)
	}
	customAssistant := msgs[4].(map[string]any)
	customCalls, _ := customAssistant["tool_calls"].([]any)
	if len(customCalls) != 1 {
		t.Fatalf("custom assistant tool calls = %v", customAssistant["tool_calls"])
	}
	customFn := customCalls[0].(map[string]any)["function"].(map[string]any)
	if customFn["name"] != "apply_patch" {
		t.Fatalf("custom tool name = %v", customFn["name"])
	}
	var customArgs map[string]string
	if err := json.Unmarshal([]byte(customFn["arguments"].(string)), &customArgs); err != nil {
		t.Fatalf("custom arguments: %v", err)
	}
	if !strings.Contains(customArgs["input"], "*** Begin Patch") {
		t.Fatalf("custom arguments = %v", customArgs)
	}
	customToolMsg := msgs[5].(map[string]any)
	if customToolMsg["tool_call_id"] != "call_patch" || customToolMsg["content"] != "done" {
		t.Fatalf("custom tool message = %v", customToolMsg)
	}

	events := parseSSE(t, stream)
	if len(events) == 0 {
		t.Fatal("no SSE events")
	}
	if events[0]["type"] != "response.created" {
		t.Fatalf("first event = %v", events[0]["type"])
	}
	if events[len(events)-1]["type"] != "response.completed" {
		t.Fatalf("last event = %v", events[len(events)-1]["type"])
	}

	msgItem := findDoneItem(events, "message")
	if msgItem == nil {
		t.Fatalf("no message item done: %s", stream)
	}
	content := msgItem["content"].([]any)[0].(map[string]any)
	if content["text"] != "Hello world" {
		t.Fatalf("message text = %v", content["text"])
	}

	fnItem := findDoneItem(events, "function_call")
	if fnItem == nil {
		t.Fatalf("no function_call item done")
	}
	if fnItem["name"] != "exec_command" || fnItem["arguments"] != `{"cmd":"ls"}` {
		t.Fatalf("function item = %v", fnItem)
	}

	customItem := findDoneItem(events, "custom_tool_call")
	if customItem == nil {
		t.Fatalf("no custom_tool_call item done")
	}
	if customItem["name"] != "apply_patch" || !strings.Contains(customItem["input"].(string), "*** Begin Patch") {
		t.Fatalf("custom item = %v", customItem)
	}

	completed := events[len(events)-1]["response"].(map[string]any)
	usage := completed["usage"].(map[string]any)
	if usage["input_tokens"] != float64(10) || usage["output_tokens"] != float64(5) || usage["total_tokens"] != float64(15) {
		t.Fatalf("usage = %v", usage)
	}
}

func TestResponsesNonStream(t *testing.T) {
	upstream := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"c1","object":"chat.completion","created":1,"model":"deepseek-flash",
			"choices":[{"index":0,"message":{"role":"assistant","content":"json answer","tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"exec_command","arguments":"{\"cmd\":\"ls\"}"}}
			]},"finish_reason":"tool_calls"}],
			"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}
		}`))
	}
	env := newTestEnv(t, upstream, map[string]float64{"sk-one": 100})
	req, _ := json.Marshal(map[string]any{
		"model":  "gpt-5-codex",
		"stream": false,
		"input":  "hi",
		"tools": []any{
			map[string]any{"type": "function", "name": "exec_command", "parameters": map[string]any{"type": "object"}},
		},
	})
	resp := env.do(t, http.MethodPost, "/v1/responses", "local-token", string(req))
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d: %s", resp.StatusCode, readAll(t, resp))
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(readAll(t, resp)), &payload); err != nil {
		t.Fatal(err)
	}
	output, _ := payload["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("output = %v", payload["output"])
	}
	if output[0].(map[string]any)["type"] != "message" {
		t.Fatalf("output[0] = %v", output[0])
	}
	if output[1].(map[string]any)["type"] != "function_call" {
		t.Fatalf("output[1] = %v", output[1])
	}
	usage := payload["usage"].(map[string]any)
	if usage["input_tokens"] != float64(3) || usage["output_tokens"] != float64(4) {
		t.Fatalf("usage = %v", usage)
	}
}

func TestChatNormalizesDeveloperRoleAndMaxTokens(t *testing.T) {
	var (
		mu       sync.Mutex
		captured map[string]any
	)
	upstream := func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		captured = body
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}
	env := newTestEnv(t, upstream, map[string]float64{"sk-one": 100})
	req := `{"model":"deepseek-flash","max_completion_tokens":123,"messages":[{"role":"developer","content":"sys"},{"role":"user","content":"hi"}]}`
	resp := env.do(t, http.MethodPost, "/v1/chat/completions", "local-token", req)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d: %s", resp.StatusCode, readAll(t, resp))
	}
	_ = readAll(t, resp)
	mu.Lock()
	body := captured
	mu.Unlock()
	if body["max_tokens"] != float64(123) {
		t.Fatalf("max_tokens = %v", body["max_tokens"])
	}
	if _, exists := body["max_completion_tokens"]; exists {
		t.Fatalf("max_completion_tokens was not removed")
	}
	msgs := body["messages"].([]any)
	first := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("role = %v, want system", first["role"])
	}
}

func TestCountTokensIsLocal(t *testing.T) {
	var hits int
	upstream := func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Error(w, "should not be called", 500)
	}
	env := newTestEnv(t, upstream, map[string]float64{"sk-one": 100})
	resp := env.do(t, http.MethodPost, "/v1/messages/count_tokens", "local-token",
		`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello world"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d: %s", resp.StatusCode, readAll(t, resp))
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(readAll(t, resp)), &payload); err != nil {
		t.Fatal(err)
	}
	if n, _ := payload["input_tokens"].(float64); n < 1 {
		t.Fatalf("input_tokens = %v", payload["input_tokens"])
	}
	if hits != 0 {
		t.Fatalf("count_tokens hit upstream %d times", hits)
	}
}

func TestAllKeysOutOfBalanceReturns503(t *testing.T) {
	upstream := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user/balance" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":{"message":"Insufficient Balance"}}`))
	}
	env := newTestEnv(t, upstream, map[string]float64{"sk-one": 100, "sk-two": 90})
	resp := env.do(t, http.MethodPost, "/v1/chat/completions", "local-token",
		`{"model":"deepseek-flash","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	body := readAll(t, resp)
	if !strings.Contains(strings.ToLower(body), "insufficient") {
		t.Fatalf("body = %s", body)
	}
	counts := env.pool.Counts()
	if counts.Cold != 2 {
		t.Fatalf("counts = %+v", counts)
	}
}

func TestResponsesRejectsPreviousResponseID(t *testing.T) {
	env := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called")
	}, map[string]float64{"sk-one": 100})
	req := `{"model":"gpt-5","input":"hi","previous_response_id":"resp_123"}`
	resp := env.do(t, http.MethodPost, "/v1/responses", "local-token", req)
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	_ = readAll(t, resp)
}

func parseSSE(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, block := range strings.Split(raw, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var data []string
		for _, line := range strings.Split(block, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "data:") {
				data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		if len(data) == 0 {
			continue
		}
		payload := strings.Join(data, "\n")
		if payload == "[DONE]" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			t.Fatalf("bad SSE payload %q: %v", payload, err)
		}
		out = append(out, m)
	}
	return out
}

func findDoneItem(events []map[string]any, typ string) map[string]any {
	for _, ev := range events {
		if ev["type"] != "response.output_item.done" {
			continue
		}
		item, _ := ev["item"].(map[string]any)
		if item != nil && item["type"] == typ {
			return item
		}
	}
	return nil
}
