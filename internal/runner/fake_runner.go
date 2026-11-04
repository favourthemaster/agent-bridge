package runner

import (
	"sync"
	"time"

	"agent-bridge/internal/parser"
)

type ScriptedAction func(r *FakeRunner)

type FakeRunner struct {
	mu           sync.Mutex
	isBusy       bool
	autoApprove  bool
	eventCb      func(e parser.ParsedEvent)
	Instructions []string
	Approvals    []bool
	interrupted  bool

	// Scripted response callbacks
	OnInstruction func(text string, r *FakeRunner)
}

func NewFakeRunner() *FakeRunner {
	return &FakeRunner{
		Instructions: make([]string, 0),
		Approvals:    make([]bool, 0),
	}
}

func (f *FakeRunner) Start(cwd, model, conversationID string) error {
	return nil
}

func (f *FakeRunner) OnEvent(cb func(e parser.ParsedEvent)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.eventCb = cb
}

func (f *FakeRunner) IsBusy() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.isBusy
}

func (f *FakeRunner) SetAutoApprove(enabled bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.autoApprove = enabled
}

func (f *FakeRunner) IsAutoApprove() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.autoApprove
}

func (f *FakeRunner) SetBusy(b bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.isBusy = b
}

func (f *FakeRunner) EmitEvent(ev parser.ParsedEvent) {
	f.mu.Lock()
	cb := f.eventCb
	f.mu.Unlock()

	if cb != nil {
		cb(ev)
	}
}

func (f *FakeRunner) WriteInstruction(text string) error {
	f.mu.Lock()
	f.Instructions = append(f.Instructions, text)
	f.isBusy = true
	handler := f.OnInstruction
	f.mu.Unlock()

	if handler != nil {
		go handler(text, f)
	}
	return nil
}

func (f *FakeRunner) SendApprovalResponse(allowed bool) error {
	f.mu.Lock()
	f.Approvals = append(f.Approvals, allowed)
	f.mu.Unlock()
	return nil
}

func (f *FakeRunner) Interrupt() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.interrupted = true
	f.isBusy = false
	return nil
}

func (f *FakeRunner) Stop() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.isBusy = false
	return nil
}

func (f *FakeRunner) EmitTurnSuccess(thinking, answer string) {
	if thinking != "" {
		f.EmitEvent(parser.ParsedEvent{
			Type: parser.EventThinkingChunk,
			Text: thinking,
		})
	}
	f.EmitEvent(parser.ParsedEvent{
		Type: parser.EventAssistantChunk,
		Text: answer,
	})
	f.EmitEvent(parser.ParsedEvent{
		Type: parser.EventPromptReady,
	})
	f.SetBusy(false)
}

func (f *FakeRunner) SimulateFlood(count int, interval time.Duration) {
	for i := 0; i < count; i++ {
		f.EmitEvent(parser.ParsedEvent{
			Type: parser.EventToolStart,
			ID:   string(rune('a' + (i % 26))),
			Text: "flooding step",
		})
		time.Sleep(interval)
	}
	f.EmitEvent(parser.ParsedEvent{
		Type: parser.EventPromptReady,
	})
	f.SetBusy(false)
}
