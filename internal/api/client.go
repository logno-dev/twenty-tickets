// Package api provides bounded, authenticated JSON requests to external APIs.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"time"
)

const MaxResponseBytes = 16 << 20

// StatusError exposes HTTP status without including potentially sensitive bodies.
type StatusError struct{ Code int }

func (e *StatusError) Error() string { return fmt.Sprintf("API returned HTTP %d", e.Code) }

type Client struct {
	base string
	key  string
	http *http.Client
}

func New(base, key string) (*Client, error) {
	return NewWithTimeout(base, key, 10*time.Second)
}

func NewWithTimeout(base, key string, timeout time.Duration) (*Client, error) {
	if timeout <= 0 {
		return nil, fmt.Errorf("API timeout must be positive")
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("API URL must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	if strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("API key is required")
	}
	return &Client{base: strings.TrimRight(base, "/"), key: key, http: &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// DoJSON accepts only relative API paths, so credentials cannot be directed to
// another host. Error response bodies are deliberately excluded from errors.
func (c *Client) DoJSON(ctx context.Context, method, path string, input, output any) error {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return fmt.Errorf("API path must start with a single slash")
	}
	var body io.Reader
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Accept", "application/json")
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	started := time.Now()
	phase := "starting request"
	var phaseMu sync.Mutex
	setPhase := func(value string) { phaseMu.Lock(); phase = value; phaseMu.Unlock() }
	getPhase := func() string { phaseMu.Lock(); defer phaseMu.Unlock(); return phase }
	trace := &httptrace.ClientTrace{
		DNSStart:             func(httptrace.DNSStartInfo) { setPhase("resolving DNS") },
		ConnectStart:         func(_, _ string) { setPhase("connecting") },
		TLSHandshakeStart:    func() { setPhase("negotiating TLS") },
		GotConn:              func(httptrace.GotConnInfo) { setPhase("writing request") },
		WroteRequest:         func(httptrace.WroteRequestInfo) { setPhase("waiting for response headers") },
		GotFirstResponseByte: func() { setPhase("reading response body") },
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("API request failed after %s while %s: %w", time.Since(started).Round(time.Millisecond), getPhase(), err)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return &StatusError{Code: res.StatusCode}
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, MaxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("API response read failed after %s: %w", time.Since(started).Round(time.Millisecond), err)
	}
	if len(b) > MaxResponseBytes {
		return fmt.Errorf("API response exceeds %d bytes", MaxResponseBytes)
	}
	if output == nil {
		return nil
	}
	if err := json.Unmarshal(b, output); err != nil {
		return fmt.Errorf("invalid API JSON: %w", err)
	}
	return nil
}
