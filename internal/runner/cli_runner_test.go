package runner

import (
	"testing"
)

func TestCLIRunnerLifecycle(t *testing.T) {
	r := NewCLIRunner()
	if r.IsBusy() {
		t.Errorf("expected new CLIRunner not to be busy")
	}

	r.SetAutoApprove(true)
	if !r.IsAutoApprove() {
		t.Errorf("expected auto-approve to be true")
	}

	if err := r.Start("/tmp", "gemini-3.8-flash-high", "conv_test_123"); err != nil {
		t.Fatalf("unexpected Start error: %v", err)
	}

	if r.cwd != "/tmp" || r.model != "gemini-3.8-flash-high" || r.convID != "conv_test_123" {
		t.Errorf("unexpected runner state: cwd=%s, model=%s, convID=%s", r.cwd, r.model, r.convID)
	}

	if err := r.Interrupt(); err != nil {
		t.Errorf("unexpected Interrupt error: %v", err)
	}
	if err := r.Stop(); err != nil {
		t.Errorf("unexpected Stop error: %v", err)
	}
}
