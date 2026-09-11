package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"twenty-tickets/internal/config"
	"twenty-tickets/internal/inbox"
	"twenty-tickets/internal/resend"
	"twenty-tickets/internal/routing"
	"twenty-tickets/internal/twenty"
)

type connections map[string]*twenty.Client

func (c connections) Client(id string) (*twenty.Client, error) {
	if client := c[id]; client != nil {
		return client, nil
	}
	return nil, fmt.Errorf("connection missing")
}

type backend struct {
	mu         sync.Mutex
	records    map[string]bool
	fail, lose bool
	posts      int
}

func (b *backend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fail {
		w.WriteHeader(429)
		return
	}
	if r.Method == "GET" {
		parts := strings.Split(r.URL.Path, "/")
		id := parts[len(parts)-1]
		if !b.records[id] {
			w.WriteHeader(404)
			return
		}
		fmt.Fprintf(w, `{"data":{"ticket":{"id":%q}}}`, id)
		return
	}
	var payload struct {
		ID string `json:"id"`
	}
	json.NewDecoder(r.Body).Decode(&payload)
	if b.records[payload.ID] {
		w.WriteHeader(400)
		return
	}
	b.records[payload.ID] = true
	b.posts++
	if b.lose {
		w.WriteHeader(502)
		return
	}
	fmt.Fprintf(w, `{"data":{"createTicket":{"id":%q}}}`, payload.ID)
}
func newBackend(t *testing.T) (*backend, *twenty.Client) {
	t.Helper()
	b := &backend{records: map[string]bool{}}
	s := httptest.NewServer(b)
	t.Cleanup(s.Close)
	c, err := twenty.New(s.URL, "test")
	if err != nil {
		t.Fatal(err)
	}
	return b, c
}
func destination(id string) routing.Destination {
	return routing.Destination{RouteID: id, ConnectionID: id, Singular: "ticket", Plural: "tickets", Mappings: []routing.Mapping{{Field: "name", Type: "TEXT", Source: "subject"}, {Field: "issueOrRequest", Type: "RICH_TEXT", Source: "body"}}}
}
func testWorker(store Store, c connections, now *time.Time) *Worker {
	w := New(store, c, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	w.pause = 0
	w.now = func() time.Time { return *now }
	return w
}
func TestIndependentDestinationsAndRestart(t *testing.T) {
	dir := t.TempDir()
	journal, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	store, err := inbox.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	d := inbox.Draft{Version: 2, Status: "pending_twenty", Email: resend.Email{ID: "email", Subject: "Help"}, Body: "Body", Destinations: []routing.Destination{destination("a"), destination("b")}}
	if err := store.Save(d); err != nil {
		t.Fatal(err)
	}
	a, ca := newBackend(t)
	b, cb := newBackend(t)
	b.fail = true
	c := connections{"a": ca, "b": cb}
	now := time.Now()
	w := testWorker(store, c, &now)
	w.recorder = journal
	if err := w.Process(context.Background()); err != nil {
		t.Fatal(err)
	}
	first, err := store.LoadDestinationDelivery("email", "a")
	if err != nil || first.Status != "delivered" {
		t.Fatalf("a: %+v %v", first, err)
	}
	second, err := store.LoadDestinationDelivery("email", "b")
	if err != nil || second.Status != "retry" || second.Attempts != 1 || !second.NextAttemptAt.Equal(now.Add(30*time.Second)) {
		t.Fatalf("b: %+v %v", second, err)
	}
	scopes, err := journal.ActivityScopes(context.Background(), "email")
	if err != nil {
		t.Fatal(err)
	}
	latest := map[string]string{}
	for _, scope := range scopes {
		latest[scope.RouteID] = scope.Status
	}
	if latest["a"] != "success" || latest["b"] != "retry" {
		t.Fatal("activity scopes merged successful and failed destinations", latest)
	}
	store, err = inbox.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	w = testWorker(store, c, &now)
	w.recorder = journal
	b.mu.Lock()
	b.fail = false
	b.lose = true
	b.mu.Unlock()
	if err := w.Process(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(31 * time.Second)
	if err := w.Process(context.Background()); err != nil {
		t.Fatal(err)
	} // Commit with lost response.
	now = now.Add(61 * time.Second)
	if err := w.Process(context.Background()); err != nil {
		t.Fatal(err)
	} // Recover by stable ID.
	second, err = store.LoadDestinationDelivery("email", "b")
	if err != nil || second.Status != "delivered" {
		t.Fatalf("retry recovery: %+v %v", second, err)
	}
	events, err := journal.ActivityEvents(context.Background(), "email", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	failedLookup, failedCreate, recovered := false, false, false
	for _, event := range events {
		if event.RouteID != "b" {
			continue
		}
		if event.Stage == "twenty_lookup" && event.Status == "failure" {
			failedLookup = true
		}
		if event.Stage == "twenty_create" && event.Status == "failure" {
			failedCreate = true
		}
		if event.Stage == "twenty_create" && event.Status == "skipped" {
			recovered = true
		}
	}
	if !failedLookup || !failedCreate || !recovered {
		t.Fatal("missing lookup, create, or recovery activity")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	b.mu.Lock()
	defer b.mu.Unlock()
	if a.posts != 1 || b.posts != 1 {
		t.Fatalf("duplicates: a=%d b=%d", a.posts, b.posts)
	}
}

type receiptFailure struct{ *inbox.Store }

func (receiptFailure) SaveDelivery(inbox.Delivery) error { return fmt.Errorf("disk full") }
func TestReceiptFailureRecoveryAndLegacyHold(t *testing.T) {
	journal, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	store, err := inbox.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []inbox.Draft{
		{Version: 1, Status: "pending_twenty", Email: resend.Email{ID: "old"}, Body: "Legacy"},
		{Version: 2, Status: "needs_review", Email: resend.Email{ID: "review"}, Destinations: []routing.Destination{destination("a")}},
		{Version: 2, Status: "pending_twenty", Email: resend.Email{ID: "new"}, Body: "Body", Destinations: []routing.Destination{destination("a")}},
	} {
		if err := store.Save(d); err != nil {
			t.Fatal(err)
		}
	}
	b, c := newBackend(t)
	now := time.Now()
	clients := connections{"a": c}
	w := testWorker(receiptFailure{store}, clients, &now)
	w.recorder = journal
	if err := w.Process(context.Background()); err == nil {
		t.Fatal("receipt failure ignored")
	}
	events, err := journal.ActivityEvents(context.Background(), "new", 0, 100)
	if err != nil || len(events) == 0 || events[0].Stage != "receipt_save" || events[0].Status != "failure" {
		t.Fatalf("receipt failure not visible: %+v %v", events, err)
	}
	w = testWorker(store, clients, &now)
	if err := w.Process(context.Background()); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	if b.posts != 1 {
		t.Fatal("legacy/review delivered or duplicate created")
	}
	b.mu.Unlock()
	if err := store.AssignLegacy("old", destination("a")); err != nil {
		t.Fatal(err)
	}
	if err := w.Process(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := store.LoadDestinationDelivery("old", "a")
	if err != nil || state.TicketID != twenty.TicketID("old") {
		t.Fatalf("legacy ID lost: %+v %v", state, err)
	}
	if err := store.AssignLegacy("old", destination("b")); err == nil {
		t.Fatal("legacy reassigned")
	}
}
func TestCancellationAndBackoff(t *testing.T) {
	store, err := inbox.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	w := testWorker(store, connections{}, &now)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.Run(ctx)
	if backoff(1) != 30*time.Second || backoff(2) != time.Minute || backoff(100) != 30*time.Minute {
		t.Fatal("bad backoff")
	}
}
