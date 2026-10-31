package balance

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"is_available": true,
			"balance_infos": [
				{"currency":"CNY","total_balance":"110.50","granted_balance":"10.00","topped_up_balance":"100.50"},
				{"currency":"CNY","total_balance":"0.50"}
			]
		}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, 5*time.Second)
	res, err := c.Fetch(context.Background(), "sk-test")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Available || !res.Known {
		t.Fatalf("result = %+v", res)
	}
	if res.Total != 111.0 {
		t.Fatalf("total = %v, want 111", res.Total)
	}
	if res.Currency != "CNY" {
		t.Fatalf("currency = %q", res.Currency)
	}
}

func TestFetchNoBalanceInfo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"is_available":true,"balance_infos":[]}`))
	}))
	defer srv.Close()
	res, err := NewClient(srv.URL, time.Second).Fetch(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	if res.Known {
		t.Fatalf("expected unknown balance: %+v", res)
	}
	if !res.Available {
		t.Fatalf("available should mirror the API: %+v", res)
	}
}

func TestFetchAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Authentication Fails"}}`))
	}))
	defer srv.Close()
	_, err := NewClient(srv.URL, time.Second).Fetch(context.Background(), "bad")
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
}

func TestFetchStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream down"))
	}))
	defer srv.Close()
	_, err := NewClient(srv.URL, time.Second).Fetch(context.Background(), "k")
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusBadGateway {
		t.Fatalf("err = %v", err)
	}
}
