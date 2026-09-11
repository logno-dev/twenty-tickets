// Package twenty integrates with the workspace's custom Ticket object.
package twenty

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"twenty-tickets/internal/api"
)

type Client struct{ api *api.Client }

// New expects the instance root, e.g. https://crm.example.com (without /rest).
func New(base, key string) (*Client, error) {
	c, err := api.New(base, key)
	if err != nil {
		return nil, err
	}
	return &Client{api: c}, nil
}

// Metadata is a read-only connectivity and authentication check.
func (c *Client) Metadata(ctx context.Context) (json.RawMessage, error) {
	var result json.RawMessage
	err := c.api.DoJSON(ctx, http.MethodGet, "/rest/metadata/objects", nil, &result)
	return result, err
}

// REST makes an authenticated request relative to /rest.
func (c *Client) REST(ctx context.Context, method, path string, input, output any) error {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.Contains(path, "..") {
		return fmt.Errorf("Twenty REST path must be relative to /rest")
	}
	return c.api.DoJSON(ctx, method, "/rest"+path, input, output)
}

// TicketID is stable across retries and restarts. Do not change the namespace:
// Twenty's primary-key uniqueness is the final protection against duplicates.
func TicketID(emailID string) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("twenty-tickets:resend:"+emailID)).String()
}

type ticketRecord struct {
	ID        string  `json:"id"`
	DeletedAt *string `json:"deletedAt"`
}

func (t ticketRecord) validate(id string) error {
	if t.ID != id {
		return fmt.Errorf("Twenty response missing or mismatching ticket ID")
	}
	if t.DeletedAt != nil {
		return fmt.Errorf("Twenty ticket has been deleted; manual review required")
	}
	return nil
}

// EnsureTicket creates a ticket or recovers a previous successful creation.
// A lost POST response or failed local receipt write is resolved by the next
// GET; retries never switch to a new ID or overwrite an existing ticket.
func (c *Client) EnsureTicket(ctx context.Context, emailID, subject, body string) (string, error) {
	if strings.TrimSpace(emailID) == "" || strings.TrimSpace(body) == "" {
		return "", fmt.Errorf("email ID and ticket body are required")
	}
	id := TicketID(emailID)
	var found struct {
		Data struct {
			Ticket ticketRecord `json:"ticket"`
		} `json:"data"`
	}
	err := c.REST(ctx, http.MethodGet, "/tickets/"+id+"?depth=0", nil, &found)
	if err == nil {
		if err := found.Data.Ticket.validate(id); err != nil {
			return "", err
		}
		return id, nil
	}
	var status *api.StatusError
	if !errors.As(err, &status) || status.Code != http.StatusNotFound {
		return "", err
	}
	if strings.TrimSpace(subject) == "" {
		subject = "(No subject)"
	}
	// Only set agreed fields. Twenty supplies Status, Type, timestamps and actors.
	input := struct {
		ID             string `json:"id"`
		Name           string `json:"name"`
		IssueOrRequest struct {
			Markdown string `json:"markdown"`
		} `json:"issueOrRequest"`
		Generated bool `json:"generated"`
	}{ID: id, Name: subject, Generated: true}
	input.IssueOrRequest.Markdown = body
	var created struct {
		Data struct {
			Ticket ticketRecord `json:"createTicket"`
		} `json:"data"`
	}
	if err := c.REST(ctx, http.MethodPost, "/tickets?depth=0", input, &created); err != nil {
		return "", err
	}
	if err := created.Data.Ticket.validate(id); err != nil {
		return "", err
	}
	return id, nil
}
