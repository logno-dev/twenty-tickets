package config

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"twenty-tickets/internal/activity"
)

func TestActivityPersistenceAndScopeOutcomes(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := activity.Event{Email: activity.Email{ID: "email", Subject: "Printer problem", From: "user@example.com", To: "support@example.com"}, AttemptID: "first", Stage: "resend_fetch", Status: "failure", Detail: "deadline exceeded"}
	if err := s.RecordActivity(e); err != nil {
		t.Fatal(err)
	}
	e.Email = activity.Email{ID: "email"}
	e.AttemptID = "retry"
	e.Status = "success"
	if err := s.RecordActivity(e); err != nil {
		t.Fatal(err)
	}
	e.RouteID, e.RouteName, e.Stage = "route-a", "Company A", "delivery"
	e.Detail = "Delivered record"
	if err := s.RecordActivity(e); err != nil {
		t.Fatal(err)
	}
	e.RouteID, e.RouteName, e.Stage, e.Status, e.Detail = "route-b", "Company B", "retry", "retry", "Next attempt later"
	if err := s.RecordActivity(e); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	email, err := s.ActivityEmail(context.Background(), "email")
	if err != nil || email.Subject != "Printer problem" || email.From != "user@example.com" {
		t.Fatalf("metadata lost: %+v %v", email, err)
	}
	scopes, err := s.ActivityScopes(context.Background(), "email")
	if err != nil || len(scopes) != 3 {
		t.Fatalf("scopes: %+v %v", scopes, err)
	}
	states := map[string]string{}
	for _, scope := range scopes {
		states[scope.RouteID] = scope.Status
	}
	if states[""] != "success" || states["route-a"] != "success" || states["route-b"] != "retry" {
		t.Fatal("one destination hid another's failure:", states)
	}
	events, err := s.ActivityEvents(context.Background(), "email", 0, 50)
	if err != nil || len(events) != 4 || events[3].Status != "failure" {
		t.Fatalf("earlier failure history lost: %+v %v", events, err)
	}
	for _, query := range []string{"PRINTER", "user@example.com", "support@example.com", "email"} {
		rows, err := s.ActivityEmails(context.Background(), query, 0, 50)
		if err != nil || len(rows) != 1 {
			t.Fatalf("search %q: %v %v", query, rows, err)
		}
	}
}

func TestActivityConcurrencyBoundsAndPagination(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var wg sync.WaitGroup
	for n := range 20 {
		wg.Go(func() {
			if err := s.RecordActivity(activity.Event{Email: activity.Email{ID: fmt.Sprintf("email-%02d", n), Subject: strings.Repeat("é", 400)}, Stage: "webhook_received", Status: "success", Detail: strings.Repeat("x", 2000)}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	first, err := s.ActivityEmails(context.Background(), "", 0, 10)
	if err != nil || len(first) != 10 {
		t.Fatal("first page", err)
	}
	second, err := s.ActivityEmails(context.Background(), "", 10, 10)
	if err != nil || len(second) != 10 {
		t.Fatal("second page", err)
	}
	ids := map[string]bool{}
	for _, row := range append(first, second...) {
		if ids[row.ID] {
			t.Fatal("duplicate across pages")
		}
		ids[row.ID] = true
		if len([]rune(row.Subject)) != 241 {
			t.Fatal("metadata not bounded")
		}
	}
	events, err := s.ActivityEvents(context.Background(), first[0].ID, 0, 1)
	if err != nil || len(events) != 1 || len(events[0].Detail) != 1203 {
		t.Fatal("event detail not bounded", err)
	}
}
