package config

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"twenty-tickets/internal/activity"
	"twenty-tickets/internal/resend"
)

// RecordActivity deliberately uses a short independent context: a cancelled
// webhook/Resend request must still be able to record why it failed.
func (s *Store) RecordActivity(e activity.Event) error {
	if !resend.ValidID(e.Email.ID) {
		return fmt.Errorf("invalid activity email ID")
	}
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	e.Email.Subject = clip(e.Email.Subject, 240)
	e.Email.From = clip(e.Email.From, 240)
	e.Email.To = clip(e.Email.To, 500)
	e.Detail = clip(e.Detail, 1200)
	e.RouteName = clip(e.RouteName, 160)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO activity_emails(email_id,subject,sender,recipients,first_seen,updated_at,last_event) VALUES(?,?,?,?,?,?,0)
	ON CONFLICT(email_id) DO UPDATE SET subject=CASE WHEN excluded.subject<>'' THEN excluded.subject ELSE activity_emails.subject END,
	sender=CASE WHEN excluded.sender<>'' THEN excluded.sender ELSE activity_emails.sender END,
	recipients=CASE WHEN excluded.recipients<>'' THEN excluded.recipients ELSE activity_emails.recipients END,
	updated_at=excluded.updated_at`, e.Email.ID, e.Email.Subject, e.Email.From, e.Email.To, e.At.UnixNano(), e.At.UnixNano())
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO activity_events(email_id,occurred_at,attempt_id,webhook_id,route_id,route_name,stage,status,detail) VALUES(?,?,?,?,?,?,?,?,?)`, e.Email.ID, e.At.UnixNano(), e.AttemptID, e.WebhookID, e.RouteID, e.RouteName, e.Stage, e.Status, e.Detail)
	if err != nil {
		return err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE activity_emails SET last_event=? WHERE email_id=?", id, e.Email.ID); err != nil {
		return err
	}
	return tx.Commit()
}
func clip(value string, limit int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return value
}

func (s *Store) ActivityEmails(ctx context.Context, query string, offset, limit int) ([]activity.Email, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT email_id,subject,sender,recipients,first_seen,updated_at FROM activity_emails
	WHERE instr(lower(email_id||' '||subject||' '||sender||' '||recipients),lower(?))>0 ORDER BY last_event DESC LIMIT ? OFFSET ?`, clip(query, 240), limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var emails []activity.Email
	for rows.Next() {
		var e activity.Email
		var first, updated int64
		if err := rows.Scan(&e.ID, &e.Subject, &e.From, &e.To, &first, &updated); err != nil {
			return nil, err
		}
		e.FirstSeen = time.Unix(0, first).UTC()
		e.UpdatedAt = time.Unix(0, updated).UTC()
		emails = append(emails, e)
	}
	return emails, rows.Err()
}
func (s *Store) ActivityEmail(ctx context.Context, id string) (activity.Email, error) {
	var e activity.Email
	var first, updated int64
	err := s.db.QueryRowContext(ctx, "SELECT email_id,subject,sender,recipients,first_seen,updated_at FROM activity_emails WHERE email_id=?", id).Scan(&e.ID, &e.Subject, &e.From, &e.To, &first, &updated)
	e.FirstSeen = time.Unix(0, first).UTC()
	e.UpdatedAt = time.Unix(0, updated).UTC()
	return e, err
}

// ActivityScopes returns the latest outcome for intake and each destination,
// keeping a success in one Twenty instance from hiding a failure in another.
func (s *Store) ActivityScopes(ctx context.Context, id string) ([]activity.Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,occurred_at,attempt_id,webhook_id,route_id,route_name,stage,status,detail FROM activity_events
	WHERE id IN(SELECT max(id) FROM activity_events WHERE email_id=? GROUP BY route_id) ORDER BY route_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows, id)
}
func (s *Store) ActivityEvents(ctx context.Context, id string, offset, limit int) ([]activity.Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,occurred_at,attempt_id,webhook_id,route_id,route_name,stage,status,detail FROM activity_events WHERE email_id=? ORDER BY id DESC LIMIT ? OFFSET ?`, id, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows, id)
}
func scanEvents(rows *sql.Rows, emailID string) ([]activity.Event, error) {
	var events []activity.Event
	for rows.Next() {
		e := activity.Event{Email: activity.Email{ID: emailID}}
		var at int64
		if err := rows.Scan(&e.ID, &at, &e.AttemptID, &e.WebhookID, &e.RouteID, &e.RouteName, &e.Stage, &e.Status, &e.Detail); err != nil {
			return nil, err
		}
		e.At = time.Unix(0, at).UTC()
		events = append(events, e)
	}
	return events, rows.Err()
}
