package db

import (
	"path/filepath"
	"testing"
	"time"
)

func TestDBSessionsAndQueue(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test.db")

	database, err := Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer database.Close()

	// 1. Session creation & retrieval
	sess, err := database.GetOrCreateSession("coachly", "gemini-3.8-flash-high", "normal")
	if err != nil {
		t.Fatalf("failed to get/create session: %v", err)
	}
	if sess.ProjectName != "coachly" || sess.Model != "gemini-3.8-flash-high" {
		t.Errorf("unexpected session: %+v", sess)
	}

	// Update model
	if err := database.UpdateSessionModel(sess.ID, "claude-sonnet-4-6"); err != nil {
		t.Fatalf("failed to update model: %v", err)
	}
	sessUpdated, err := database.GetOrCreateSession("coachly", "gemini-3.8-flash-high", "normal")
	if err != nil || sessUpdated.Model != "claude-sonnet-4-6" {
		t.Fatalf("expected updated model 'claude-sonnet-4-6', got %s", sessUpdated.Model)
	}

	// 2. Queueing messages
	id1, ahead1, err := database.EnqueueMessage(sess.ID, 12345, "first message", "")
	if err != nil {
		t.Fatalf("enqueue 1 failed: %v", err)
	}
	if ahead1 != 0 {
		t.Errorf("expected 0 ahead for first message, got %d", ahead1)
	}

	id2, ahead2, err := database.EnqueueMessage(sess.ID, 12345, "second message", "")
	if err != nil {
		t.Fatalf("enqueue 2 failed: %v", err)
	}
	if ahead2 != 1 {
		t.Errorf("expected 1 ahead for second message, got %d", ahead2)
	}

	count, err := database.GetPendingQueueCount(sess.ID)
	if err != nil || count != 2 {
		t.Errorf("expected 2 pending, got %d", count)
	}

	// Get next
	item1, err := database.GetNextPendingMessage(sess.ID)
	if err != nil || item1.ID != id1 || item1.Text != "first message" {
		t.Fatalf("expected item1, got %+v", item1)
	}

	// Mark done
	if err := database.MarkMessageStatus(id1, "done"); err != nil {
		t.Fatalf("failed to mark done: %v", err)
	}

	item2, err := database.GetNextPendingMessage(sess.ID)
	if err != nil || item2.ID != id2 || item2.Text != "second message" {
		t.Fatalf("expected item2, got %+v", item2)
	}
	_ = database.MarkMessageStatus(id2, "done")

	// Test prioritize
	id3, _, err := database.EnqueueMessage(sess.ID, 12345, "third message", "")
	if err != nil {
		t.Fatalf("enqueue 3 failed: %v", err)
	}
	id4, _, err := database.EnqueueMessage(sess.ID, 12345, "fourth message", "")
	if err != nil {
		t.Fatalf("enqueue 4 failed: %v", err)
	}
	// Prioritize id4 over id3
	if err := database.PrioritizeMessage(id4); err != nil {
		t.Fatalf("prioritize failed: %v", err)
	}
	nextPrio, err := database.GetNextPendingMessage(sess.ID)
	if err != nil || nextPrio.ID != id4 {
		t.Fatalf("expected prioritized id4 (%d), got id %d", id4, nextPrio.ID)
	}
	// Test cancel
	if err := database.CancelMessage(id4); err != nil {
		t.Fatalf("cancel failed: %v", err)
	}
	nextAfterCancel, err := database.GetNextPendingMessage(sess.ID)
	if err != nil || nextAfterCancel.ID != id3 {
		t.Fatalf("expected next after cancel id3 (%d), got id %d", id3, nextAfterCancel.ID)
	}

	// 3. Approvals
	expiresAt := time.Now().Add(10 * time.Minute).Unix()
	if err := database.CreateApproval("app_123", sess.ID, "rm -rf build/", expiresAt); err != nil {
		t.Fatalf("failed to create approval: %v", err)
	}
	app, err := database.GetApproval("app_123")
	if err != nil || app.Prompt != "rm -rf build/" || app.Status != "pending" {
		t.Fatalf("unexpected approval: %+v", app)
	}

	if err := database.ResolveApproval("app_123", "allowed"); err != nil {
		t.Fatalf("failed to resolve approval: %v", err)
	}
	appResolved, err := database.GetApproval("app_123")
	if err != nil || appResolved.Status != "allowed" {
		t.Fatalf("expected status 'allowed', got %s", appResolved.Status)
	}

	// 4. Telegram updates
	if err := database.SetHandledUpdateID(1001); err != nil {
		t.Fatalf("failed to set update id: %v", err)
	}
	lastID, err := database.GetLastHandledUpdateID()
	if err != nil || lastID != 1001 {
		t.Fatalf("expected last update id 1001, got %d", lastID)
	}
}
