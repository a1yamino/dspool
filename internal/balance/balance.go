// Package balance talks to DeepSeek's account balance endpoint.
package balance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ErrAuth means DeepSeek rejected the key (HTTP 401/403). A key that returns
// this is removed from the pool.
var ErrAuth = errors.New("api key rejected by DeepSeek")

type Info struct {
	Currency        string `json:"currency"`
	TotalBalance    string `json:"total_balance"`
	GrantedBalance  string `json:"granted_balance"`
	ToppedUpBalance string `json:"topped_up_balance"`
}

type Result struct {
	// Available mirrors DeepSeek's is_available.
	Available bool
	// Known is false when the response had no balance_infos entry.
	Known    bool
	Total    float64
	Currency string
	Infos    []Info
}

type StatusError struct {
	Status int
	Body   string
}

func (e *StatusError) Error() string {
	body := strings.TrimSpace(e.Body)
	if len(body) > 300 {
		body = body[:300] + "…"
	}
	if body == "" {
		return fmt.Sprintf("balance endpoint returned HTTP %d", e.Status)
	}
	return fmt.Sprintf("balance endpoint returned HTTP %d: %s", e.Status, body)
}

type Client struct {
	url string
	hc  *http.Client
}

func NewClient(url string, timeout time.Duration) *Client {
	return &Client{
		url: url,
		hc: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          32,
				MaxIdleConnsPerHost:   16,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   15 * time.Second,
				ResponseHeaderTimeout: timeout,
			},
		},
	}
}

type apiResponse struct {
	IsAvailable  bool   `json:"is_available"`
	BalanceInfos []Info `json:"balance_infos"`
}

func (c *Client) Fetch(ctx context.Context, apiKey string) (Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "dspool/0.1")

	resp, err := c.hc.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Result{}, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return Result{}, fmt.Errorf("%w: HTTP %d: %s", ErrAuth, resp.StatusCode, snippet(body))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Result{}, &StatusError{Status: resp.StatusCode, Body: string(body)}
	}
	var parsed apiResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Result{}, fmt.Errorf("decode balance response: %w", err)
	}

	out := Result{
		Available: parsed.IsAvailable,
		Infos:     parsed.BalanceInfos,
	}
	if len(parsed.BalanceInfos) == 0 {
		return out, nil
	}

	// DeepSeek accounts are normally single-currency. If more are returned,
	// use the first one and sum entries of that same currency.
	currency := parsed.BalanceInfos[0].Currency
	var total float64
	for _, info := range parsed.BalanceInfos {
		if !strings.EqualFold(info.Currency, currency) {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(info.TotalBalance), 64)
		if err != nil {
			return Result{}, fmt.Errorf("parse total_balance %q: %w", info.TotalBalance, err)
		}
		total += v
	}
	out.Known = true
	out.Total = total
	out.Currency = currency
	return out, nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
