package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"

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
		startTime := time.Now()
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

		log.Printf("[CLIRunner] Launching agy turn (cwd: %s, model: %s, conv: %s, prompt: %q)", cwd, model, convID, text)

		cmd := exec.Command("agy", args...)
		if cwd != "" {
			cmd.Dir = cwd
		}
		cmd.Env = os.Environ()

		var stderrBuf bytes.Buffer
		cmd.Stderr = &stderrBuf

		r.mu.Lock()
		r.cmd = cmd
		r.mu.Unlock()

		stdoutBytes, err := cmd.Output()

		r.mu.Lock()
		r.cmd = nil
		// Mark idle BEFORE notifying listener so queue drainer can dispatch next item
		r.isBusy = false
		r.mu.Unlock()

		duration := time.Since(startTime)

		if err != nil {
			stderrStr := stderrBuf.String()
			errMsg := fmt.Sprintf("agy exited with error: %v\n%s", err, stderrStr)
			log.Printf("[CLIRunner] Turn failed (%v): %s", duration, errMsg)
			if cb != nil {
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

		// Parse JSON response
		var res AgyJSONResult
		jsonParsed := false

		// In case any preamble appears before JSON on stdout, find the outermost JSON object
		startIdx := bytes.IndexByte(stdoutBytes, '{')
		lastIdx := bytes.LastIndexByte(stdoutBytes, '}')
		if startIdx >= 0 && lastIdx > startIdx {
			cleanJSON := stdoutBytes[startIdx : lastIdx+1]
			if jsonErr := json.Unmarshal(cleanJSON, &res); jsonErr == nil && res.Response != "" {
				jsonParsed = true
				if res.ConversationID != "" {
					r.mu.Lock()
					r.convID = res.ConversationID
					r.mu.Unlock()
				}
				log.Printf("[CLIRunner] Turn completed in %v. Conv: %s, response length: %d chars", duration, res.ConversationID, len(res.Response))
				if cb != nil {
					cb(parser.ParsedEvent{
						Type: parser.EventPromptReady,
						Text: res.Response,
					})
				}
				return
			}
		}

		// Fallback: raw stdout if not clean JSON
		log.Printf("[CLIRunner] Turn completed in %v (non-JSON output, parsed=%v, raw len: %d)", duration, jsonParsed, len(stdoutBytes))
		if cb != nil {
			textOut := string(stdoutBytes)
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
