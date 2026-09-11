package delivery

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"twenty-tickets/internal/inbox"
	"twenty-tickets/internal/message"
	"twenty-tickets/internal/resend"
)

type fakeTickets struct {
	calls int
	fail  bool
}

func (f *fakeTickets) EnsureTicket(_ context.Context, id, subject, body string) (string, error) {
	f.calls++
	if f.fail {
		return "", fmt.Errorf("API returned HTTP 429")
	}
	return "ticket-" + id, nil
}

func newTestWorker(store Store, tickets Tickets, now *time.Time) *Worker {
	w := New(store, tickets, slog.New(slog.NewTextHandler(io.Discard, nil)), message.RecipientFilter{})
	w.now = func() time.Time { return *now }
	w.pause = 0
	return w
}

func saveDraft(t *testing.T, store *inbox.Store, id, status string) {
	t.Helper()
	err := store.Save(inbox.Draft{Version: 1, Email: resend.Email{ID: id, Subject: "Help"}, Body: "Please fix it.", Status: status})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPersistedRetryAndRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := inbox.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	saveDraft(t, store, "email", "pending_twenty")
	saveDraft(t, store, "review", "needs_review")
	tickets := &fakeTickets{fail: true}
	now := time.Now()
	w := newTestWorker(store, tickets, &now)
	if err := w.Process(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := store.LoadDelivery("email")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != "retry" || state.Attempts != 1 || state.LastError == "" || !state.NextAttemptAt.Equal(now.Add(30*time.Second)) {
		t.Fatalf("bad retry: %+v", state)
	}
	// Simulate restart: deadlines live on disk, not only in the old worker.
	store, err = inbox.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	w = newTestWorker(store, tickets, &now)
	if err := w.Process(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tickets.calls != 1 {
		t.Fatal("retried before deadline or sent review draft")
	}
	now = now.Add(31 * time.Second)
	tickets.fail = false
	if err := w.Process(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err = store.LoadDelivery("email")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != "delivered" || state.TicketID != "ticket-email" || state.Attempts != 2 || state.LastError != "" || !state.NextAttemptAt.IsZero() {
		t.Fatalf("bad receipt: %+v", state)
	}
	store, err = inbox.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	w = newTestWorker(store, tickets, &now)
	if err := w.Process(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tickets.calls != 2 {
		t.Fatal("delivered ticket sent again")
	}
}

type receiptFailure struct{ *inbox.Store }

func (s receiptFailure) SaveDelivery(inbox.Delivery) error { return fmt.Errorf("disk full") }

func TestReceiptFailureRemainsPending(t *testing.T) {
	store, err := inbox.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	saveDraft(t, store, "email", "pending_twenty")
	tickets := &fakeTickets{}
	now := time.Now()
	w := newTestWorker(receiptFailure{store}, tickets, &now)
	if err := w.Process(context.Background()); err == nil {
		t.Fatal("receipt failure ignored")
	}
	state, err := store.LoadDelivery("email")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status == "delivered" {
		t.Fatal("failed receipt recorded as delivered")
	}
	w = newTestWorker(store, tickets, &now)
	if err := w.Process(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tickets.calls != 2 {
		t.Fatal("pending draft not retried")
	}
}

func TestCancellationAndBackoff(t *testing.T) {
	store, err := inbox.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	saveDraft(t, store, "email", "pending_twenty")
	tickets := &fakeTickets{}
	now := time.Now()
	w := newTestWorker(store, tickets, &now)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.Process(ctx); err == nil {
		t.Fatal("cancellation ignored")
	}
	if tickets.calls != 0 {
		t.Fatal("request after cancellation")
	}
	if backoff(1) != 30*time.Second || backoff(2) != time.Minute || backoff(100) != 30*time.Minute {
		t.Fatal("bad retry backoff")
	}
}

func TestRecipientFilterAppliesToSavedDrafts(t *testing.T) {
	store, err := inbox.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for id, to := range map[string][]string{
		"matching": {"Support <SUPPORT@example.com>"},
		"other":    {"other@example.com"},
		"missing":  nil,
	} {
		if err := store.Save(inbox.Draft{Version: 1, Email: resend.Email{ID: id, To: to}, Body: "Body", Status: "pending_twenty"}); err != nil {
			t.Fatal(err)
		}
	}
	tickets := &fakeTickets{}
	now := time.Now()
	w := newTestWorker(store, tickets, &now)
	w.recipient, err = message.NewRecipientFilter("support@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Process(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tickets.calls != 1 {
		t.Fatalf("sent %d tickets, want only matching recipient", tickets.calls)
	}
	state, err := store.LoadDelivery("matching")
	if err != nil || state.Status != "delivered" {
		t.Fatalf("missing matching delivery: %+v %v", state, err)
	}
	for _, id := range []string{"other", "missing"} {
		state, err := store.LoadDelivery(id)
		if err != nil || state.Status != "" || state.Attempts != 0 {
			t.Fatalf("filtered draft marked attempted: %+v %v", state, err)
		}
	}
	// Filtering does not delete the old backlog; clearing the setting makes it eligible again.
	w = newTestWorker(store, tickets, &now)
	if err := w.Process(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tickets.calls != 3 {
		t.Fatalf("backlog was lost or delivered again: %d", tickets.calls)
	}
}
