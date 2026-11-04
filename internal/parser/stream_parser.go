package parser

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

type EventType string

const (
	EventAssistantChunk EventType = "assistant_chunk"
	EventThinkingChunk  EventType = "thinking_chunk"
	EventToolStart      EventType = "tool_start"
	EventToolEnd        EventType = "tool_end"
	EventApprovalReq    EventType = "approval_request"
	EventPromptReady    EventType = "prompt_ready"
	EventError          EventType = "error"
)

type ParsedEvent struct {
	Type       EventType
	ID         string
	Text       string
	Thinking   string
	Ok         bool
	DurationMs int64
	ExitCode   int
	StderrTail string
	Options    []string
}

type StreamParser struct {
	mu           sync.Mutex
	buffer       string
	inThinking   bool
	thinkingBuf  strings.Builder
	assistantBuf strings.Builder

	// Approval detection patterns
	approvalRegexes []*regexp.Regexp
	promptRegexes   []*regexp.Regexp
}

func NewStreamParser() *StreamParser {
	return &StreamParser{
		approvalRegexes: []*regexp.Regexp{
			regexp.MustCompile(`(?i)Allow:\s*(.+?)\s*\?\s*Reply 1 = yes, 2 = no`),
			regexp.MustCompile(`(?i)Allow:\s*(.+?)\s*\?\s*\[y/N\]`),
			regexp.MustCompile(`(?i)Execute\s+(.+?)\s*\?\s*\[y/N\]`),
			regexp.MustCompile(`(?i)Requesting your permission to execute (?:shell command|JavaScript|action):\s*(.+)`),
			regexp.MustCompile(`(?i)Do you want to run\s*(.+?)\s*\?\s*\(yes/no\)`),
		},
		promptRegexes: []*regexp.Regexp{
			regexp.MustCompile(`(?m)^>\s*$`),
			regexp.MustCompile(`(?m)What would you like to do\?`),
			regexp.MustCompile(`(?m)Ready for next command`),
		},
	}
}

// FeedPTYChunk processes incoming raw PTY bytes and returns parsed events.
func (p *StreamParser) FeedPTYChunk(raw string) []ParsedEvent {
	p.mu.Lock()
	defer p.mu.Unlock()

	clean := StripVT(raw)
	p.buffer += clean

	var events []ParsedEvent

	lines := strings.Split(p.buffer, "\n")
	// Keep the last incomplete line in buffer
	if len(lines) > 0 {
		p.buffer = lines[len(lines)-1]
		lines = lines[:len(lines)-1]
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		// Check for approval prompt
		matchedApproval := false
		for _, re := range p.approvalRegexes {
			if match := re.FindStringSubmatch(trimmed); len(match) > 1 {
				cmd := strings.TrimSpace(match[1])
				events = append(events, ParsedEvent{
					Type:    EventApprovalReq,
					ID:      fmt.Sprintf("app_%d", time.Now().UnixNano()),
					Text:    cmd,
					Options: []string{"Allow", "Deny"},
				})
				matchedApproval = true
				break
			}
		}
		if matchedApproval {
			continue
		}

		// Check for thinking block transitions
		if strings.Contains(trimmed, "<thinking>") {
			p.inThinking = true
			continue
		}
		if strings.Contains(trimmed, "</thinking>") {
			p.inThinking = false
			continue
		}

		if p.inThinking {
			p.thinkingBuf.WriteString(line + "\n")
			events = append(events, ParsedEvent{
				Type: EventThinkingChunk,
				Text: line + "\n",
			})
			continue
		}

		// Check for tool execution markers
		if strings.HasPrefix(trimmed, "▶ ") || strings.HasPrefix(trimmed, "Running: ") {
			cmdLabel := strings.TrimPrefix(trimmed, "▶ ")
			cmdLabel = strings.TrimPrefix(cmdLabel, "Running: ")
			events = append(events, ParsedEvent{
				Type: EventToolStart,
				ID:   fmt.Sprintf("tool_%d", time.Now().UnixNano()),
				Text: cmdLabel,
			})
			continue
		}

		if strings.HasPrefix(trimmed, "✔ ") {
			label := strings.TrimPrefix(trimmed, "✔ ")
			events = append(events, ParsedEvent{
				Type: EventToolEnd,
				Text: label,
				Ok:   true,
			})
			continue
		}

		if strings.HasPrefix(trimmed, "✖ ") {
			label := strings.TrimPrefix(trimmed, "✖ ")
			events = append(events, ParsedEvent{
				Type:     EventToolEnd,
				Text:     label,
				Ok:       false,
				ExitCode: 1,
			})
			continue
		}

		// Check for prompt ready state
		matchedPrompt := false
		for _, re := range p.promptRegexes {
			if re.MatchString(trimmed) {
				events = append(events, ParsedEvent{
					Type: EventPromptReady,
				})
				matchedPrompt = true
				break
			}
		}
		if matchedPrompt {
			continue
		}

		// Default to assistant output line
		p.assistantBuf.WriteString(line + "\n")
		events = append(events, ParsedEvent{
			Type: EventAssistantChunk,
			Text: line + "\n",
		})
	}

	return events
}

