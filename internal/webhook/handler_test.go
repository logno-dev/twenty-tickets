package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"twenty-tickets/internal/delivery"
	"twenty-tickets/internal/inbox"
	"twenty-tickets/internal/message"
	"twenty-tickets/internal/resend"
	"twenty-tickets/internal/routing"
	"twenty-tickets/internal/twenty"
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

func TestWebhookPipeline(t *testing.T) {
	var calls atomic.Int32
	var fail atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/emails/receiving/email_123" || r.Header.Get("Authorization") != "Bearer re_test" || r.URL.Query().Get("html_format") != "cid" {
			t.Errorf("unexpected Resend request: %s", r.URL)
		}
		if fail.Load() {
			w.WriteHeader(429)
			return
		}
		io.WriteString(w, `{"id":"email_123","subject":"Help","text":"Please fix it.\n\nOn Tue, A wrote:\n> old"}`)
	}))
	defer upstream.Close()
	receiver, err := resend.NewWithURL(upstream.URL, "re_test")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	store, err := inbox.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New("whsec_"+base64.StdEncoding.EncodeToString([]byte(testKey)), receiver, store, slog.New(slog.NewTextHandler(io.Discard, nil)), testRouter{})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"type":"email.received","data":{"email_id":"email_123"}}`
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
			handler.ServeHTTP(w, tt.request)
			if w.Code != tt.status {
				t.Fatalf("got %d, want %d", w.Code, tt.status)
			}
		})
	}
	tampered := signedRequest(body, time.Now())
	tampered.Body = io.NopCloser(strings.NewReader(body + " "))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, tampered)
	if w.Code != 401 || calls.Load() != 0 {
		t.Fatal("untrusted event reached upstream")
	}
	fail.Store(true)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, signedRequest(body, time.Now()))
	if w.Code != 503 {
		t.Fatalf("upstream failure returned %d", w.Code)
	}
	fail.Store(false)
	for range 2 {
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, signedRequest(body, time.Now()))
		if w.Code != 204 {
			t.Fatalf("delivery returned %d: %s", w.Code, w.Body.String())
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("duplicate fetched again: %d calls", calls.Load())
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("expected one draft: %v, %v", files, err)
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var draft inbox.Draft
	if err := json.Unmarshal(b, &draft); err != nil {
		t.Fatal(err)
	}
	if draft.Body != "Please fix it." || draft.Status != "pending_twenty" || draft.Email.Text == nil {
		t.Fatalf("unexpected draft: %+v", draft)
	}
	// Reopen storage to confirm deduplication survives a process restart.
	reopened, err := inbox.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if exists, err := reopened.Exists("email_123"); !exists || err != nil {
		t.Fatalf("lost saved draft: %v", err)
	}
	// Deliver the actual draft produced by the signed webhook to a mock Twenty
	// endpoint, including its rich-text field.
	var creates atomic.Int32
	crm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.WriteHeader(404)
			return
		}
		creates.Add(1)
		var input struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			Issue struct {
				Markdown string `json:"markdown"`
			} `json:"issueOrRequest"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if r.URL.Path != "/rest/tickets" || input.Name != "Help" || input.Issue.Markdown != "Please fix it." {
			t.Errorf("wrong ticket from webhook: %+v", input)
		}
		fmt.Fprintf(w, `{"data":{"createTicket":{"id":%q}}}`, input.ID)
	}))
	defer crm.Close()
	client, err := twenty.New(crm.URL, "test")
	if err != nil {
		t.Fatal(err)
	}
	worker := delivery.New(reopened, testConnections{client}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for range 2 {
		if err := worker.Process(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	receipt, err := reopened.LoadDestinationDelivery("email_123", "test-route")
	if err != nil {
		t.Fatal(err)
	}
	if creates.Load() != 1 || receipt.Status != "delivered" || receipt.TicketID != (routing.Destination{RouteID: "test-route"}).RecordID("email_123") {
		t.Fatalf("bad ticket delivery: %+v, creates=%d", receipt, creates.Load())
	}
}

type fakeReceiver struct{ text *string }

func (f fakeReceiver) Receive(_ context.Context, id string) (resend.Email, error) {
	return resend.Email{ID: id, Text: f.text}, nil
}

type brokenStore struct{}

func (brokenStore) Exists(string) (bool, error) { return false, nil }
func (brokenStore) Save(inbox.Draft) error      { return fmt.Errorf("disk full") }

func TestReviewAndStorageFailure(t *testing.T) {
	for _, text := range []*string{nil, new(string), ptr("> old only")} {
		dir := t.TempDir()
		store, err := inbox.New(dir)
		if err != nil {
			t.Fatal(err)
		}
		h, err := New("whsec_"+base64.StdEncoding.EncodeToString([]byte(testKey)), fakeReceiver{text}, store, slog.Default(), testRouter{})
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, signedRequest(`{"type":"email.received","data":{"email_id":"test"}}`, time.Now()))
		if w.Code != 204 {
			t.Fatal(w.Code)
		}
		files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
		if len(files) != 1 {
			t.Fatal("missing review record")
		}
		b, err := os.ReadFile(files[0])
		if err != nil {
			t.Fatal(err)
		}
		var d inbox.Draft
		if err := json.Unmarshal(b, &d); err != nil {
			t.Fatal(err)
		}
		if d.Status != "needs_review" || d.ReviewReason == "" {
			t.Fatalf("missing review flag: %+v", d)
		}
	}
	h, err := New("whsec_"+base64.StdEncoding.EncodeToString([]byte(testKey)), fakeReceiver{ptr("hello")}, brokenStore{}, slog.Default(), testRouter{})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, signedRequest(`{"type":"email.received","data":{"email_id":"test"}}`, time.Now()))
	if w.Code != 503 {
		t.Fatalf("storage failure acknowledged: %d", w.Code)
	}
}

