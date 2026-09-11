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
	"twenty-tickets/internal/routing"
)

type Draft struct {
	Version      int                   `json:"version"`
	EventID      string                `json:"event_id"`
	SavedAt      time.Time             `json:"saved_at"`
	Status       string                `json:"status"`
	ReviewReason string                `json:"review_reason,omitempty"`
	Email        resend.Email          `json:"email"`
	Body         string                `json:"body"`
	Forwarded    bool                  `json:"forwarded"`
	Destinations []routing.Destination `json:"destinations,omitempty"`
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
	if err := os.MkdirAll(filepath.Join(dir, "assignments"), 0700); err != nil {
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
	RouteID       string    `json:"route_id,omitempty"`
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
	return s.LoadDestinationDelivery(id, "")
}

func (s *Store) LoadDestinationDelivery(id, routeID string) (Delivery, error) {
	var d Delivery
	key := id
	if routeID != "" {
		key += "@route:" + routeID
	}
	b, err := os.ReadFile(s.deliveryPath(key))
	if os.IsNotExist(err) {
		return Delivery{EmailID: id, RouteID: routeID}, nil
	}
	if err != nil {
		return d, err
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return d, err
	}
	if d.EmailID != id || d.RouteID != routeID || (d.Status != "delivered" && d.Status != "retry") || d.Attempts < 0 || (d.Status == "delivered" && d.TicketID == "") {
		return d, fmt.Errorf("invalid delivery state for email %s", id)
	}
	return d, nil
}

func (s *Store) SaveDelivery(d Delivery) error {
	key := d.EmailID
	if d.RouteID != "" {
		key += "@route:" + d.RouteID
	}
	return writeJSON(s.deliveryPath(key), d, true)
}

func (s *Store) Draft(id string) (Draft, error) {
	var d Draft
	b, err := os.ReadFile(s.path(id))
	if err != nil {
		return d, err
	}
	err = json.Unmarshal(b, &d)
	if err == nil && d.Email.ID != id {
		err = fmt.Errorf("invalid draft identity")
	}
	return d, err
}
func (s *Store) Destinations(d Draft) ([]routing.Destination, error) {
	if d.Version == 2 {
		return d.Destinations, nil
	}
	legacy, err := s.LoadDelivery(d.Email.ID)
	if err != nil {
		return nil, err
	}
	if legacy.Status == "delivered" {
		return nil, nil
	}
	b, err := os.ReadFile(filepath.Join(s.dir, "assignments", filepath.Base(s.path(d.Email.ID))))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var destinations []routing.Destination
	err = json.Unmarshal(b, &destinations)
	return destinations, err
}
func (s *Store) AssignLegacy(id string, destination routing.Destination) error {
	d, err := s.Draft(id)
	if err != nil {
		return err
	}
	if d.Version != 1 || d.Status != "pending_twenty" {
		return fmt.Errorf("only legacy pending drafts can be assigned")
	}
	state, err := s.LoadDelivery(id)
	if err != nil {
		return err
	}
	if state.Status == "delivered" {
		return fmt.Errorf("legacy draft was already delivered")
	}
	old, err := s.Destinations(d)
	if err != nil {
		return err
	}
	if len(old) > 0 {
		return fmt.Errorf("draft already has a destination")
	}
	destination.Legacy = true
	return writeJSON(filepath.Join(s.dir, "assignments", filepath.Base(s.path(id))), []routing.Destination{destination}, false)
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
		if err == nil && ((d.Version != 1 && d.Version != 2) || !resend.ValidID(d.Email.ID) || filepath.Base(s.path(d.Email.ID)) != entry.Name()) {
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
