package intake_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"twenty-tickets/internal/config"
	"twenty-tickets/internal/inbox"
	"twenty-tickets/internal/intake"
	"twenty-tickets/internal/resend"
	"twenty-tickets/internal/routing"
)

type receiverFunc func(context.Context, string) (resend.Email, error)

func (f receiverFunc) Receive(ctx context.Context, id string) (resend.Email, error) {
	return f(ctx, id)
}

type routerFunc func([]string, string) ([]routing.Destination, error)

func (f routerFunc) Match(to []string, from string) ([]routing.Destination, error) {
	return f(to, from)
}

func ptr(s string) *string { return &s }

func TestWorkerRetriesOutsideWebhookAndPreservesHistory(t *testing.T) {
	dir := t.TempDir()
	queue, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer queue.Close()
	store, err := inbox.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	destination := routing.Destination{RouteID: "route", RouteName: "Company", Inbound: "support@example.com", ConnectionID: "connection"}
	event := intake.Event{Version: 1, WebhookID: "webhook", Email: resend.Email{ID: "email", Subject: "Printer", From: "user@example.com", To: []string{"support@example.com"}}, Destinations: []routing.Destination{destination}, QueuedAt: now}
	if inserted, err := queue.EnqueueIntake(context.Background(), event); err != nil || !inserted {
		t.Fatal(inserted, err)
	}
	fail := true
	rawBody := "Please handle.\n\n---------- Forwarded message ---------\nFrom: A <a@example.com>\nDate: Tue\nSubject: Printer\nTo: Support\n\nPRIVATE BODY\n\nOn Mon, B wrote:\n> Older"
	receiver := receiverFunc(func(ctx context.Context, id string) (resend.Email, error) {
		if ctx.Err() != nil {
			t.Fatal("worker inherited an already-cancelled webhook context")
		}
		if fail {
			return resend.Email{}, context.DeadlineExceeded
		}
		return resend.Email{ID: id, Subject: "Fwd: Printer", From: "user@example.com", To: []string{"Support <support@example.com>"}, Text: ptr(rawBody)}, nil
	})
	w := intake.New(queue, receiver, store, routerFunc(func([]string, string) ([]routing.Destination, error) {
		return nil, fmt.Errorf("unexpected fallback routing")
	}), queue, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := w.Process(context.Background()); err != nil {
		t.Fatal(err)
	}
	if exists, _ := store.Exists("email"); exists {
		t.Fatal("failed retrieval created a draft")
	}
	if due, _ := queue.DueIntake(context.Background(), now.Add(29*time.Second), 10); len(due) != 0 {
		t.Fatal("retry ran before its deadline")
	}
	now = now.Add(31 * time.Second)
	if err := queue.RetryIntake(context.Background(), "email", time.Now().Add(-time.Second), "make retry due"); err != nil {
		t.Fatal(err)
	}
	fail = false
	if err := w.Process(context.Background()); err != nil {
		t.Fatal(err)
	}
	draft, err := store.Draft("email")
	if err != nil || draft.Body != "Please handle.\n\nPRIVATE BODY" || !draft.Forwarded || len(draft.Destinations) != 1 {
		t.Fatalf("draft: %+v %v", draft, err)
	}
	if due, _ := queue.DueIntake(context.Background(), now.Add(time.Hour), 10); len(due) != 0 {
		t.Fatal("completed intake remained due")
	}
	events, err := queue.ActivityEvents(context.Background(), "email", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	failure, retry, success := false, false, false
	for _, event := range events {
		if event.Stage == "resend_fetch" && event.Status == "failure" {
			failure = true
		}
		if event.Stage == "intake_retry" && event.Status == "retry" {
			retry = true
		}
		if event.Stage == "queued" && event.Status == "success" {
			success = true
		}
		if event.Detail == "PRIVATE BODY" {
			t.Fatal("email body leaked into activity")
		}
	}
	if !failure || !retry || !success {
		t.Fatalf("missing activity outcomes: failure=%t retry=%t success=%t", failure, retry, success)
	}
}

func TestWorkerFallsBackToFetchedRecipientsAndRecoversSavedDraft(t *testing.T) {
	dir := t.TempDir()
	queue, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer queue.Close()
	store, err := inbox.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	event := intake.Event{Version: 1, WebhookID: "webhook", Email: resend.Email{ID: "email"}, QueuedAt: now}
	if _, err := queue.EnqueueIntake(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	fetches := 0
	receiver := receiverFunc(func(context.Context, string) (resend.Email, error) {
		fetches++
		return resend.Email{ID: "email", To: []string{"support@example.com"}, Text: ptr("Body")}, nil
	})
	router := routerFunc(func(to []string, from string) ([]routing.Destination, error) {
		return []routing.Destination{{RouteID: "route", Inbound: "support@example.com"}}, nil
	})
	w := intake.New(queue, receiver, store, router, queue, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := w.Process(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fetches != 1 {
		t.Fatal(fetches)
	}
	if err := queue.RetryIntake(context.Background(), "email", now, "simulated crash after draft save"); err != nil {
		t.Fatal(err)
	}
	if err := w.Process(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fetches != 1 {
		t.Fatal("existing draft was fetched again")
	}
}

func TestWorkerRejectsRetrievedSenderOutsideSnapshottedDomain(t *testing.T) {
	dir := t.TempDir()
	queue, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer queue.Close()
	store, err := inbox.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	event := intake.Event{
		Version:   1,
		WebhookID: "webhook",
		Email:     resend.Email{ID: "restricted", From: "user@somedomain.com", To: []string{"support@example.com"}},
		Destinations: []routing.Destination{{
			RouteID: "route", Inbound: "support@example.com", FromDomain: "somedomain.com",
		}},
		QueuedAt: time.Now().UTC(),
	}
	if _, err := queue.EnqueueIntake(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	receiver := receiverFunc(func(context.Context, string) (resend.Email, error) {
		return resend.Email{ID: "restricted", From: "attacker@other.com", To: []string{"support@example.com"}, Text: ptr("Body")}, nil
	})
	router := routerFunc(func([]string, string) ([]routing.Destination, error) {
		return nil, fmt.Errorf("unexpected fallback routing")
	})
	w := intake.New(queue, receiver, store, router, queue, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := w.Process(context.Background()); err != nil {
		t.Fatal(err)
	}
	if exists, err := store.Exists("restricted"); err != nil || exists {
		t.Fatal("disallowed sender created a draft")
	}
	if due, err := queue.DueIntake(context.Background(), time.Now().Add(time.Hour), 10); err != nil || len(due) != 0 {
		t.Fatal("ignored sender remained queued")
	}
}
