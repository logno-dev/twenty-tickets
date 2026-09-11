// Package activity records processing outcomes without email bodies or API keys.
package activity

import (
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"twenty-tickets/internal/resend"
)

type Email struct {
	ID        string
	Subject   string
	From      string
	To        string
	FirstSeen time.Time
	UpdatedAt time.Time
}

func Metadata(email resend.Email) Email {
	return Email{ID: email.ID, Subject: email.Subject, From: email.From, To: strings.Join(email.To, ", ")}
}
func (e Email) Title() string {
	if e.Subject == "" {
		return "(No subject available)"
	}
	return e.Subject
}

type Event struct {
	ID        int64
	Email     Email
	At        time.Time
	AttemptID string
	WebhookID string
	RouteID   string
	RouteName string
	Stage     string
	Status    string
	Detail    string
}

func (e Event) Time() string { return e.At.UTC().Format("2006-01-02 15:04:05 UTC") }
func (e Event) Scope() string {
	if e.RouteID == "" {
		return "Intake"
	}
	if e.RouteName != "" {
		return e.RouteName
	}
	return e.RouteID
}
func (e Event) ShortAttempt() string {
	if len(e.AttemptID) > 8 {
		return e.AttemptID[:8]
	}
	return e.AttemptID
}
func (e Event) StageLabel() string {
	switch e.Stage {
	case "webhook_received":
		return "Webhook received"
	case "signature_verified":
		return "Signature verification"
	case "intake_queue":
		return "Queue verified email"
	case "intake_retry":
		return "Schedule intake retry"
	case "inbox_lookup":
		return "Duplicate check"
	case "routing":
		return "Recipient routing"
	case "resend_fetch":
		return "Retrieve from Resend"
	case "parsing":
		return "Extract message"
	case "inbox_save":
		return "Save draft"
	case "queued":
		return "Delivery queued"
	case "field_mapping":
		return "Map fields"
	case "connection":
		return "Load Twenty connection"
	case "twenty_lookup":
		return "Find existing record"
	case "twenty_create":
		return "Create Twenty record"
	case "receipt_save":
		return "Save delivery result"
	case "receipt_load":
		return "Read delivery state"
	case "destination_load":
		return "Read saved destinations"
	case "delivery":
		return "Delivery"
	case "retry":
		return "Retry scheduled"
	case "legacy_assignment":
		return "Legacy assignment"
	default:
		return e.Stage
	}
}

type Recorder interface{ RecordActivity(Event) error }
type Tracker struct {
	recorder Recorder
	log      *slog.Logger
	base     Event
}

func New(recorder Recorder, log *slog.Logger, email Email, routeID, routeName string) *Tracker {
	return &Tracker{recorder: recorder, log: log, base: Event{Email: email, RouteID: routeID, RouteName: routeName, AttemptID: uuid.NewString()}}
}
func (t *Tracker) SetEmail(email resend.Email) { t.base.Email = Metadata(email) }
func (t *Tracker) SetWebhookID(id string)      { t.base.WebhookID = id }
func (t *Tracker) Emit(stage, status, detail string) {
	if t.recorder == nil {
		return
	}
	e := t.base
	e.At = time.Now().UTC()
	e.Stage, e.Status, e.Detail = stage, status, detail
	if err := t.recorder.RecordActivity(e); err != nil {
		t.log.Error("activity log write failed", "email_id", e.Email.ID, "stage", stage, "error", err)
	}
}
