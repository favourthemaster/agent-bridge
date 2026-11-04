package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sync"

	"agent-bridge/internal/parser"
)

type CLIRunner struct {
	cmd         *exec.Cmd
	eventCb     func(e parser.ParsedEvent)
	isBusy      bool
	cwd         string
	model       string
	convID      string
	autoApprove bool
	mu          sync.Mutex
}

func NewCLIRunner() *CLIRunner {
	return &CLIRunner{}
}

func (r *CLIRunner) OnEvent(cb func(e parser.ParsedEvent)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.eventCb = cb
}

func (r *CLIRunner) IsBusy() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.isBusy
}

func (r *CLIRunner) SetAutoApprove(enabled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.autoApprove = enabled
}

func (r *CLIRunner) IsAutoApprove() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.autoApprove
}

func (r *CLIRunner) Start(cwd, model, conversationID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cwd = cwd
	r.model = model
	r.convID = conversationID
	r.isBusy = false
	return nil
}

type AgyJSONResult struct {
	ConversationID string `json:"conversation_id"`
	Status         string `json:"status"`
	Response       string `json:"response"`
	Error          string `json:"error"`
}

func (r *CLIRunner) WriteInstruction(text string) error {
	r.mu.Lock()
	if r.isBusy {
		r.mu.Unlock()
		return fmt.Errorf("runner is busy")
	}
	r.isBusy = true
	cwd := r.cwd
	model := r.model
	convID := r.convID
	autoApprove := r.autoApprove
	cb := r.eventCb
	r.mu.Unlock()

	go func() {
		defer func() {
			r.mu.Lock()
			r.isBusy = false
			r.mu.Unlock()
		}()

		args := []string{"--output-format", "json", fmt.Sprintf("--print=%s", text)}
		if model != "" {
			args = append(args, "--model", model)
		}
		if convID != "" {
			args = append(args, "--conversation", convID)
		}
		if autoApprove {
			args = append(args, "--dangerously-skip-permissions")
		}

		cmd := exec.Command("agy", args...)
		if cwd != "" {
			cmd.Dir = cwd
		}
		cmd.Env = os.Environ()

		r.mu.Lock()
		r.cmd = cmd
		r.mu.Unlock()

		out, err := cmd.CombinedOutput()

		r.mu.Lock()
		r.cmd = nil
		r.mu.Unlock()

		if err != nil {
			if cb != nil {
				errMsg := fmt.Sprintf("agy exited with error: %v\nOutput: %s", err, string(out))
				cb(parser.ParsedEvent{
					Type: parser.EventError,
					Text: errMsg,
				})
				cb(parser.ParsedEvent{
					Type: parser.EventPromptReady,
					Text: fmt.Sprintf("⚠️ %s", errMsg),
				})
			}
			return
		}

		var res AgyJSONResult
		if jsonErr := json.Unmarshal(out, &res); jsonErr == nil && res.Response != "" {
			if res.ConversationID != "" {
				r.mu.Lock()
				r.convID = res.ConversationID
				r.mu.Unlock()
			}
			if cb != nil {
				cb(parser.ParsedEvent{
					Type: parser.EventPromptReady,
					Text: res.Response,
				})
			}
			return
		}

		// Fallback: raw text output if not JSON
		if cb != nil {
			textOut := string(out)
			if textOut == "" {
				textOut = "Done."
			}
			cb(parser.ParsedEvent{
				Type: parser.EventPromptReady,
				Text: textOut,
			})
		}
	}()

	return nil
}

func (r *CLIRunner) SendApprovalResponse(allowed bool) error {
	return nil
}

func (r *CLIRunner) Interrupt() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cmd != nil && r.cmd.Process != nil {
		_ = r.cmd.Process.Kill()
	}
	r.isBusy = false
	return nil
}

func (r *CLIRunner) Stop() error {
	return r.Interrupt()
}