func ptr(s string) *string { return &s }

type receiverFunc func(context.Context, string) (resend.Email, error)

func (f receiverFunc) Receive(ctx context.Context, id string) (resend.Email, error) {
	return f(ctx, id)
}

func TestRecipientFiltering(t *testing.T) {
	filter, err := message.NewRecipientFilter("support@example.com")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name                    string
		eventTo, fetchedTo      []string
		unsigned                bool
		wantStatus, wantFetches int
		wantSaved               bool
	}{
		{"matching", []string{"support@example.com"}, []string{"Support <SUPPORT@example.com>"}, false, 204, 1, true},
		{"unrelated event", []string{"other@example.com"}, nil, false, 204, 0, false},
		{"missing event To falls back", nil, []string{"other@example.com", "support@example.com"}, false, 204, 1, true},
		{"fetched recipient differs", []string{"support@example.com"}, []string{"other@example.com"}, false, 204, 1, false},
		{"missing all recipients", nil, nil, false, 204, 1, false},
		{"CC and sender do not count", nil, []string{"other@example.com"}, false, 204, 1, false},
		{"verification before filtering", []string{"other@example.com"}, nil, true, 401, 0, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store, err := inbox.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			fetches := 0
			receiver := receiverFunc(func(_ context.Context, id string) (resend.Email, error) {
				fetches++
				return resend.Email{ID: id, To: tt.fetchedTo, CC: []string{"support@example.com"}, From: "support@example.com", Text: ptr("Newest message\nTo: support@example.com")}, nil
			})
			h, err := New("whsec_"+base64.StdEncoding.EncodeToString([]byte(testKey)), receiver, store, slog.New(slog.NewTextHandler(io.Discard, nil)), testRouter{filter: filter, inbound: "support@example.com"})
			if err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(map[string]any{"type": "email.received", "data": map[string]any{"email_id": "email_filter", "to": tt.eventTo}})
			if err != nil {
				t.Fatal(err)
			}
			r := signedRequest(string(body), time.Now())
			if tt.unsigned {
				r.Header.Del("svix-signature")
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tt.wantStatus || fetches != tt.wantFetches {
				t.Fatalf("status=%d fetches=%d; want %d %d", w.Code, fetches, tt.wantStatus, tt.wantFetches)
			}
			exists, err := store.Exists("email_filter")
			if err != nil || exists != tt.wantSaved {
				t.Fatalf("saved=%v; want %v, error=%v", exists, tt.wantSaved, err)
			}
		})
	}
}

type testRouter struct {
	filter  message.RecipientFilter
	inbound string
}

func (r testRouter) Match(to []string) ([]routing.Destination, error) {
	if !r.filter.Matches(to) {
		return nil, nil
	}
	return []routing.Destination{{RouteID: "test-route", ConnectionID: "test-connection", Inbound: r.inbound, Singular: "ticket", Plural: "tickets", Mappings: []routing.Mapping{{Field: "name", Type: "TEXT", Source: "subject"}, {Field: "issueOrRequest", Type: "RICH_TEXT", Source: "body"}}}}, nil
}

type testConnections struct{ client *twenty.Client }

func (c testConnections) Client(string) (*twenty.Client, error) { return c.client, nil }

type routerFunc func([]string) ([]routing.Destination, error)

func (f routerFunc) Match(to []string) ([]routing.Destination, error) { return f(to) }
func TestRoutingSnapshotAndSetupFailure(t *testing.T) {
	for _, setup := range []bool{false, true} {
		store, err := inbox.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		fetches := 0
		receiver := receiverFunc(func(_ context.Context, id string) (resend.Email, error) {
			fetches++
			return resend.Email{ID: id, To: []string{"a@example.com", "b@example.com"}, Text: ptr("Body")}, nil
		})
		router := routerFunc(func(to []string) ([]routing.Destination, error) {
			if !setup {
				return nil, fmt.Errorf("no routes")
			}
			return []routing.Destination{{RouteID: "a", Inbound: "a@example.com"}, {RouteID: "b", Inbound: "b@example.com"}}, nil
		})
		h, err := New("whsec_"+base64.StdEncoding.EncodeToString([]byte(testKey)), receiver, store, slog.New(slog.NewTextHandler(io.Discard, nil)), router)
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, signedRequest(`{"type":"email.received","data":{"email_id":"multi","to":["a@example.com","b@example.com"]}}`, time.Now()))
		if !setup {
			if w.Code != 503 || fetches != 0 {
				t.Fatal("unconfigured event acknowledged or fetched")
			}
			continue
		}
		d, err := store.Draft("multi")
		if err != nil || w.Code != 204 || fetches != 1 || len(d.Destinations) != 2 || d.Version != 2 {
			t.Fatalf("bad routed intake: %+v %v, status=%d fetches=%d", d, err, w.Code, fetches)
		}
	}
}
