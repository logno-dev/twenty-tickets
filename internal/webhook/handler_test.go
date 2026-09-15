package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"twenty-tickets/internal/config"
	"twenty-tickets/internal/intake"
	"twenty-tickets/internal/routing"
)

const testKey = "test-signing-key-for-webhook-tests"

func signedRequest(body string, timestamp time.Time) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/webhooks/resend", strings.NewReader(body))
	ts := fmt.Sprint(timestamp.Unix())
	r.Header.Set("svix-id", "msg_test")
	r.Header.Set("svix-timestamp", ts)
	mac := hmac.New(sha256.New, []byte(testKey))
	mac.Write([]byte("msg_test." + ts + "." + body))
	r.Header.Set("svix-signature", "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	return r
}

type routerFunc func([]string, string) ([]routing.Destination, error)

func (f routerFunc) Match(to []string, from string) ([]routing.Destination, error) {
	return f(to, from)
}

type failingQueue struct{}

func (failingQueue) EnqueueIntake(context.Context, intake.Event) (bool, error) {
	return false, fmt.Errorf("disk full")
}

func TestWebhookValidationAndDurableQueue(t *testing.T) {
	settings, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer settings.Close()
	router := routerFunc(func(to []string, from string) ([]routing.Destination, error) {
		if from != "user@example.com" {
			t.Errorf("sender metadata not passed to router: %q", from)
		}
		if len(to) == 0 {
			return nil, nil
		}
		if to[0] != "support@example.com" {
			return nil, nil
		}
		return []routing.Destination{{RouteID: "route", RouteName: "Company", Inbound: "support@example.com"}}, nil
	})
	h, err := New("whsec_"+base64.StdEncoding.EncodeToString([]byte(testKey)), settings, slog.New(slog.NewTextHandler(io.Discard, nil)), router, settings)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"type":"email.received","data":{"email_id":"email_123","subject":"Help","from":"user@example.com","to":["support@example.com"]}}`
	for _, tt := range []struct {
		name    string
		request *http.Request
		status  int
	}{
		{"unsigned", httptest.NewRequest("POST", "/webhooks/resend", strings.NewReader(body)), 401},
		{"stale", signedRequest(body, time.Now().Add(-10*time.Minute)), 401},
		{"future", signedRequest(body, time.Now().Add(10*time.Minute)), 401},
		{"malformed", signedRequest(`{`, time.Now()), 400},
		{"missing id", signedRequest(`{"type":"email.received","data":{}}`, time.Now()), 400},
		{"wrong event", signedRequest(`{"type":"email.sent"}`, time.Now()), 204},
		{"large body", signedRequest(strings.Repeat("x", (1<<20)+1), time.Now()), 413},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, tt.request)
			if w.Code != tt.status {
				t.Fatalf("got %d, want %d", w.Code, tt.status)
			}
		})
	}
	tampered := signedRequest(body, time.Now())
	tampered.Body = io.NopCloser(strings.NewReader(body + " "))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, tampered)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w = httptest.NewRecorder()
	h.ServeHTTP(w, signedRequest(body, time.Now()).WithContext(ctx))
	if w.Code != 204 {
		t.Fatalf("cancelled caller prevented durable queueing: %d %s", w.Code, w.Body.String())
	}
	events, err := settings.DueIntake(context.Background(), time.Now(), 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("queued events: %+v %v", events, err)
	}
	event := events[0]
	if event.Email.Subject != "Help" || event.Email.From != "user@example.com" || len(event.Destinations) != 1 || event.Destinations[0].RouteID != "route" {
		t.Fatalf("metadata or route snapshot lost: %+v", event)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, signedRequest(body, time.Now()))
	if w.Code != 204 {
		t.Fatal(w.Code)
	}
	events, _ = settings.DueIntake(context.Background(), time.Now(), 10)
	if len(events) != 1 {
		t.Fatal("duplicate webhook created another intake job")
	}
}

func TestWebhookRoutingAndQueueFailures(t *testing.T) {
	secret := "whsec_" + base64.StdEncoding.EncodeToString([]byte(testKey))
	body := `{"type":"email.received","data":{"email_id":"email_123","to":["support@example.com"]}}`
	for _, tt := range []struct {
		name   string
		queue  Queue
		router Router
		status int
	}{
		{"queue failure", failingQueue{}, routerFunc(func([]string, string) ([]routing.Destination, error) {
			return []routing.Destination{{RouteID: "route"}}, nil
		}), 503},
		{"router failure", failingQueue{}, routerFunc(func([]string, string) ([]routing.Destination, error) { return nil, fmt.Errorf("no routes") }), 503},
		{"unmatched", failingQueue{}, routerFunc(func([]string, string) ([]routing.Destination, error) { return nil, nil }), 204},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h, err := New(secret, tt.queue, slog.New(slog.NewTextHandler(io.Discard, nil)), tt.router, nil)
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, signedRequest(body, time.Now()))
			if w.Code != tt.status {
				t.Fatalf("got %d, want %d", w.Code, tt.status)
			}
		})
	}
}
