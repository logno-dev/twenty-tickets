package inbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"twenty-tickets/internal/resend"
)

func TestConcurrentDuplicateSave(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if err := s.Save(Draft{Email: resend.Email{ID: "same"}, Body: "complete"}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("files: %v, error: %v", files, err)
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var d Draft
	if err := json.Unmarshal(b, &d); err != nil || d.Body != "complete" {
		t.Fatalf("partial draft: %s, %v", b, err)
	}
}
