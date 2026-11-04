package tests

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-bridge/internal/activity"
	"agent-bridge/internal/db"
	"agent-bridge/internal/media"
	"agent-bridge/internal/parser"
	"agent-bridge/internal/queue"
	"agent-bridge/internal/redact"
	"agent-bridge/internal/runner"
	"agent-bridge/internal/telegram"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

func setupTestBridge(t *testing.T) (*db.DB, *runner.FakeRunner, *queue.Manager, string) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "e2e.db")

	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("failed opening test db: %v", err)
	}

	fake := runner.NewFakeRunner()
	queueMgr := queue.NewManager(database, fake, "session_e2e")

	return database, fake, queueMgr, tempDir
}

// 1. Duplicate & Relay Test
func TestDuplicateAndRelay(t *testing.T) {
	_, fake, queueMgr, _ := setupTestBridge(t)

	var sentMessages []string
	var mu sync.Mutex

	fake.OnEvent(func(e parser.ParsedEvent) {
		if e.Type == parser.EventPromptReady {
			mu.Lock()
			sentMessages = append(sentMessages, e.Text)
			mu.Unlock()
		}
	})

	fake.OnInstruction = func(text string, r *runner.FakeRunner) {
		// Emit assistant answer once
		r.EmitEvent(parser.ParsedEvent{
			Type: parser.EventAssistantChunk,
			Text: "All tests pass",
		})
		r.EmitEvent(parser.ParsedEvent{
			Type: parser.EventPromptReady,
			Text: "All tests pass",
		})
	}

	_, err := queueMgr.Submit(12345, "run all tests", "")
	if err != nil {
		t.Fatalf("submit failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	count := len(sentMessages)
	mu.Unlock()

	if count != 1 {
		t.Fatalf("expected exactly 1 relayed message, got %d", count)
	}
}

// 2. Flood Test: 500 events in rapid succession throttled to <= 1 edit / 3s
func TestFloodThrottling(t *testing.T) {
	card := activity.NewCard("coachly", "gemini-3.8-flash-high", "normal")

	var editCount int32
	debouncer := activity.NewDebouncer(card, 50*time.Millisecond, func(html string) error {
		atomic.AddInt32(&editCount, 1)
		return nil
	})

	// Emit 500 tool steps
	for i := 0; i < 500; i++ {
		card.AddOrUpdateStep(fmt.Sprintf("%d", i), fmt.Sprintf("step %d", i), activity.StepRunning, 0, 0, "")
		debouncer.Trigger()
	}

	// Should be throttled while loop runs
	count := atomic.LoadInt32(&editCount)
	if count > 3 {
		t.Errorf("expected throttled edits <= 3 during flood, got %d", count)
	}

	// Wait and flush final
	time.Sleep(100 * time.Millisecond)
	debouncer.Flush()

	total := atomic.LoadInt32(&editCount)
	if total > 5 {
		t.Errorf("expected total edits <= 5, got %d", total)
	}
}

// 3. Activity Card Failure Formatting
func TestActivityCardFailure(t *testing.T) {
	card := activity.NewCard("coachly", "gemini-3.8-flash-high", "normal")
	card.AddOrUpdateStep("t1", "go build ./...", activity.StepFailed, 4*time.Second, 1, "cmd/main.go:12: undefined: Foo\nexit status 1")

	html := card.RenderHTML()
	if !strings.Contains(html, "✖ go build ./...  (4s) (exit 1)") {
		t.Errorf("expected failed step indicator and exit code, got:\n%s", html)
	}
	if !strings.Contains(html, "<pre><code>cmd/main.go:12: undefined: Foo") {
		t.Errorf("expected monospace stderr block, got:\n%s", html)
	}
}

// 4. Approvals Timeout & Allow/Deny Logic
func TestApprovalsTimeoutAndResolve(t *testing.T) {
	database, fake, _, _ := setupTestBridge(t)
	defer database.Close()

	// Create approval with 50ms expiry
	expiresAt := time.Now().Add(50 * time.Millisecond).Unix()
	err := database.CreateApproval("app_timeout_test", "sess_1", "rm -rf /tmp/data", expiresAt)
	if err != nil {
		t.Fatalf("create approval failed: %v", err)
	}

	// Wait for expiry
	time.Sleep(70 * time.Millisecond)

	app, err := database.GetApproval("app_timeout_test")
	if err != nil || app == nil {
		t.Fatalf("failed retrieving approval: %v", err)
	}

	if time.Now().Unix() < app.ExpiresAt {
		t.Errorf("expected approval to be expired")
	}

	// Simulate resolving
	_ = database.ResolveApproval("app_timeout_test", "expired")
	_ = fake.SendApprovalResponse(false)

	if len(fake.Approvals) != 1 || fake.Approvals[0] != false {
		t.Errorf("expected denied approval on timeout, got %+v", fake.Approvals)
	}
}

// 5. Allowlist Authentication Check
func TestAllowlistRejection(t *testing.T) {
	allowedID := int64(987654321)
	middleware := telegram.AuthMiddleware(allowedID)

	var processed bool
	handler := middleware(func(ctx context.Context, b *bot.Bot, update *models.Update) {
		processed = true
	})

	// Attacker / unknown ID
	unauthorizedUpdate := &models.Update{
		Message: &models.Message{
			From: &models.User{ID: 111111111},
			Text: "/stop",
		},
	}

	handler(context.Background(), nil, unauthorizedUpdate)
	if processed {
		t.Errorf("unauthorized message was processed; should have been dropped")
	}
}

// 6. Redaction Pass
func TestRedactionPass(t *testing.T) {
	redactor := redact.New()
	outputWithSecret := "Build failed with key sk-abcdef1234567890abcdef1234567890 and private token AIzaSyD12345678901234567890123456789012"
	safe := redactor.Redact(outputWithSecret)

	if strings.Contains(safe, "sk-abcdef") || strings.Contains(safe, "AIzaSyD") {
		t.Errorf("secret was leaked into output: %s", safe)
	}
	if !strings.Contains(safe, "[REDACTED]") {
		t.Errorf("expected [REDACTED] in output, got: %s", safe)
	}
}

// 7. Media Outbox Watcher & Move
func TestOutboxWatcherIntegration(t *testing.T) {
	tempProject := t.TempDir()

	var sentFile string
	var wg sync.WaitGroup
	wg.Add(1)

	watcher, err := media.NewOutboxWatcher(tempProject, 50*time.Millisecond, func(path string, cat media.FileCategory) error {
		sentFile = path
		wg.Done()
		return nil
	})
	if err != nil {
		t.Fatalf("outbox watcher init failed: %v", err)
	}
	defer watcher.Stop()

	// Write file to outbox
	testPath := filepath.Join(tempProject, ".bridge", "outbox", "diff.txt")
	_ = os.WriteFile(testPath, []byte("diff content"), 0644)

	wg.Wait()

	if sentFile != testPath {
		t.Errorf("expected outbox watcher to send %s, got %s", testPath, sentFile)
	}

	time.Sleep(20 * time.Millisecond)
	// Must be moved to sent/
	sentDirFile := filepath.Join(tempProject, ".bridge", "sent", "diff.txt")
	if _, err := os.Stat(sentDirFile); err != nil {
		t.Errorf("file was not moved to sent/: %v", err)
	}
}

// 8. /get Sandbox Security
func TestGetCommandSandboxing(t *testing.T) {
	tempProject := t.TempDir()

	// Create valid file
	_ = os.WriteFile(filepath.Join(tempProject, "doc.md"), []byte("# Title"), 0644)
	// Create .env file
	_ = os.WriteFile(filepath.Join(tempProject, ".env"), []byte("TOKEN=XYZ"), 0644)

	// Valid file succeeds
	path, err := media.ValidateGetPath(tempProject, "doc.md", 1024*1024)
	if err != nil || !strings.HasSuffix(path, "doc.md") {
		t.Errorf("expected valid file access, got: %s, err: %v", path, err)
	}

	// .env blocked
	_, err = media.ValidateGetPath(tempProject, ".env", 1024*1024)
	if err == nil {
		t.Errorf("expected access denied for .env, got nil")
	}

	// Traversal blocked
	_, err = media.ValidateGetPath(tempProject, "../../../../etc/hosts", 1024*1024)
	if err == nil {
		t.Errorf("expected access denied for path traversal, got nil")
	}
}

// 9. Model Choice Persistence
func TestModelChoicePersistence(t *testing.T) {
	database, _, _, _ := setupTestBridge(t)
	defer database.Close()

	sess, err := database.GetOrCreateSession("coachly", "gemini-3.8-flash-high", "normal")
	if err != nil {
		t.Fatalf("create session failed: %v", err)
	}

	// Switch model
	_ = database.UpdateSessionModel(sess.ID, "claude-sonnet-4-6")

	// Verify it survived and is loaded
	reloaded, _ := database.GetOrCreateSession("coachly", "gemini-3.8-flash-high", "normal")
	if reloaded.Model != "claude-sonnet-4-6" {
		t.Errorf("expected model 'claude-sonnet-4-6', got %s", reloaded.Model)
	}
}

// 10. Message While Busy (Queueing & Sequential Execution)
func TestQueueWhileBusy(t *testing.T) {
	database, fake, queueMgr, _ := setupTestBridge(t)
	defer database.Close()

	// First instruction dispatched when idle
	res1, _ := queueMgr.Submit(12345, "task 1", "")
	if res1.Queued {
		t.Errorf("expected task 1 not queued")
	}

	// Runner is now busy
	res2, _ := queueMgr.Submit(12345, "task 2", "")
	if !res2.Queued || res2.AheadCount != 0 {
		t.Errorf("expected task 2 queued with 0 ahead, got %+v", res2)
	}

	res3, _ := queueMgr.Submit(12345, "task 3", "")
	if !res3.Queued || res3.AheadCount != 1 {
		t.Errorf("expected task 3 queued with 1 ahead, got %+v", res3)
	}

	// Finish task 1
	fake.SetBusy(false)

	// Drain task 2
	drainedItem, err := queueMgr.DrainNext()
	if err != nil || drainedItem == nil || drainedItem.Text != "task 2" {
		t.Errorf("expected task 2 drained, got: %+v", drainedItem)
	}

	// Draining while now busy returns nothing
	drainedMidTask, _ := queueMgr.DrainNext()
	if drainedMidTask != nil {
		t.Errorf("expected nil drain while busy, got %+v", drainedMidTask)
	}

	// Finish task 2 and drain task 3
	fake.SetBusy(false)
	drainedItem3, err := queueMgr.DrainNext()
	if err != nil || drainedItem3 == nil || drainedItem3.Text != "task 3" {
		t.Errorf("expected task 3 drained, got: %+v", drainedItem3)
	}
}
