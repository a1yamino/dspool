package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHostIsLoopback(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8787": true,
		"localhost:8787": true,
		"[::1]:8787":     true,
		"evil.example":   false,
		"0.0.0.0:8787":   false,
		"192.168.1.10:8": false,
	}
	for host, want := range cases {
		if got := hostIsLoopback(host); got != want {
			t.Errorf("hostIsLoopback(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestSameOriginHost(t *testing.T) {
	cases := []struct {
		origin, host string
		want         bool
	}{
		{"http://127.0.0.1:8787", "127.0.0.1:8787", true},
		{"https://localhost:8787", "localhost:8787", true},
		{"https://evil.example", "127.0.0.1:8787", false},
		{"not a url", "127.0.0.1:8787", false},
	}
	for _, c := range cases {
		if got := sameOriginHost(c.origin, c.host); got != c.want {
			t.Errorf("sameOriginHost(%q, %q) = %v, want %v", c.origin, c.host, got, c.want)
		}
	}
}

func TestBrowserGuard(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	t.Run("cross origin rejected", func(t *testing.T) {
		h := (&Server{clientHashes: map[string]struct{}{}}).browserGuardMiddleware(next)
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/v1/chat/completions", nil)
		req.Header.Set("Origin", "https://evil.example")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d", rec.Code)
		}
	})

	t.Run("same origin allowed", func(t *testing.T) {
		h := (&Server{clientHashes: map[string]struct{}{}}).browserGuardMiddleware(next)
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8787/dashboard/refresh", nil)
		req.Header.Set("Origin", "http://127.0.0.1:8787")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d", rec.Code)
		}
	})

	t.Run("sec fetch site rejected", func(t *testing.T) {
		h := (&Server{clientHashes: map[string]struct{}{}}).browserGuardMiddleware(next)
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/dashboard/data", nil)
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d", rec.Code)
		}
	})

	t.Run("anonymous mode rejects non-loopback host", func(t *testing.T) {
		h := (&Server{clientHashes: map[string]struct{}{}}).browserGuardMiddleware(next)
		req := httptest.NewRequest(http.MethodGet, "http://evil.example:8787/", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d", rec.Code)
		}
	})

	t.Run("client token mode allows non-loopback host", func(t *testing.T) {
		h := (&Server{clientHashes: map[string]struct{}{"x": {}}}).browserGuardMiddleware(next)
		req := httptest.NewRequest(http.MethodGet, "http://192.168.1.2:8787/", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d", rec.Code)
		}
	})
}
