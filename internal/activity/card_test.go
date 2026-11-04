package activity

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCardRenderSteps(t *testing.T) {
	card := NewCard("coachly", "gemini-3.8-flash-high", "normal")
	card.AddOrUpdateStep("1", "read config/thresholds.yaml", StepDone, 2*time.Second, 0, "")
	card.AddOrUpdateStep("2", "go build ./...", StepDone, 4*time.Second, 0, "")
	card.AddOrUpdateStep("3", "go test ./...", StepRunning, 0, 0, "")

	html := card.RenderHTML()
	if !strings.Contains(html, "<b>coachly</b>") {
		t.Errorf("expected project name in bold, got:\n%s", html)
	}
	if !strings.Contains(html, "✔ read config/thresholds.yaml") {
		t.Errorf("expected completed step, got:\n%s", html)
	}
	if !strings.Contains(html, "▶ go test ./...") {
		t.Errorf("expected running step, got:\n%s", html)
	}

	// Update running step to failed
	card.AddOrUpdateStep("3", "go test ./...", StepFailed, 5*time.Second, 1, "FAIL: TestConfigLoad\npanic: nil pointer")
	htmlFailed := card.RenderHTML()
	if !strings.Contains(htmlFailed, "✖ go test ./...  (5s) (exit 1)") {
		t.Errorf("expected failed step with exit code, got:\n%s", htmlFailed)
	}
	if !strings.Contains(htmlFailed, "<pre><code>FAIL: TestConfigLoad") {
		t.Errorf("expected stderr in pre code, got:\n%s", htmlFailed)
	}

	// Mark done
	card.MarkDone("All done")
	htmlDone := card.RenderHTML()
	if !strings.Contains(htmlDone, "Done in") {
		t.Errorf("expected 'Done in' in header, got:\n%s", htmlDone)
	}
}

func TestCardFolding(t *testing.T) {
	card := NewCard("coachly", "gemini-3.8-flash-high", "normal")
	for i := 1; i <= 20; i++ {
		card.AddOrUpdateStep(fmt.Sprintf("%d", i), fmt.Sprintf("step %d", i), StepDone, time.Second, 0, "")
	}

	html := card.RenderHTML()
	// Should show +8 earlier steps
	if !strings.Contains(html, "+8 earlier steps") {
		t.Errorf("expected '+8 earlier steps' fold indicator, got:\n%s", html)
	}
	// Should not show step 1, but should show step 20
	if strings.Contains(html, "✔ step 1\n") {
		t.Errorf("step 1 should be folded, got:\n%s", html)
	}
	if !strings.Contains(html, "✔ step 20") {
		t.Errorf("step 20 should be visible, got:\n%s", html)
	}
}

func TestDebouncerThrottling(t *testing.T) {
	card := NewCard("test", "model", "normal")

	var callCount int32
	updateFn := func(html string) error {
		atomic.AddInt32(&callCount, 1)
		return nil
	}

	interval := 50 * time.Millisecond
	debouncer := NewDebouncer(card, interval, updateFn)

	// Fire 20 updates in rapid succession
	for i := 0; i < 20; i++ {
		card.AddOrUpdateStep("step", fmt.Sprintf("run %d", i), StepRunning, 0, 0, "")
		debouncer.Trigger()
	}

	// Immediate calls should be throttled (at most 1 initial call immediately)
	initial := atomic.LoadInt32(&callCount)
	if initial > 2 {
		t.Errorf("expected throttled calls <= 2, got %d", initial)
	}

	// Wait for debounce timer to fire
	time.Sleep(100 * time.Millisecond)

	// Flush final state
	debouncer.Flush()

	total := atomic.LoadInt32(&callCount)
	if total > 4 {
		t.Errorf("expected total calls <= 4, got %d", total)
	}
}
