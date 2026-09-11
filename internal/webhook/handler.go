package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	svix "github.com/svix/svix-webhooks/go"
	"twenty-tickets/internal/activity"
	"twenty-tickets/internal/intake"
	"twenty-tickets/internal/resend"
	"twenty-tickets/internal/routing"
)

type Queue interface {
	EnqueueIntake(context.Context, intake.Event) (bool, error)
}

type Router interface {
	Match([]string) ([]routing.Destination, error)
}

func New(secret string, queue Queue, log *slog.Logger, router Router, recorder activity.Recorder) (http.Handler, error) {
	verifier, err := svix.NewWebhook(secret)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("POST /webhooks/resend", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		payload, err := io.ReadAll(r.Body)
		if err != nil {
			var tooLarge *http.MaxBytesError
			status := http.StatusBadRequest
			if errors.As(err, &tooLarge) {
				status = http.StatusRequestEntityTooLarge
			}
			http.Error(w, "invalid request body", status)
			return
		}
		if err := verifier.Verify(payload, r.Header); err != nil {
			http.Error(w, "invalid webhook signature", http.StatusUnauthorized)
			return
		}
		var event struct {
			Type string `json:"type"`
			Data struct {
				EmailID string   `json:"email_id"`
				To      []string `json:"to"`
				Subject string   `json:"subject"`
				From    string   `json:"from"`
			} `json:"data"`
		}
		if err := json.Unmarshal(payload, &event); err != nil || event.Type == "" {
			http.Error(w, "invalid event", http.StatusBadRequest)
			return
		}
		if event.Type != "email.received" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		id := event.Data.EmailID
		if !resend.ValidID(id) {
			http.Error(w, "invalid email_id", http.StatusBadRequest)
			return
		}
		trace := activity.New(recorder, log, activity.Metadata(resend.Email{ID: id, Subject: event.Data.Subject, From: event.Data.From, To: event.Data.To}), "", "")
		trace.SetWebhookID(r.Header.Get("svix-id"))
		trace.Emit("webhook_received", "success", "Received email.received webhook.")
		trace.Emit("signature_verified", "success", "Signature and timestamp verified.")
		ignore := func() {
			trace.Emit("routing", "skipped", "No matching destination for the email's To recipients; email ignored.")
			log.Info("email ignored", "email_id", id, "reason", "recipient_mismatch")
			w.WriteHeader(http.StatusNoContent)
		}
		fail := func(stage string, err error) {
			trace.Emit(stage, "failure", err.Error())
			log.Error("email processing failed", "email_id", id, "stage", stage, "error", err)
			http.Error(w, "processing failed; retry later", http.StatusServiceUnavailable)
		}
		// Snapshot routes from verified event recipients before acknowledging so
		// later configuration changes cannot retarget an accepted email.
		var destinations []routing.Destination
		if len(event.Data.To) > 0 {
			trace.Emit("routing", "running", "Matching verified To recipients against enabled routes.")
			destinations, err = router.Match(event.Data.To)
			if err != nil {
				fail("routing", err)
				return
			}
			if len(destinations) == 0 {
				ignore()
				return
			}
			trace.Emit("routing", "success", fmt.Sprintf("Snapshotted %d destination(s); recipients will be confirmed after retrieval.", len(destinations)))
		}
		e := intake.Event{Version: 1, WebhookID: r.Header.Get("svix-id"), Email: resend.Email{ID: id, Subject: event.Data.Subject, From: event.Data.From, To: event.Data.To}, Destinations: destinations, QueuedAt: time.Now().UTC()}
		queueCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Second)
		inserted, err := queue.EnqueueIntake(queueCtx, e)
		cancel()
		if err != nil {
			fail("intake_queue", err)
			return
		}
		if inserted {
			trace.Emit("intake_queue", "success", "Verified event committed for background retrieval.")
		} else {
			trace.Emit("intake_queue", "skipped", "Email is already present in the durable intake queue.")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux, nil
}
