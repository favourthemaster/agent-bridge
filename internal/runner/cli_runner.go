package runner

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// StreamEvent models NDJSON lines emitted by agy --output-format stream-json
type StreamEvent struct {
	Event          string           `json:"event"`
	ConversationID string           `json:"conversation_id"`
	StepUpdate     *StepUpdateEvent `json:"step_update,omitempty"`
	Result         *ResultEvent     `json:"result,omitempty"`
}

type StepUpdateEvent struct {
	ConversationID string        `json:"conversation_id"`
	StepIndex      int           `json:"step_index"`
	State          string        `json:"state"`     // ACTIVE, DONE, ERROR
	StepType       string        `json:"step_type"` // user_input, tool, agent_response
	ToolName       string        `json:"tool_name,omitempty"`
	TextDelta      string        `json:"text_delta,omitempty"`
	DurationSec    float64       `json:"duration_seconds,omitempty"`
	ToolInfo       *StepToolInfo `json:"tool_info,omitempty"`
}

type StepToolInfo struct {
	Name       string                 `json:"name"`
	Parameters map[string]interface{} `json:"parameters,omitempty"`
	Output     string                 `json:"output,omitempty"`
}

type ResultEvent struct {
	ConversationID  string  `json:"conversation_id"`
	Status          string  `json:"status"`
	Response        string  `json:"response"`
	Error           string  `json:"error,omitempty"`
	DurationSeconds float64 `json:"duration_seconds"`
}

func formatToolLabel(name string, info *StepToolInfo) string {
	if info == nil || info.Parameters == nil {
		return name
	}
	switch name {
	case "run_command":
		if cmd, ok := info.Parameters["CommandLine"].(string); ok && cmd != "" {
			if len(cmd) > 35 {
				return fmt.Sprintf("run: %s...", cmd[:32])
			}
			return fmt.Sprintf("run: %s", cmd)
		}
	case "read_file":
		if p, ok := info.Parameters["Path"].(string); ok && p != "" {
			return fmt.Sprintf("read: %s", filepath.Base(p))
		}
	case "edit_file", "write_file":
		if p, ok := info.Parameters["Path"].(string); ok && p != "" {
			return fmt.Sprintf("edit: %s", filepath.Base(p))
		}
	}
	return name
}

func getTranscriptPath(convID string) string {
	if convID == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".gemini", "antigravity-cli", "brain", convID, ".system_generated", "logs", "transcript.jsonl")
}

func countTranscriptLines(path string) int {
	if path == "" {
		return 0
	}
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	count := 0
	for scanner.Scan() {
		count++
	}
	return count
}

