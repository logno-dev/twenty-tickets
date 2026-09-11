package resend

import (
	"testing"
	"time"
)

func TestTimeoutConfiguration(t *testing.T) {
	for _, timeout := range []time.Duration{0, -time.Second, 46 * time.Second} {
		if _, err := NewWithTimeout("test", timeout); err == nil {
			t.Errorf("accepted invalid timeout %s", timeout)
		}
	}
	for _, timeout := range []time.Duration{DefaultTimeout, MaxTimeout} {
		if _, err := NewWithTimeout("test", timeout); err != nil {
			t.Fatal(err)
		}
	}
}
