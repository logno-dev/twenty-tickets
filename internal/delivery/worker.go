// Package delivery drains the durable inbox into Twenty independently of the
// webhook request, so Twenty outages do not exhaust Resend's delivery retries.
package delivery

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"twenty-tickets/internal/inbox"
	"twenty-tickets/internal/message"
)

type Tickets interface {
	EnsureTicket(context.Context, string, string, string) (string, error)
}

type Store interface {
	EachDraft(context.Context, func(inbox.Draft) error) error
	LoadDelivery(string) (inbox.Delivery, error)
	SaveDelivery(inbox.Delivery) error
}

type Worker struct {
	store     Store
	tickets   Tickets
	log       *slog.Logger
	now       func() time.Time
	pause     time.Duration
	recipient message.RecipientFilter
}

func New(store Store, tickets Tickets, log *slog.Logger, recipient message.RecipientFilter) *Worker {
	return &Worker{store: store, tickets: tickets, log: log, now: time.Now, pause: 2 * time.Second, recipient: recipient}
}

// Run uses one worker per volume. Two-second spacing between attempts keeps
// this worker's maximum two API calls per ticket below 100 requests/minute.
func (w *Worker) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := w.Process(ctx); err != nil && ctx.Err() == nil {
			w.log.Error("inbox delivery scan failed", "error", err)
		}
		if err := wait(ctx, 5*time.Second); err != nil {
			return
		}
	}
}

// Process makes one pass over the inbox, honoring persisted retry deadlines.
// It must not be called concurrently on the same volume.
func (w *Worker) Process(ctx context.Context) error {
	return w.store.EachDraft(ctx, func(d inbox.Draft) error {
		if d.Status != "pending_twenty" || !w.recipient.Matches(d.Email.To) {
			return nil
		}
		state, err := w.store.LoadDelivery(d.Email.ID)
		if err != nil {
			return err
		}
		if state.Status == "delivered" || w.now().Before(state.NextAttemptAt) {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		id, sendErr := w.tickets.EnsureTicket(ctx, d.Email.ID, d.Email.Subject, d.Body)
		if sendErr == nil && id == "" {
			sendErr = fmt.Errorf("Twenty returned an empty ticket ID")
		}
		// An interrupted POST may already have committed. Next start uses the
		// same ticket ID and checks Twenty before attempting another creation.
		if sendErr != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		state.Attempts++
		state.UpdatedAt = w.now().UTC()
		if sendErr != nil {
			state.Status = "retry"
			state.LastError = sendErr.Error()
			state.NextAttemptAt = state.UpdatedAt.Add(backoff(state.Attempts))
			w.log.Error("Twenty ticket delivery failed", "email_id", d.Email.ID, "attempt", state.Attempts, "retry_at", state.NextAttemptAt, "error", sendErr)
		} else {
			state.Status, state.TicketID, state.LastError = "delivered", id, ""
			state.NextAttemptAt = time.Time{}
		}
		saveErr := w.store.SaveDelivery(state)
		if saveErr == nil && sendErr == nil {
			w.log.Info("Twenty ticket delivered", "email_id", d.Email.ID, "ticket_id", id)
		}
		if err := wait(ctx, w.pause); err != nil {
			return err
		}
		return saveErr
	})
}

func backoff(attempts int) time.Duration {
	delay := 30 * time.Second
	for i := 1; i < attempts && delay < 30*time.Minute; i++ {
		delay *= 2
	}
	return min(delay, 30*time.Minute)
}

func wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
