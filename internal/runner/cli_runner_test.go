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

func TestFormatToolLabel(t *testing.T) {
	// Case 1: Prioritize toolSummary
	infoSummary := &StepToolInfo{
		Name: "run_command",
		Parameters: map[string]interface{}{
			"CommandLine": "instabox exec test-dev -- \"python3 long_obscure_script.py\"",
			"toolSummary": "Test live hot reload",
		},
	}
	if label := formatToolLabel("run_command", infoSummary); label != "Test live hot reload" {
		t.Errorf("expected 'Test live hot reload', got %q", label)
	}

	// Case 2: Instabox exec command fallback
	infoInstabox := &StepToolInfo{
		Name: "run_command",
		Parameters: map[string]interface{}{
			"CommandLine": "instabox exec test-android -- \"adb logcat -d\"",
		},
	}
	if label := formatToolLabel("run_command", infoInstabox); label != "test-android: adb logcat -d" {
		t.Errorf("expected 'test-android: adb logcat -d', got %q", label)
	}

	// Case 3: Truncate long commands without summary
	infoLongCmd := &StepToolInfo{
		Name: "run_command",
		Parameters: map[string]interface{}{
			"CommandLine": "find /some/very/long/nested/path/to/files/somewhere -name '*.png' -exec cp {} /dest/ \\;",
		},
	}
	if label := formatToolLabel("run_command", infoLongCmd); len(label) == 0 || !labelHasPrefix(label, "run: ") {
		t.Errorf("expected truncated run prefix, got %q", label)
	}

	// Case 4: File tools
	infoFile := &StepToolInfo{
		Name: "read_file",
		Parameters: map[string]interface{}{
			"Path": "/home/admin/workspace/lib/main.dart",
		},
	}
	if label := formatToolLabel("read_file", infoFile); label != "read main.dart" {
		t.Errorf("expected 'read main.dart', got %q", label)
	}

	// Case 5: Task management
	infoTask := &StepToolInfo{
		Name: "manage_task",
		Parameters: map[string]interface{}{
			"Action": "status",
		},
	}
	if label := formatToolLabel("manage_task", infoTask); label != "task: status" {
		t.Errorf("expected 'task: status', got %q", label)
	}
}

func labelHasPrefix(s, p string) bool {
	return len(s) >= len(p) && s[:len(p)] == p
}
