package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
