package message

import "testing"

func TestExtract(t *testing.T) {
	tests := []struct {
		name, subject, input, want string
		forwarded                  bool
	}{
		{"plain", "Help", "Hello\r\n\r\nPlease fix this.\r\n", "Hello\n\nPlease fix this.", false},
		{"reply", "Re: Help", "Newest reply\n\nOn Tue, A <a@example.com> wrote:\n> Old", "Newest reply", false},
		{"wrapped reply", "Re: Help", "Newest\nOn Tuesday, September 1,\nA <a@example.com>\nwrote:\nOld", "Newest", false},
		{"quoted history", "Help", "Newest\n\n> old\nold continuation", "Newest", false},
		{"gmail forward", "Fwd: Help", "Please handle.\n\n---------- Forwarded message ---------\nFrom: A <a@example.com>\nDate: Tue\nSubject: Help\nTo: B\n\nMy printer is broken.\n\nOn Mon, B wrote:\n> Older", "Please handle.\n\nMy printer is broken.", true},
		{"apple forward", "Fwd: Help", "FYI\nBegin forwarded message:\n\n> From: A\n> Date: Tue\n> Subject: Help\n> To: B\n>\n> Please fix this.\n>\n> On Mon, B wrote:\n>> Old", "FYI\n\nPlease fix this.", true},
		{"outlook forward", "FW: Help", "Please handle\n__________\nFrom: A\nSent: Tue\nTo: B\nSubject: Help\n\nNew issue\n\nFrom: B\nSent: Mon\nTo: A\nSubject: Old\n\nOld issue", "Please handle\n\nNew issue", true},
		{"outlook reply", "Re: Help", "New reply\n__________\nFrom: A\nSent: Tue\nTo: B\nSubject: Help\n\nOld", "New reply", false},
		{"original forward", "Fw: Help", "Note\n-----Original Message-----\nFrom: A\nSent: Tue\nTo: B\nSubject: Help\n\nNew issue", "Note\n\nNew issue", true},
		{"nested forwards", "Fwd: Help", "---------- Forwarded message ---------\nFrom: A\nTo: B\nSubject: Help\n\nFirst\n---------- Forwarded message ---------\nFrom: C\nTo: A\nSubject: Old\n\nSecond", "First", true},
		{"ordinary prose", "Help", "From: my perspective\nthis needs work.\nOn Tuesday we should meet.", "From: my perspective\nthis needs work.\nOn Tuesday we should meet.", false},
		{"empty", "Help", "\n\t", "", false},
		{"quoted blank before headers", "Fwd: Help", "FYI\nBegin forwarded message:\n>\n> From: A\n> Date: Tue\n> To: B\n> Subject: Help\n>\n> Please help.\n>> Old", "FYI\n\nPlease help.", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, forward := Extract(tt.input, tt.subject)
			if got != tt.want || forward != tt.forwarded {
				t.Fatalf("got %q, %v; want %q, %v", got, forward, tt.want, tt.forwarded)
			}
		})
	}
}
