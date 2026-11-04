package parser

import (
	"strings"
	"testing"
)

func TestStripVT(t *testing.T) {
	input := "\x1b[?1049h\x1b[2J\x1b[32m✔ read config/load.go\x1b[0m\r\nHello World\x1b[>4;2m"
	got := StripVT(input)
	expected := "✔ read config/load.go\nHello World"
	if strings.TrimSpace(got) != expected {
		t.Errorf("expected %q, got %q", expected, got)
	}
}

func TestStreamParserPTYApproval(t *testing.T) {
	p := NewStreamParser()

	raw := "\x1b[33mAllow: rm -rf build/ ? Reply 1 = yes, 2 = no\x1b[0m\n"
	events := p.FeedPTYChunk(raw)

	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.Type != EventApprovalReq {
		t.Errorf("expected EventApprovalReq, got %v", ev.Type)
	}
	if ev.Text != "rm -rf build/" {
		t.Errorf("expected 'rm -rf build/', got %q", ev.Text)
	}
}

func TestStreamParserPTYThinkingAndTools(t *testing.T) {
	p := NewStreamParser()

	chunk := `
<thinking>
Analyzing config loading in main.go
Need to verify thresholds
</thinking>
▶ go test ./...
✔ go test ./...
All tests pass successfully.
`
	events := p.FeedPTYChunk(chunk)

	var hasThinking, hasToolStart, hasToolEnd, hasAssistant bool
	for _, ev := range events {
		switch ev.Type {
		case EventThinkingChunk:
			hasThinking = true
		case EventToolStart:
			hasToolStart = true
		case EventToolEnd:
			hasToolEnd = true
		case EventAssistantChunk:
			hasAssistant = true
		}
	}

	if !hasThinking {
		t.Errorf("expected thinking event")
	}
	if !hasToolStart {
		t.Errorf("expected tool start event")
	}
	if !hasToolEnd {
		t.Errorf("expected tool end event")
	}
	if !hasAssistant {
		t.Errorf("expected assistant chunk event")
	}

	if !strings.Contains(p.GetThinkingText(), "Analyzing config loading") {
		t.Errorf("expected thinking buffer to have analysis, got: %s", p.GetThinkingText())
	}
}

func TestStreamParserNDJSON(t *testing.T) {
	p := NewStreamParser()

	lineToolStart := `{"event":"step_update","step_update":{"conversation_id":"c1","step_index":2,"state":"ACTIVE","step_type":"tool","tool_name":"run_command","tool_info":{"name":"run_command","parameters":{"CommandLine":"echo hello"}}}}`
	ev1, err := p.FeedNDJSONLine(lineToolStart)
	if err != nil || ev1 == nil || ev1.Type != EventToolStart {
		t.Fatalf("expected EventToolStart, got: %+v, err: %v", ev1, err)
	}

	lineToolEnd := `{"event":"step_update","step_update":{"conversation_id":"c1","step_index":2,"state":"DONE","step_type":"tool","duration_seconds":0.5,"tool_name":"run_command","tool_info":{"output":"hello\n"}}}`
	ev2, err := p.FeedNDJSONLine(lineToolEnd)
	if err != nil || ev2 == nil || ev2.Type != EventToolEnd || !ev2.Ok {
		t.Fatalf("expected EventToolEnd ok, got: %+v, err: %v", ev2, err)
	}

	lineApproval := `{"event":"control_request","id":"app_99","prompt":"Run git reset --hard?"}`
	ev3, err := p.FeedNDJSONLine(lineApproval)
	if err != nil || ev3 == nil || ev3.Type != EventApprovalReq || ev3.Text != "Run git reset --hard?" {
		t.Fatalf("expected EventApprovalReq, got: %+v, err: %v", ev3, err)
	}
}
