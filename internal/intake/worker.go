// Package intake durably processes verified Resend events outside the webhook request.
package intake

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"twenty-tickets/internal/activity"
	"twenty-tickets/internal/inbox"
	"twenty-tickets/internal/message"
	"twenty-tickets/internal/resend"
	"twenty-tickets/internal/routing"
)

type Event struct {
	Version      int                   `json:"version"`
	WebhookID    string                `json:"webhook_id"`
	Email        resend.Email          `json:"email"`
	Destinations []routing.Destination `json:"destinations,omitempty"`
	QueuedAt     time.Time             `json:"queued_at"`
	Attempts     int                   `json:"-"`
}

type Queue interface {
	DueIntake(context.Context, time.Time, int) ([]Event, error)
	CompleteIntake(context.Context, string, string) error
	RetryIntake(context.Context, string, time.Time, string) error
}

type Receiver interface {
	Receive(context.Context, string) (resend.Email, error)
}

type Router interface {
	Match([]string) ([]routing.Destination, error)
}

type Store interface {
	Exists(string) (bool, error)
	Save(inbox.Draft) error
}

type Worker struct {
	queue    Queue
	receiver Receiver
	store    Store
	router   Router
	recorder activity.Recorder
	log      *slog.Logger
	now      func() time.Time
	pause    time.Duration
	mu       sync.Mutex
}

func New(queue Queue, receiver Receiver, store Store, router Router, recorder activity.Recorder, log *slog.Logger) *Worker {
	return &Worker{queue: queue, receiver: receiver, store: store, router: router, recorder: recorder, log: log, now: time.Now, pause: 5 * time.Second}
}

func (w *Worker) Run(ctx context.Context) {
	for {
		if err := w.Process(ctx); err != nil && ctx.Err() == nil {
			w.log.Error("intake worker failed", "error", err)
		}
		timer := time.NewTimer(w.pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (w *Worker) Process(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	events, err := w.queue.DueIntake(ctx, w.now(), 20)
	if err != nil {
		return err
	}
	for _, event := range events {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := w.process(ctx, event); err != nil {
			w.log.Error("queued email processing failed", "email_id", event.Email.ID, "error", err)
		}
	}
	return nil
}

func (w *Worker) process(ctx context.Context, event Event) error {
	trace := activity.New(w.recorder, w.log, activity.Metadata(event.Email), "", "")
	trace.SetWebhookID(event.WebhookID)
	exists, err := w.store.Exists(event.Email.ID)
	if err != nil {
		return w.retry(event, trace, "inbox_lookup", err)
	}
	if exists {
		trace.Emit("inbox_lookup", "skipped", "Draft already saved; completing the queued event.")
		return w.queue.CompleteIntake(context.Background(), event.Email.ID, "done")
	}
	trace.Emit("resend_fetch", "running", "Requesting received email content from Resend outside the webhook request.")
	email, err := w.receiver.Receive(ctx, event.Email.ID)
	if err != nil {
		return w.retry(event, trace, "resend_fetch", err)
	}
	trace.SetEmail(email)
	trace.Emit("resend_fetch", "success", "Received email content from Resend.")
	destinations := event.Destinations
	if len(event.Email.To) == 0 {
		destinations, err = w.router.Match(email.To)
		if err != nil {
			return w.retry(event, trace, "routing", err)
		}
	} else {
		matched := destinations[:0]
		for _, destination := range destinations {
			filter, _ := message.NewRecipientFilter(destination.Inbound)
			if filter.Matches(email.To) {
				matched = append(matched, destination)
			}
		}
		destinations = matched
	}
	if len(destinations) == 0 {
		trace.Emit("routing", "skipped", "No queued destination matched the retrieved To recipients; email ignored.")
		return w.queue.CompleteIntake(context.Background(), event.Email.ID, "ignored")
	}
	names := make([]string, 0, len(destinations))
	for _, destination := range destinations {
		name := destination.RouteName
		if name == "" {
			name = destination.RouteID
		}
		names = append(names, name)
	}
	trace.Emit("routing", "success", "Confirmed destinations: "+strings.Join(names, ", "))
	trace.Emit("parsing", "running", "Extracting the top message and introductory note.")
	draft := inbox.Draft{Version: 2, EventID: event.WebhookID, SavedAt: w.now().UTC(), Status: "pending_twenty", Email: email, Destinations: destinations}
	if email.Text == nil || strings.TrimSpace(*email.Text) == "" {
		draft.Status, draft.ReviewReason = "needs_review", "missing_plain_text"
	} else {
		draft.Body, draft.Forwarded = message.Extract(*email.Text, email.Subject)
		if draft.Body == "" {
			draft.Status, draft.ReviewReason = "needs_review", "empty_extraction"
		}
	}
	if draft.Status == "needs_review" {
		trace.Emit("parsing", "review", draft.ReviewReason+"; manual review required.")
	} else {
		trace.Emit("parsing", "success", fmt.Sprintf("Extracted %d bytes; forwarded message recognized: %t.", len(draft.Body), draft.Forwarded))
	}
	trace.Emit("inbox_save", "running", "Writing the draft and frozen destination mappings.")
	if err := w.store.Save(draft); err != nil {
		return w.retry(event, trace, "inbox_save", err)
	}
	trace.Emit("inbox_save", "success", "Draft saved to persistent storage.")
	if draft.Status == "needs_review" {
		trace.Emit("queued", "review", "Saved for review; automatic delivery skipped.")
	} else {
		trace.Emit("queued", "success", fmt.Sprintf("Queued for %d destination(s).", len(destinations)))
	}
	if err := w.queue.CompleteIntake(context.Background(), event.Email.ID, "done"); err != nil {
		return err
	}
	w.log.Info("email saved", "email_id", event.Email.ID, "status", draft.Status, "body_bytes", len(draft.Body), "forwarded", draft.Forwarded)
	return nil
}

func (w *Worker) retry(event Event, trace *activity.Tracker, stage string, cause error) error {
	trace.Emit(stage, "failure", cause.Error())
	delay := 30 * time.Second * time.Duration(1<<min(event.Attempts, 6))
	if delay > 30*time.Minute {
		delay = 30 * time.Minute
	}
	next := w.now().Add(delay)
	if err := w.queue.RetryIntake(context.Background(), event.Email.ID, next, cause.Error()); err != nil {
		return fmt.Errorf("%w; save intake retry: %v", cause, err)
	}
	trace.Emit("intake_retry", "retry", "Next intake attempt after "+next.UTC().Format(time.RFC3339)+".")
	return cause
}
