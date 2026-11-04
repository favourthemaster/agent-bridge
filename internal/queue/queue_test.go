package queue

import (
	"path/filepath"
	"testing"

	"agent-bridge/internal/db"
	"agent-bridge/internal/runner"
)

func TestQueueSubmitAndDrain(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "queue_test.db")

	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	defer database.Close()

	fake := runner.NewFakeRunner()
	mgr := NewManager(database, fake, "test_session_1")

	// 1. Submit when idle
	res1, err := mgr.Submit(123, "first instruction", "")
	if err != nil {
		t.Fatalf("submit 1 failed: %v", err)
	}
	if res1.Queued {
		t.Errorf("expected not queued when idle, got queued")
	}
	if len(fake.Instructions) != 1 || fake.Instructions[0] != "first instruction" {
		t.Errorf("expected instruction dispatched directly, got %+v", fake.Instructions)
	}

	// Runner is now busy (from WriteInstruction)
	// 2. Submit while busy -> should queue
	res2, err := mgr.Submit(123, "second instruction", "")
	if err != nil {
		t.Fatalf("submit 2 failed: %v", err)
	}
	if !res2.Queued || res2.AheadCount != 0 {
		t.Errorf("expected queued with 0 ahead, got: %+v", res2)
	}

	res3, err := mgr.Submit(123, "third instruction", "")
	if err != nil {
		t.Fatalf("submit 3 failed: %v", err)
	}
	if !res3.Queued || res3.AheadCount != 1 {
		t.Errorf("expected queued with 1 ahead, got: %+v", res3)
	}

	count, _ := mgr.PendingCount()
	if count != 2 {
		t.Errorf("expected 2 pending messages, got %d", count)
	}

	// 3. Mark runner idle and drain
	fake.SetBusy(false)
	item, err := mgr.DrainNext()
	if err != nil || item == nil {
		t.Fatalf("drain failed: %v", err)
	}
	if item.Text != "second instruction" {
		t.Errorf("expected 'second instruction', got %s", item.Text)
	}

	// 4. Clear remaining queue
	cleared, err := mgr.Clear()
	if err != nil || cleared != 1 {
		t.Errorf("expected 1 cleared, got %d, err: %v", cleared, err)
	}
}
