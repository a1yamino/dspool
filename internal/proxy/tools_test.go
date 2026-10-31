package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func balanceReply(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"100"}]}`))
}

func TestToolSearchProxy(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	upstream := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user/balance" {
			balanceReply(w)
			return
		}
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"title":"T","url":"https://example.com","snippet":"S"}]}`))
	}

	env := newTestEnv(t, upstream, map[string]float64{"sk-one": 100})
	input := `{"query":"dspool","count":2,"need_content":true}`
	resp := env.do(t, http.MethodPost, "/v1/tools/search", "local-token", input)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, readAll(t, resp))
	}
	body := readAll(t, resp)
	if !strings.Contains(body, `"title":"T"`) {
		t.Fatalf("body = %s", body)
	}
	if gotPath != "/v1/tools/search" {
		t.Fatalf("upstream path = %q", gotPath)
	}
	if gotAuth != "Bearer sk-one" {
		t.Fatalf("upstream auth = %q", gotAuth)
	}
	if gotBody != input {
		t.Fatalf("upstream body = %s", gotBody)
	}
}

func TestToolFetchProxyAndFailover(t *testing.T) {
	var calls []string
	upstream := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user/balance" {
			balanceReply(w)
			return
		}
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		calls = append(calls, key)
		if key == "sk-one" {
			w.WriteHeader(http.StatusPaymentRequired)
			_, _ = w.Write([]byte(`{"error":{"message":"Insufficient Balance"}}`))
			return
		}
		if r.URL.Path != "/v1/tools/fetch" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"title":"Example","content":"# Hello"}`))
	}

	env := newTestEnv(t, upstream, map[string]float64{"sk-one": 100, "sk-two": 90})
	resp := env.do(t, http.MethodPost, "/tools/fetch", "local-token", `{"url":"https://example.com"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, readAll(t, resp))
	}
	body := readAll(t, resp)
	if !strings.Contains(body, "# Hello") {
		t.Fatalf("body = %s", body)
	}
	if len(calls) != 2 || calls[0] != "sk-one" || calls[1] != "sk-two" {
		t.Fatalf("upstream calls = %v", calls)
	}
}

func TestToolRequestValidation(t *testing.T) {
	calls := 0
	upstream := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user/balance" {
			balanceReply(w)
			return
		}
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}
	env := newTestEnv(t, upstream, map[string]float64{"sk-one": 100})

	longQuery := `{"query":"` + strings.Repeat("x", 129) + `"}`
	cases := []struct {
		name string
		path string
		body string
		want int
	}{
		{"empty query", "/v1/tools/search", `{"query":""}`, http.StatusBadRequest},
		{"blank query", "/v1/tools/search", `{"query":"   "}`, http.StatusBadRequest},
		{"count zero", "/v1/tools/search", `{"query":"x","count":0}`, http.StatusBadRequest},
		{"count too high", "/v1/tools/search", `{"query":"x","count":51}`, http.StatusBadRequest},
		{"query too long", "/v1/tools/search", longQuery, http.StatusBadRequest},
		{"invalid json", "/v1/tools/search", `{`, http.StatusBadRequest},
		{"empty url", "/v1/tools/fetch", `{"url":""}`, http.StatusBadRequest},
		{"non-http scheme", "/v1/tools/fetch", `{"url":"ftp://example.com"}`, http.StatusBadRequest},
		{"credentials in url", "/v1/tools/fetch", `{"url":"https://user:pass@example.com"}`, http.StatusBadRequest},
		{"missing host", "/v1/tools/fetch", `{"url":"https:///path"}`, http.StatusBadRequest},
		{"missing token", "/v1/tools/search", `{"query":"x"}`, http.StatusUnauthorized},
		{"wrong token", "/v1/tools/search", `{"query":"x"}`, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token := "local-token"
			if tc.want == http.StatusUnauthorized {
				token = ""
				if tc.name == "wrong token" {
					token = "nope"
				}
			}
			resp := env.do(t, http.MethodPost, tc.path, token, tc.body)
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
	if calls != 0 {
		t.Fatalf("invalid requests reached upstream %d times", calls)
	}
}

func TestToolOversizedBody(t *testing.T) {
	upstream := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user/balance" {
			balanceReply(w)
			return
		}
		t.Error("oversized request reached upstream")
	}
	env := newTestEnv(t, upstream, map[string]float64{"sk-one": 100})
	body := `{"query":"` + strings.Repeat("x", maxToolRequestBytes) + `"}`
	resp := env.do(t, http.MethodPost, "/v1/tools/search", "local-token", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}