// FeedNDJSONLine parses a single line of NDJSON from agy stream-json mode.
func (p *StreamParser) FeedNDJSONLine(line string) (*ParsedEvent, error) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return nil, nil
	}

	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return nil, err
	}

	eventKind, _ := raw["event"].(string)
	switch eventKind {
	case "step_update":
		stepUpdate, _ := raw["step_update"].(map[string]interface{})
		if stepUpdate == nil {
			return nil, nil
		}
		stepType, _ := stepUpdate["step_type"].(string)
		state, _ := stepUpdate["state"].(string)

		if stepType == "tool" {
			toolName, _ := stepUpdate["tool_name"].(string)
			toolInfo, _ := stepUpdate["tool_info"].(map[string]interface{})
			paramStr := ""
			if toolInfo != nil {
				params, _ := toolInfo["parameters"].(map[string]interface{})
				if params != nil {
					if cmd, ok := params["CommandLine"].(string); ok {
						paramStr = cmd
					}
				}
			}
			label := toolName
			if paramStr != "" {
				label = fmt.Sprintf("%s (%s)", toolName, paramStr)
			}

			if state == "ACTIVE" {
				return &ParsedEvent{
					Type: EventToolStart,
					ID:   fmt.Sprintf("tool_%v", stepUpdate["step_index"]),
					Text: label,
				}, nil
			} else if state == "DONE" {
				durSec, _ := stepUpdate["duration_seconds"].(float64)
				out, _ := toolInfo["output"].(string)
				ok := !strings.Contains(out, "exited with code 1")
				exitCode := 0
				if !ok {
					exitCode = 1
				}
				return &ParsedEvent{
					Type:       EventToolEnd,
					ID:         fmt.Sprintf("tool_%v", stepUpdate["step_index"]),
					Text:       label,
					Ok:         ok,
					DurationMs: int64(durSec * 1000),
					ExitCode:   exitCode,
					StderrTail: out,
				}, nil
			}
		} else if stepType == "agent_response" {
			delta, _ := stepUpdate["text_delta"].(string)
			if delta != "" {
				return &ParsedEvent{
					Type: EventAssistantChunk,
					Text: delta,
				}, nil
			}
		}

	case "control_request":
		prompt, _ := raw["prompt"].(string)
		id, _ := raw["id"].(string)
		if id == "" {
			id = fmt.Sprintf("app_%d", time.Now().UnixNano())
		}
		return &ParsedEvent{
			Type:    EventApprovalReq,
			ID:      id,
			Text:    prompt,
			Options: []string{"Allow", "Deny"},
		}, nil

	case "result":
		res, _ := raw["result"].(map[string]interface{})
		response, _ := res["response"].(string)
		return &ParsedEvent{
			Type: EventPromptReady,
			Text: response,
		}, nil
	}

	return nil, nil
}

func (p *StreamParser) GetThinkingText() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.thinkingBuf.String()
}

func (p *StreamParser) GetAssistantText() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.assistantBuf.String()
}

func (p *StreamParser) ResetTurn() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buffer = ""
	p.inThinking = false
	p.thinkingBuf.Reset()
	p.assistantBuf.Reset()
}
