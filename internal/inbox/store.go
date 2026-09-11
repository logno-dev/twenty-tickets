// Package inbox durably stores email drafts and Twenty delivery state.
package inbox

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"twenty-tickets/internal/resend"
)

type Draft struct {
	Version      int          `json:"version"`
	EventID      string       `json:"event_id"`
	SavedAt      time.Time    `json:"saved_at"`
	Status       string       `json:"status"`
	ReviewReason string       `json:"review_reason,omitempty"`
	Email        resend.Email `json:"email"`
	Body         string       `json:"body"`
	Forwarded    bool         `json:"forwarded"`
}

type Store struct{ dir string }

func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, ".write-check-*")
	if err != nil {
		return nil, err
	}
	name := f.Name()
	f.Close()
	if err := os.Remove(name); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "deliveries"), 0700); err != nil {
		return nil, err
	}
	if err := syncDir(dir); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func (s *Store) path(id string) string {
	return filepath.Join(s.dir, fmt.Sprintf("%x.json", sha256.Sum256([]byte(id))))
}

func (s *Store) Exists(id string) (bool, error) {
	_, err := os.Stat(s.path(id))
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

// Save publishes a fully written file atomically without replacing an existing
// email, including when simultaneous duplicate webhook deliveries race.
func (s *Store) Save(d Draft) error {
	return writeJSON(s.path(d.Email.ID), d, false)
}

// Delivery is separate from the immutable original draft. A single delivery
// worker owns these records; webhook handlers only create original drafts.
type Delivery struct {
	EmailID       string    `json:"email_id"`
	TicketID      string    `json:"ticket_id,omitempty"`
	Status        string    `json:"status"`
	Attempts      int       `json:"attempts"`
	UpdatedAt     time.Time `json:"updated_at"`
	NextAttemptAt time.Time `json:"next_attempt_at,omitzero"`
	LastError     string    `json:"last_error,omitempty"`
}

func (s *Store) deliveryPath(id string) string {
	return filepath.Join(s.dir, "deliveries", filepath.Base(s.path(id)))
}

func (s *Store) LoadDelivery(id string) (Delivery, error) {
	var d Delivery
	b, err := os.ReadFile(s.deliveryPath(id))
	if os.IsNotExist(err) {
		return Delivery{EmailID: id}, nil
	}
	if err != nil {
		return d, err
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return d, err
	}
	if d.EmailID != id || (d.Status != "delivered" && d.Status != "retry") || d.Attempts < 0 || (d.Status == "delivered" && d.TicketID == "") {
		return d, fmt.Errorf("invalid delivery state for email %s", id)
	}
	return d, nil
}

func (s *Store) SaveDelivery(d Delivery) error {
	return writeJSON(s.deliveryPath(d.EmailID), d, true)
}

// EachDraft reads one draft at a time. Bad files are reported but do not prevent
// later drafts from being delivered. Cancellation interrupts a backlog scan.
func (s *Store) EachDraft(ctx context.Context, visit func(Draft) error) error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.dir, entry.Name()))
		var d Draft
		if err == nil {
			err = json.Unmarshal(b, &d)
		}
		if err == nil && (d.Version != 1 || !resend.ValidID(d.Email.ID) || filepath.Base(s.path(d.Email.ID)) != entry.Name()) {
			err = fmt.Errorf("invalid draft identity or version")
		}
		if err == nil {
			err = visit(d)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("draft %s: %w", entry.Name(), err))
		}
	}
	return errors.Join(errs...)
}

func writeJSON(path string, value any, replace bool) error {
	directory := filepath.Dir(path)
	f, err := os.CreateTemp(directory, ".write-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := json.NewEncoder(f).Encode(value); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if replace {
		if err := os.Rename(f.Name(), path); err != nil {
			return err
		}
	} else {
		if err := os.Link(f.Name(), path); err != nil && !os.IsExist(err) {
			return err
		}
	}
	return syncDir(directory)
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
