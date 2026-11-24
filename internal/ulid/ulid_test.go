package ulid

import (
	"sync"
	"testing"
	"time"
)

func TestNewAndIsValid(t *testing.T) {
	now := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	prev := New(now)
	if !IsValid(prev) {
		t.Fatalf("expected valid ULID, got %q", prev)
	}
	for i := 0; i < 100; i++ {
		cur := New(now)
		if !IsValid(cur) {
			t.Fatalf("invalid ULID %q", cur)
		}
		if cur <= prev {
			t.Fatalf("expected monotonic ordering within same ms: %q <= %q", cur, prev)
		}
		prev = cur
	}

	later := New(now.Add(5 * time.Millisecond))
	if later <= prev {
		t.Fatalf("expected later timestamp ULID > prev: %q <= %q", later, prev)
	}

	invalid := []string{"", "short", "01JDM4K8N27Q9X2V4R6W8Y0Z1I", "81JDM4K8N27Q9X2V4R6W8Y0Z1A", "01jdm4k8n27q9x2v4r6w8y0z1a", "../etc/passwd"}
	for _, s := range invalid {
		if IsValid(s) {
			t.Errorf("expected invalid ULID for %q", s)
		}
	}
}

func TestConcurrentNew(t *testing.T) {
	now := time.Now().UTC()
	const workers = 8
	const perWorker = 50
	ch := make(chan string, workers*perWorker)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				ch <- New(now)
			}
		}()
	}
	wg.Wait()
	close(ch)

	seen := make(map[string]struct{}, workers*perWorker)
	for id := range ch {
		if !IsValid(id) {
			t.Fatalf("invalid ULID %q", id)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate ULID generated: %q", id)
		}
		seen[id] = struct{}{}
	}
}
