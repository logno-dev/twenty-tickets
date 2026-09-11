package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	svix "github.com/svix/svix-webhooks/go"
	"twenty-tickets/internal/activity"
	"twenty-tickets/internal/inbox"
	"twenty-tickets/internal/message"
	"twenty-tickets/internal/resend"
	"twenty-tickets/internal/routing"
)

type Receiver interface {
	Receive(context.Context, string) (resend.Email, error)
}
type Store interface {
	Exists(string) (bool, error)
	Save(inbox.Draft) error
}

type Router interface {
	Match([]string) ([]routing.Destination, error)
}

func New(secret string, receiver Receiver, store Store, log *slog.Logger, router Router, recorder activity.Recorder) (http.Handler, error) {
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
		exists, err := store.Exists(id)
		if err != nil {
			fail("inbox_lookup", err)
			return
		}
		if exists {
			trace.Emit("inbox_lookup", "skipped", "Draft already saved. This duplicate webhook does not queue another delivery.")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		trace.Emit("inbox_lookup", "success", "New email; no saved draft found.")
		// Route once from verified event recipients when present, then confirm
		// against the fetched To array. Configuration changes cannot retarget it.
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
			trace.Emit("routing", "success", fmt.Sprintf("Selected %d destination(s); recipients will be confirmed after retrieval.", len(destinations)))
		}
		trace.Emit("resend_fetch", "running", "Requesting received email content from Resend.")
		email, err := receiver.Receive(r.Context(), id)
		if err != nil {
			fail("resend_fetch", err)
			return
		}
		trace.SetEmail(email)
		trace.Emit("resend_fetch", "success", "Received email content from Resend.")
		if len(event.Data.To) == 0 {
			destinations, err = router.Match(email.To)
			if err != nil {
				fail("routing", err)
				return
			}
		} else {
			// Each snapshot includes its matched inbound mailbox.
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
			ignore()
			return
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
		draft := inbox.Draft{Version: 2, EventID: r.Header.Get("svix-id"), SavedAt: time.Now().UTC(), Status: "pending_twenty", Email: email, Destinations: destinations}
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
		if err := store.Save(draft); err != nil {
			fail("inbox_save", err)
			return
		}
		trace.Emit("inbox_save", "success", "Draft saved to persistent storage.")
		if draft.Status == "needs_review" {
			trace.Emit("queued", "review", "Saved for review; automatic delivery skipped.")
		} else {
			trace.Emit("queued", "success", fmt.Sprintf("Queued for %d destination(s).", len(destinations)))
		}
		log.Info("email saved", "email_id", id, "status", draft.Status, "body_bytes", len(draft.Body), "forwarded", draft.Forwarded)
		w.WriteHeader(http.StatusNoContent)
	})
	return mux, nil
}
