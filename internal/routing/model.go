// Package routing defines immutable per-email destination snapshots and typed
// field mappings. No connection credentials are stored in email drafts.
package routing

import (
	"encoding/json"
	"fmt"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"twenty-tickets/internal/message"
	"twenty-tickets/internal/resend"
	"twenty-tickets/internal/twenty"
)

type Mapping struct {
	Field  string `json:"field"`
	Type   string `json:"type"`
	Source string `json:"source"`
	Value  string `json:"value,omitempty"`
	Target string `json:"target,omitempty"`
}
type Route struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Inbound      string    `json:"inbound"`
	FromDomain   string    `json:"from_domain,omitempty"`
	ConnectionID string    `json:"connection_id"`
	ObjectID     string    `json:"object_id"`
	Singular     string    `json:"singular"`
	Plural       string    `json:"plural"`
	Mappings     []Mapping `json:"mappings"`
	Enabled      bool      `json:"enabled"`
}
type Destination struct {
	Inbound      string    `json:"inbound"`
	FromDomain   string    `json:"from_domain,omitempty"`
	RouteID      string    `json:"route_id"`
	RouteName    string    `json:"route_name"`
	ConnectionID string    `json:"connection_id"`
	Singular     string    `json:"singular"`
	Plural       string    `json:"plural"`
	Mappings     []Mapping `json:"mappings"`
	Legacy       bool      `json:"legacy,omitempty"`
}