func extractThinkingSince(convID string, startLine int) string {
	path := getTranscriptPath(convID)
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 2*1024*1024)

	currentLine := 0
	var thinkings []string

	type TranscriptStep struct {
		Type     string `json:"type"`
		Thinking string `json:"thinking"`
	}

	for scanner.Scan() {
		currentLine++
		if currentLine <= startLine {
			continue
		}
		var step TranscriptStep
		if err := json.Unmarshal(scanner.Bytes(), &step); err == nil {
			if step.Type == "PLANNER_RESPONSE" && strings.TrimSpace(step.Thinking) != "" {
				thinkings = append(thinkings, strings.TrimSpace(step.Thinking))
			}
		}
	}

	if len(thinkings) == 0 {
		return ""
	}
	return strings.Join(thinkings, "\n\n")
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
		startTranscriptLines := countTranscriptLines(getTranscriptPath(convID))
		args := []string{"--output-format", "stream-json", fmt.Sprintf("--print=%s", text)}
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

		stdoutPipe, err := cmd.StdoutPipe()
		if err != nil {
			r.mu.Lock()
			r.isBusy = false
			r.mu.Unlock()
			log.Printf("[CLIRunner] Error creating stdout pipe: %v", err)
			if cb != nil {
				cb(parser.ParsedEvent{Type: parser.EventError, Text: err.Error()})
			}
			return
		}

		var stderrBuf bytes.Buffer
		cmd.Stderr = &stderrBuf

		r.mu.Lock()
		r.cmd = cmd
		r.mu.Unlock()

		if err := cmd.Start(); err != nil {
			r.mu.Lock()
			r.cmd = nil
			r.isBusy = false
			r.mu.Unlock()
			log.Printf("[CLIRunner] Error starting agy: %v", err)
			if cb != nil {
				cb(parser.ParsedEvent{Type: parser.EventError, Text: err.Error()})
			}
			return
		}

		reader := bufio.NewReader(stdoutPipe)
		var accumulatedText strings.Builder
		var finalResponse string
		var conversationID string
		hasResult := false

		for {
			line, readErr := reader.ReadBytes('\n')
			if len(line) > 0 {
				lineTrimmed := bytes.TrimSpace(line)
				if len(lineTrimmed) > 0 && lineTrimmed[0] == '{' {
					var ev StreamEvent
					if jsonErr := json.Unmarshal(lineTrimmed, &ev); jsonErr == nil {
						if ev.Event == "init" && ev.ConversationID != "" {
							conversationID = ev.ConversationID
							r.mu.Lock()
							r.convID = ev.ConversationID
							r.mu.Unlock()
						} else if ev.Event == "step_update" && ev.StepUpdate != nil {
							su := ev.StepUpdate
							if su.ConversationID != "" && conversationID == "" {
								conversationID = su.ConversationID
								r.mu.Lock()
								r.convID = su.ConversationID
								r.mu.Unlock()
							}
							switch su.StepType {
							case "tool":
								stepID := fmt.Sprintf("step-%d", su.StepIndex)
								label := formatToolLabel(su.ToolName, su.ToolInfo)
								if su.State == "ACTIVE" {
									if cb != nil {
										cb(parser.ParsedEvent{
											Type: parser.EventToolStart,
											ID:   stepID,
											Text: label,
										})
									}
								} else if su.State == "DONE" || su.State == "ERROR" {
									if cb != nil {
										cb(parser.ParsedEvent{
											Type:       parser.EventToolEnd,
											ID:         stepID,
											Ok:         su.State == "DONE",
											DurationMs: int64(su.DurationSec * 1000),
											Text:       label,
										})
									}
								}
							case "agent_response":
								if su.TextDelta != "" {
									accumulatedText.WriteString(su.TextDelta)
									if cb != nil {
										cb(parser.ParsedEvent{
											Type: parser.EventAssistantChunk,
											Text: su.TextDelta,
										})
									}
								}
							}
						} else if ev.Event == "result" && ev.Result != nil {
							hasResult = true
							finalResponse = ev.Result.Response
							if ev.Result.ConversationID != "" {
								conversationID = ev.Result.ConversationID
								r.mu.Lock()
								r.convID = ev.Result.ConversationID
								r.mu.Unlock()
							}
							if ev.Result.Status != "SUCCESS" && ev.Result.Error != "" {
								finalResponse = fmt.Sprintf("Error: %s", ev.Result.Error)
							}
						}
					}
				}
			}
			if readErr != nil {
				break
			}
		}

		waitErr := cmd.Wait()
		duration := time.Since(startTime)

		r.mu.Lock()
		r.cmd = nil
		// Mark idle BEFORE notifying listener so queue drainer can dispatch next item
		r.isBusy = false
		r.mu.Unlock()

		if waitErr != nil && !hasResult {
			stderrStr := stderrBuf.String()
			errMsg := fmt.Sprintf("agy exited with error: %v\n%s", waitErr, stderrStr)
			log.Printf("[CLIRunner] Turn failed (%v): %s", duration, errMsg)
			if cb != nil {
				cb(parser.ParsedEvent{
					Type: parser.EventError,
					Text: errMsg,
				})
			}
			return
		}

		if finalResponse == "" {
			finalResponse = accumulatedText.String()
		}
		if finalResponse == "" {
			finalResponse = "Done."
		}

		activeConv := conversationID
		if activeConv == "" {
			activeConv = convID
		}
		thinkingTrace := extractThinkingSince(activeConv, startTranscriptLines)

		log.Printf("[CLIRunner] Turn completed in %v. Conv: %s, response length: %d chars, thinking length: %d chars", duration, conversationID, len(finalResponse), len(thinkingTrace))
		if cb != nil {
			cb(parser.ParsedEvent{
				Type:     parser.EventPromptReady,
				Text:     finalResponse,
				Thinking: thinkingTrace,
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
