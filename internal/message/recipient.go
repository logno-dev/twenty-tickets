package message

import (
	"fmt"
	"net/mail"
	"strings"
)

// RecipientFilter matches one configured To mailbox. Its zero value allows all
// recipients, preserving inbox-only and unfiltered deployments.
type RecipientFilter struct{ address string }

func NewRecipientFilter(value string) (RecipientFilter, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return RecipientFilter{}, nil
	}
	address, err := mail.ParseAddress(value)
	if err != nil {
		return RecipientFilter{}, fmt.Errorf("recipient must be a single valid email address")
	}
	return RecipientFilter{address: address.Address}, nil
}

// Matches compares entire mailbox addresses, not substrings. Plus tags and
// aliases remain distinct. A configured filter rejects missing/malformed To.
func (f RecipientFilter) Matches(to []string) bool {
	if f.address == "" {
		return true
	}
	for _, value := range to {
		address, err := mail.ParseAddress(strings.TrimSpace(value))
		if err == nil && strings.EqualFold(address.Address, f.address) {
			return true
		}
	}
	return false
}
