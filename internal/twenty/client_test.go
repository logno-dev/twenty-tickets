package twenty

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func TestMetadata(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/rest/metadata/objects" || r.Header.Get("Authorization") != "Bearer test" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		io.WriteString(w, `{"data":{"objects":[]}}`)
	}))
	defer s.Close()
	c, err := New(s.URL, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Metadata(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestTicketCreationAndRecovery(t *testing.T) {
	for _, loseResponse := range []bool{false, true} {
		t.Run(fmt.Sprint("lost_response=", loseResponse), func(t *testing.T) {
			id := TicketID("email_123")
			if _, err := uuid.Parse(id); err != nil {
				t.Fatal(err)
			}
			if id == TicketID("email_456") {
				t.Fatal("different emails share an ID")
			}
			var mu sync.Mutex
			exists, posts := false, 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.Header.Get("Authorization") != "Bearer test" || r.URL.Query().Get("depth") != "0" {
					t.Error("missing auth/depth")
				}
				if r.Method == "GET" {
					if r.URL.Path != "/rest/tickets/"+id {
						t.Errorf("wrong lookup: %s", r.URL)
					}
					if !exists {
						w.WriteHeader(404)
						return
					}
					fmt.Fprintf(w, `{"data":{"ticket":{"id":%q}}}`, id)
					return
				}
				if r.Method != "POST" || r.URL.Path != "/rest/tickets" || r.Header.Get("Content-Type") != "application/json" {
					t.Errorf("wrong creation request: %s %s", r.Method, r.URL)
				}
				posts++
				var payload map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				if len(payload) != 3 || payload["generated"] != nil {
					t.Errorf("expected only id, name and issueOrRequest; generated must be omitted: %s", payload)
				}
				var name, actualID string
				json.Unmarshal(payload["name"], &name)
				json.Unmarshal(payload["id"], &actualID)
				if name != "Printer issue" || actualID != id {
					t.Errorf("wrong ticket identity: %s %s", name, actualID)
				}
				var richText map[string]string
				if err := json.Unmarshal(payload["issueOrRequest"], &richText); err != nil {
					t.Error(err)
				}
				if len(richText) != 1 || richText["markdown"] != "Please handle.\n\nPrinter is broken." {
					t.Errorf("wrong rich text: %v", richText)
				}
				exists = true
				if loseResponse {
					w.WriteHeader(502)
					return
				} // Committed, but proxy lost the success response.
				w.WriteHeader(201)
				fmt.Fprintf(w, `{"data":{"createTicket":{"id":%q}}}`, id)
			}))
			defer s.Close()
			c, err := New(s.URL, "test")
			if err != nil {
				t.Fatal(err)
			}
			got, err := c.EnsureTicket(context.Background(), "email_123", "Printer issue", "Please handle.\n\nPrinter is broken.")
			if loseResponse && err == nil {
				t.Fatal("lost response must remain unconfirmed")
			}
			if !loseResponse && (err != nil || got != id) {
				t.Fatalf("creation: %s %v", got, err)
			}
			// A new client simulates restart, recovering the committed ticket by ID.
			c, err = New(s.URL, "test")
			if err != nil {
				t.Fatal(err)
			}
			got, err = c.EnsureTicket(context.Background(), "email_123", "Changed local subject", "Changed local body")
			if err != nil || got != id {
				t.Fatalf("recovery: %s %v", got, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if posts != 1 {
				t.Fatalf("duplicate creation or overwrite: %d posts", posts)
			}
		})
	}
}

func TestTicketFailures(t *testing.T) {
	for _, tt := range []struct {
		name           string
		lookupStatus   int
		lookup, create string
		wantPosts      int
	}{
		{"unauthorized lookup", 401, "", "", 0},
		{"rate limited lookup", 429, "", "", 0},
		{"failed lookup", 500, "", "", 0},
		{"missing lookup record", 200, `{"data":{"ticket":null}}`, "", 0},
		{"wrong lookup ID", 200, `{"data":{"ticket":{"id":"wrong"}}}`, "", 0},
		{"missing create record", 404, "", `{"data":{"createTicket":null}}`, 1},
		{"wrong create ID", 404, "", `{"data":{"createTicket":{"id":"wrong"}}}`, 1},
		{"malformed create response", 404, "", `{`, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			posts := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					w.WriteHeader(tt.lookupStatus)
					io.WriteString(w, tt.lookup)
					return
				}
				posts++
				w.WriteHeader(201)
				io.WriteString(w, tt.create)
			}))
			defer s.Close()
			c, err := New(s.URL, "test")
			if err != nil {
				t.Fatal(err)
			}
			if id, err := c.EnsureTicket(context.Background(), "email", "Subject", "Body"); err == nil || id != "" {
				t.Fatalf("failure acknowledged: %s %v", id, err)
			}
			if posts != tt.wantPosts {
				t.Fatalf("got %d posts, want %d", posts, tt.wantPosts)
			}
		})
	}
}

func TestEmptySubjectAndBody(t *testing.T) {
	posts := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.WriteHeader(404)
			return
		}
		posts++
		var input struct {
			ID, Name string
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if input.Name != "(No subject)" {
			t.Errorf("bad fallback: %+v", input)
		}
		fmt.Fprintf(w, `{"data":{"createTicket":{"id":%q}}}`, input.ID)
	}))
	defer s.Close()
	c, err := New(s.URL, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.EnsureTicket(context.Background(), "email", " \n", "Body"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.EnsureTicket(context.Background(), "email", "Subject", " \n"); err == nil {
		t.Fatal("empty body submitted")
	}
	if _, err := c.EnsureTicket(context.Background(), "", "Subject", "Body"); err == nil {
		t.Fatal("empty ID submitted")
	}
	if posts != 1 {
		t.Fatal(posts)
	}
}

func TestConcurrentTicketCreation(t *testing.T) {
	var mu sync.Mutex
	created := false
	creates := 0
	id := TicketID("email")
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == "GET" {
			if !created {
				w.WriteHeader(404)
				return
			}
			fmt.Fprintf(w, `{"data":{"ticket":{"id":%q}}}`, id)
			return
		}
		if created {
			w.WriteHeader(400)
			return
		} // Twenty rejects duplicate primary keys.
		var input map[string]any
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if input["id"] != id {
			t.Error("unstable creation identity")
		}
		created = true
		creates++
		fmt.Fprintf(w, `{"data":{"createTicket":{"id":%q}}}`, id)
	}))
	defer s.Close()
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			c, err := New(s.URL, "test")
			if err != nil {
				t.Error(err)
				return
			}
			_, err = c.EnsureTicket(context.Background(), "email", "Subject", "Body")
			if err != nil && strings.Contains(err.Error(), "HTTP 400") {
				_, err = c.EnsureTicket(context.Background(), "email", "Subject", "Body")
			}
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if creates != 1 {
		t.Fatalf("created %d tickets", creates)
	}
}
