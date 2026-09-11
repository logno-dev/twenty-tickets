package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFailures(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
	}{
		{"unauthorized", 401, "secret upstream details"},
		{"rate limited", 429, "secret upstream details"},
		{"invalid json", 200, "not json"},
		{"oversized", 200, strings.Repeat(" ", MaxResponseBytes+1)},
		{"redirect", 302, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", "/redirect")
				w.WriteHeader(tt.status)
				io.WriteString(w, tt.body)
			}))
			defer s.Close()
			c, err := New(s.URL, "test")
			if err != nil {
				t.Fatal(err)
			}
			var result any
			err = c.DoJSON(context.Background(), "GET", "/test", nil, &result)
			if err == nil || strings.Contains(err.Error(), "secret upstream details") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestConfiguredTimeoutAndCallerCancellation(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer s.Close()
	c, err := NewWithTimeout(s.URL, "test", 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	var output any
	err = c.DoJSON(context.Background(), "GET", "/slow", nil, &output)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("configured deadline not enforced: %v", err)
	}
	if !strings.Contains(err.Error(), "waiting for response headers") {
		t.Fatalf("request phase missing: %v", err)
	}
	c, err = NewWithTimeout(s.URL, "test", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.DoJSON(ctx, "GET", "/slow", nil, &output); !errors.Is(err, context.Canceled) {
		t.Fatalf("caller cancellation ignored: %v", err)
	}
	standard, err := New(s.URL, "test")
	if err != nil {
		t.Fatal(err)
	}
	if standard.http.Timeout != 10*time.Second {
		t.Fatal("default Twenty timeout changed")
	}
	if _, err := NewWithTimeout(s.URL, "test", 0); err == nil {
		t.Fatal("unbounded timeout accepted")
	}
}
