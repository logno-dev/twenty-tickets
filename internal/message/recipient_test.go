package message

import "testing"

func TestRecipientFilter(t *testing.T) {
	filter, err := NewRecipientFilter("  Tickets <support@example.com>  ")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		to   []string
		want bool
	}{
		{"exact", []string{"support@example.com"}, true},
		{"case and display name", []string{"  Support Team <SUPPORT@EXAMPLE.COM>  "}, true},
		{"multiple recipients", []string{"other@example.com", "support@example.com"}, true},
		{"quoted display name", []string{`"Support, Team" <support@example.com>`}, true},
		{"different mailbox", []string{"other@example.com"}, false},
		{"substring", []string{"not-support@example.com"}, false},
		{"different domain", []string{"support@example.com.other.test"}, false},
		{"plus tag", []string{"support+tag@example.com"}, false},
		{"display name only", []string{`"support@example.com" <other@example.com>`}, false},
		{"malformed", []string{"not an address"}, false},
		{"missing", nil, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := filter.Matches(tt.to); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
	for _, invalid := range []string{"not an address", "a@example.com,b@example.com", "support@"} {
		if _, err := NewRecipientFilter(invalid); err == nil {
			t.Errorf("accepted invalid setting: %q", invalid)
		}
	}
	all, err := NewRecipientFilter(" \t")
	if err != nil || !all.Matches(nil) || !all.Matches([]string{"other@example.com"}) {
		t.Fatal("empty filter must preserve unfiltered behavior")
	}
}
