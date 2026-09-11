package resend

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"twenty-tickets/internal/api"
)

type Email struct {
	ID        string   `json:"id"`
	From      string   `json:"from"`
	To        []string `json:"to"`
	CC        []string `json:"cc"`
	ReplyTo   []string `json:"reply_to"`
	Subject   string   `json:"subject"`
	Text      *string  `json:"text"`
	MessageID string   `json:"message_id"`
	CreatedAt string   `json:"created_at"`
}

var validID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

func ValidID(id string) bool { return validID.MatchString(id) }

type Client struct{ api *api.Client }

const DefaultTimeout = 30 * time.Second
const MaxTimeout = 45 * time.Second

func New(key string) (*Client, error) { return NewWithTimeout(key, DefaultTimeout) }

func NewWithTimeout(key string, timeout time.Duration) (*Client, error) {
	if timeout <= 0 || timeout > MaxTimeout {
		return nil, fmt.Errorf("RESEND_HTTP_TIMEOUT must be greater than zero and at most 45s")
	}
	c, err := api.NewWithTimeout("https://api.resend.com", key, timeout)
	if err != nil {
		return nil, err
	}
	return &Client{api: c}, nil
}

func NewWithURL(base, key string) (*Client, error) {
	c, err := api.NewWithTimeout(base, key, DefaultTimeout)
	if err != nil {
		return nil, err
	}
	return &Client{api: c}, nil
}

func (c *Client) Receive(ctx context.Context, id string) (Email, error) {
	var email Email
	if !ValidID(id) {
		return email, fmt.Errorf("invalid received email ID")
	}
	err := c.api.DoJSON(ctx, http.MethodGet, "/emails/receiving/"+id+"?html_format=cid", nil, &email)
	if err != nil {
		return email, err
	}
	if email.ID != id {
		return email, fmt.Errorf("received email ID does not match requested ID")
	}
	return email, nil
}
