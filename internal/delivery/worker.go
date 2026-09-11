// Package delivery drains snapshotted destinations independently of webhooks.
package delivery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"twenty-tickets/internal/activity"
	"twenty-tickets/internal/inbox"
	"twenty-tickets/internal/routing"
	"twenty-tickets/internal/twenty"
)

type Connections interface {
	Client(string) (*twenty.Client, error)
}
type Store interface {
	EachDraft(context.Context, func(inbox.Draft) error) error
	Destinations(inbox.Draft) ([]routing.Destination, error)
	LoadDestinationDelivery(string, string) (inbox.Delivery, error)
	SaveDelivery(inbox.Delivery) error
}
type Worker struct {
	store       Store
	connections Connections
	log         *slog.Logger
	now         func() time.Time
	pause       time.Duration
	recorder    activity.Recorder
}

func New(store Store, connections Connections, log *slog.Logger, recorder activity.Recorder) *Worker {
	return &Worker{store: store, connections: connections, log: log, now: time.Now, pause: 2 * time.Second, recorder: recorder}
}
func (w *Worker) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := w.Process(ctx); err != nil && ctx.Err() == nil {
			w.log.Error("inbox delivery scan failed", "error", err)
		}
		if wait(ctx, 5*time.Second) != nil {
			return
		}
	}
}
func (w *Worker) Process(ctx context.Context) error {
	return w.store.EachDraft(ctx, func(d inbox.Draft) error {
		if d.Status != "pending_twenty" {
			return nil
		}
		destinations, err := w.store.Destinations(d)
		if err != nil {
			activity.New(w.recorder, w.log, activity.Metadata(d.Email), "", "").Emit("destination_load", "failure", err.Error())
			return err
		}
		var errs []error
		for _, destination := range destinations {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err := w.deliver(ctx, d, destination); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	})
}
func (w *Worker) deliver(ctx context.Context, d inbox.Draft, destination routing.Destination) error {
	state, err := w.store.LoadDestinationDelivery(d.Email.ID, destination.RouteID)
	if err != nil {
		activity.New(w.recorder, w.log, activity.Metadata(d.Email), destination.RouteID, destination.RouteName).Emit("receipt_load", "failure", err.Error())
		return err
	}
	if state.Status == "delivered" || w.now().Before(state.NextAttemptAt) {
		return nil
	}
	trace := activity.New(w.recorder, w.log, activity.Metadata(d.Email), destination.RouteID, destination.RouteName)
	trace.Emit("delivery", "running", fmt.Sprintf("Starting delivery attempt %d to %s.", state.Attempts+1, destination.Plural))
	trace.Emit("field_mapping", "running", "Applying the saved field mappings.")
	fields, sendErr := destination.Payload(d.Email, d.Body)
	var id string
	if sendErr == nil {
		trace.Emit("field_mapping", "success", fmt.Sprintf("Mapped %d field(s). Default fields are omitted.", len(fields)))
		var client *twenty.Client
		trace.Emit("connection", "running", "Loading the saved Twenty connection.")
		client, sendErr = w.connections.Client(destination.ConnectionID)
		if sendErr == nil {
			trace.Emit("connection", "success", "Connection configuration loaded.")
			id, sendErr = client.EnsureRecordTracked(ctx, destination.Singular, destination.Plural, destination.RecordID(d.Email.ID), fields, trace.Emit)
		} else {
			trace.Emit("connection", "failure", sendErr.Error())
		}
	} else {
		trace.Emit("field_mapping", "failure", sendErr.Error())
	}
	if sendErr == nil && id == "" {
		sendErr = fmt.Errorf("empty Twenty record ID")
	}
	if sendErr != nil && ctx.Err() != nil {
		trace.Emit("delivery", "failure", "Delivery interrupted: "+sendErr.Error()+". It will be checked again after restart.")
		return ctx.Err()
	}
	state.Attempts++
	state.UpdatedAt = w.now().UTC()
	if sendErr != nil {
		trace.Emit("delivery", "failure", sendErr.Error())
		state.Status = "retry"
		state.LastError = sendErr.Error()
		state.NextAttemptAt = state.UpdatedAt.Add(backoff(state.Attempts))
		w.log.Error("Twenty delivery failed", "email_id", d.Email.ID, "route_id", destination.RouteID, "retry_at", state.NextAttemptAt, "error", sendErr)
	} else {
		state.Status = "delivered"
		state.TicketID = id
		state.LastError = ""
		state.NextAttemptAt = time.Time{}
	}
	trace.Emit("receipt_save", "running", "Saving delivery result and retry state.")
	err = w.store.SaveDelivery(state)
	if err != nil {
		trace.Emit("receipt_save", "failure", err.Error()+". Delivery remains eligible for retry; a committed record will be recovered by ID.")
	} else if sendErr != nil {
		trace.Emit("receipt_save", "success", "Failure and retry deadline saved.")
		trace.Emit("retry", "retry", fmt.Sprintf("Attempt %d failed; next retry after %s.", state.Attempts, state.NextAttemptAt.UTC().Format(time.RFC3339)))
	} else {
		trace.Emit("receipt_save", "success", "Delivery receipt saved.")
		trace.Emit("delivery", "success", "Delivered record "+id)
	}
	if err == nil && sendErr == nil {
		w.log.Info("Twenty record delivered", "email_id", d.Email.ID, "route_id", destination.RouteID, "record_id", id)
	}
	if waitErr := wait(ctx, w.pause); waitErr != nil {
		return waitErr
	}
	return err
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
