package config

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"twenty-tickets/internal/intake"
)

func (s *Store) EnqueueIntake(ctx context.Context, event intake.Event) (bool, error) {
	raw, err := json.Marshal(event)
	if err != nil {
		return false, err
	}
	now := event.QueuedAt.UnixNano()
	result, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO intake_events(email_id,webhook_id,payload,state,attempts,next_attempt_at,queued_at,updated_at,last_error) VALUES(?,?,?,'pending',0,0,?,?, '')`, event.Email.ID, event.WebhookID, raw, now, now)
	if err != nil {
		return false, err
	}
	inserted, err := result.RowsAffected()
	if err != nil || inserted == 1 {
		return inserted == 1, err
	}
	var emailID string
	if err := s.db.QueryRowContext(ctx, "SELECT email_id FROM intake_events WHERE webhook_id=?", event.WebhookID).Scan(&emailID); err != nil && err != sql.ErrNoRows {
		return false, err
	}
	if emailID != "" && emailID != event.Email.ID {
		return false, fmt.Errorf("webhook ID is already associated with another email")
	}
	return false, nil
}

func (s *Store) DueIntake(ctx context.Context, now time.Time, limit int) ([]intake.Event, error) {
	if limit < 1 || limit > 100 {
		return nil, fmt.Errorf("invalid intake query limit")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload,attempts FROM intake_events WHERE state IN ('pending','retry') AND next_attempt_at<=? ORDER BY queued_at LIMIT ?`, now.UnixNano(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []intake.Event
	for rows.Next() {
		var event intake.Event
		var raw []byte
		if err := rows.Scan(&raw, &event.Attempts); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &event); err != nil {
			return nil, err
		}
		result = append(result, event)
	}
	return result, rows.Err()
}

func (s *Store) CompleteIntake(ctx context.Context, emailID, state string) error {
	if state != "done" && state != "ignored" {
		return fmt.Errorf("invalid terminal intake state")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE intake_events SET state=?,updated_at=?,last_error='' WHERE email_id=?`, state, time.Now().UnixNano(), emailID)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return fmt.Errorf("intake event not found")
	}
	return nil
}

func (s *Store) RetryIntake(ctx context.Context, emailID string, next time.Time, detail string) error {
	detail = strings.TrimSpace(detail)
	if len(detail) > 1200 {
		detail = detail[:1200] + "..."
	}
	result, err := s.db.ExecContext(ctx, `UPDATE intake_events SET state='retry',attempts=attempts+1,next_attempt_at=?,updated_at=?,last_error=? WHERE email_id=?`, next.UnixNano(), time.Now().UnixNano(), detail, emailID)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return fmt.Errorf("intake event not found")
	}
	return nil
}