var domainPattern = regexp.MustCompile(`(?i)^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func (r Route) Matches(to []string, from string) bool {
	f, err := message.NewRecipientFilter(r.Inbound)
	return err == nil && strings.TrimSpace(r.Inbound) != "" && r.Enabled && f.Matches(to) && (r.FromDomain == "" || from == "" || matchesDomain(from, r.FromDomain))
}
func (r Route) Snapshot() Destination {
	return Destination{Inbound: r.Inbound, FromDomain: r.FromDomain, RouteID: r.ID, RouteName: r.Name, ConnectionID: r.ConnectionID, Singular: r.Singular, Plural: r.Plural, Mappings: append([]Mapping(nil), r.Mappings...)}
}
func (d Destination) Matches(to []string, from string) bool {
	f, err := message.NewRecipientFilter(d.Inbound)
	return err == nil && f.Matches(to) && (d.FromDomain == "" || matchesDomain(from, d.FromDomain))
}
func matchesDomain(from, allowed string) bool {
	address, err := mail.ParseAddress(strings.TrimSpace(from))
	if err != nil {
		return false
	}
	at := strings.LastIndexByte(address.Address, '@')
	return at > 0 && strings.EqualFold(address.Address[at+1:], allowed)
}
func (d Destination) RecordID(emailID string) string {
	if d.Legacy {
		return twenty.TicketID(emailID)
	}
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("twenty-tickets:route:"+d.RouteID+":resend:"+emailID)).String()
}

func Validate(route *Route, object twenty.Object) error {
	if strings.TrimSpace(route.Name) == "" || strings.TrimSpace(route.Inbound) == "" {
		return fmt.Errorf("route name and inbound email are required")
	}
	if _, err := message.NewRecipientFilter(route.Inbound); err != nil {
		return err
	}
	route.FromDomain = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(route.FromDomain), "@")))
	if len(route.FromDomain) > 253 || (route.FromDomain != "" && !domainPattern.MatchString(route.FromDomain)) {
		return fmt.Errorf("allowed sender domain must be a domain such as example.com")
	}
	if !object.Valid() || route.ObjectID != object.ID {
		return fmt.Errorf("select a valid object")
	}
	fields := map[string]twenty.Field{}
	for _, f := range object.Fields {
		fields[f.Name] = f
	}
	seen := map[string]bool{}
	for i := range route.Mappings {
		m := &route.Mappings[i]
		f, ok := fields[m.Field]
		if !ok || !f.Editable() || seen[m.Field] {
			return fmt.Errorf("invalid or duplicate field %s", m.Field)
		}
		seen[m.Field] = true
		m.Type = f.Type // The browser cannot override metadata types.
		m.Target = f.APIName()
		if m.Source == "default" {
			if f.Required() {
				return fmt.Errorf("%s requires a value and has no default", f.Label)
			}
			continue
		}
		if !f.Supported() {
			return fmt.Errorf("%s (%s) currently supports only its Twenty default", f.Label, f.Type)
		}
		if m.Source == "empty" {
			if f.IsNullable == nil || !*f.IsNullable {
				return fmt.Errorf("%s cannot be explicitly empty", f.Label)
			}
			continue
		}
		if m.Source == "fixed" {
			value, err := typed(*m, m.Value)
			if err != nil {
				return fmt.Errorf("%s: %w", f.Label, err)
			}
			if f.Type == "SELECT" || f.Type == "MULTI_SELECT" {
				values := []string{m.Value}
				if list, ok := value.([]string); ok {
					values = list
				}
				for _, v := range values {
					valid := false
					for _, option := range f.Options {
						if option.Value == v {
							valid = true
						}
					}
					if !valid {
						return fmt.Errorf("%s has an invalid select option", f.Label)
					}
				}
			}
			if (f.Type == "NUMBER" || f.Type == "NUMERIC") && (f.Settings.DataType == "int" || f.Settings.DataType == "bigint") {
				if _, err := strconv.ParseInt(m.Value, 10, 64); err != nil {
					return fmt.Errorf("%s requires an integer", f.Label)
				}
			}
			continue
		}
		switch m.Source {
		case "subject", "cleaned_subject", "raw_subject", "body", "cleaned_body", "raw_body", "from", "email_id", "message_id", "received_at":
		default:
			return fmt.Errorf("unknown mapping source")
		}
		if f.Type != "TEXT" && f.Type != "RICH_TEXT" && !(m.Source == "received_at" && (f.Type == "DATE_TIME" || f.Type == "DATE")) {
			return fmt.Errorf("%s needs a fixed value or default for type %s", f.Label, f.Type)
		}
	}
	for _, f := range object.Fields {
		if f.Editable() && f.Required() && !seen[f.Name] {
			return fmt.Errorf("%s requires a mapping", f.Label)
		}
	}
	route.Singular, route.Plural = object.NameSingular, object.NamePlural
	return nil
}

func (d Destination) Payload(email resend.Email, body string) (map[string]any, error) {
	fields := map[string]any{}
	for _, m := range d.Mappings {
		field := m.Target
		if field == "" {
			field = m.Field
		}
		if m.Source == "default" {
			continue
		}
		if m.Source == "empty" {
			fields[field] = nil
			continue
		}
		value := m.Value
		switch m.Source {
		case "fixed":
		case "subject", "cleaned_subject":
			value = message.CleanSubject(email.Subject)
			if strings.TrimSpace(value) == "" {
				value = "(No subject)"
			}
		case "raw_subject":
			value = email.Subject
			if strings.TrimSpace(value) == "" {
				value = "(No subject)"
			}
		case "body", "cleaned_body":
			value = body
		case "raw_body":
			if email.Text != nil {
				value = *email.Text
			} else {
				value = ""
			}
		case "from":
			value = email.From
		case "email_id":
			value = email.ID
		case "message_id":
			value = email.MessageID
		case "received_at":
			value = email.CreatedAt
			if m.Type == "DATE" {
				t, err := time.Parse(time.RFC3339Nano, value)
				if err != nil {
					return nil, fmt.Errorf("email has no valid received date")
				}
				value = t.Format("2006-01-02")
			}
		default:
			return nil, fmt.Errorf("invalid mapping source")
		}
		v, err := typed(m, value)
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", m.Field, err)
		}
		fields[field] = v
	}
	return fields, nil
}

func typed(m Mapping, value string) (any, error) {
	switch m.Type {
	case "TEXT", "SELECT":
		return value, nil
	case "RICH_TEXT":
		return map[string]string{"markdown": value}, nil
	case "BOOLEAN":
		if value == "true" {
			return true, nil
		}
		if value == "false" {
			return false, nil
		}
	case "NUMBER", "NUMERIC":
		if _, err := strconv.ParseFloat(value, 64); err == nil && json.Valid([]byte(value)) {
			return json.Number(value), nil
		}
	case "MULTI_SELECT":
		var values []string
		if json.Unmarshal([]byte(value), &values) == nil && values != nil {
			return values, nil
		}
	case "UUID", "RELATION":
		if id, err := uuid.Parse(value); err == nil {
			return id.String(), nil
		}
	case "DATE":
		if _, err := time.Parse("2006-01-02", value); err == nil {
			return value, nil
		}
	case "DATE_TIME":
		if _, err := time.Parse(time.RFC3339Nano, value); err == nil {
			return value, nil
		}
	}
	return nil, fmt.Errorf("invalid value for %s", m.Type)
}
