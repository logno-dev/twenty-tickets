package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	svix "github.com/svix/svix-webhooks/go"
	"twenty-tickets/internal/inbox"
	"twenty-tickets/internal/message"
	"twenty-tickets/internal/resend"
)

type Receiver interface {
	Receive(context.Context, string) (resend.Email, error)
}
type Store interface {
	Exists(string) (bool, error)
	Save(inbox.Draft) error
}

func New(secret string, receiver Receiver, store Store, log *slog.Logger) (http.Handler, error) {
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
				EmailID string `json:"email_id"`
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
		fail := func(stage string, err error) {
			log.Error("email processing failed", "email_id", id, "stage", stage, "error", err)
			http.Error(w, "processing failed; retry later", http.StatusServiceUnavailable)
		}
		exists, err := store.Exists(id)
		if err != nil {
			fail("inbox_lookup", err)
			return
		}
		if exists {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		email, err := receiver.Receive(r.Context(), id)
		if err != nil {
			fail("resend_fetch", err)
			return
		}
		draft := inbox.Draft{Version: 1, EventID: r.Header.Get("svix-id"), SavedAt: time.Now().UTC(), Status: "pending_twenty", Email: email}
		if email.Text == nil || strings.TrimSpace(*email.Text) == "" {
			draft.Status, draft.ReviewReason = "needs_review", "missing_plain_text"
		} else {
			draft.Body, draft.Forwarded = message.Extract(*email.Text, email.Subject)
			if draft.Body == "" {
				draft.Status, draft.ReviewReason = "needs_review", "empty_extraction"
			}
		}
		if err := store.Save(draft); err != nil {
			fail("inbox_save", err)
			return
		}
		log.Info("email saved", "email_id", id, "status", draft.Status, "body_bytes", len(draft.Body), "forwarded", draft.Forwarded)
		w.WriteHeader(http.StatusNoContent)
	})
	return mux, nil
}
