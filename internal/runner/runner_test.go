package runner

import (
	"sync"
	"testing"
	"time"

	"agent-bridge/internal/parser"
)

func TestFakeRunnerTurn(t *testing.T) {
	fake := NewFakeRunner()

	var received []parser.ParsedEvent
	var mu sync.Mutex

	fake.OnEvent(func(e parser.ParsedEvent) {
		mu.Lock()
		received = append(received, e)
		mu.Unlock()
	})

	fake.OnInstruction = func(text string, r *FakeRunner) {
		r.EmitEvent(parser.ParsedEvent{
			Type: parser.EventToolStart,
			ID:   "t1",
			Text: "go test ./...",
		})
		time.Sleep(10 * time.Millisecond)
		r.EmitEvent(parser.ParsedEvent{
			Type: parser.EventToolEnd,
			ID:   "t1",
			Text: "go test ./...",
			Ok:   true,
		})
		r.EmitTurnSuccess("thinking hard", "All tests passed.")
	}

	if err := fake.WriteInstruction("run tests"); err != nil {
		t.Fatalf("write instruction failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	count := len(received)
	mu.Unlock()

	if count < 4 {
		t.Errorf("expected at least 4 events (tool_start, tool_end, thinking, assistant, ready), got %d", count)
	}

	if fake.IsBusy() {
		t.Errorf("expected fake runner to be idle after turn, but isBusy is true")
	}
}

func TestFakeRunnerApproval(t *testing.T) {
	fake := NewFakeRunner()

	fake.OnInstruction = func(text string, r *FakeRunner) {
		r.EmitEvent(parser.ParsedEvent{
			Type: parser.EventApprovalReq,
			ID:   "app_1",
			Text: "rm -rf build/",
		})
	}

	_ = fake.WriteInstruction("clean build")
	time.Sleep(10 * time.Millisecond)

	if err := fake.SendApprovalResponse(true); err != nil {
		t.Fatalf("send approval response failed: %v", err)
	}

	if len(fake.Approvals) != 1 || !fake.Approvals[0] {
		t.Errorf("expected approved response recorded, got: %+v", fake.Approvals)
	}
}
