package runner

import (
	"sync"

	"agent-bridge/internal/parser"
)

// FanoutRunner wraps a single Runner and broadcasts OnEvent notifications
// to multiple independent subscribers (e.g. Telegram BridgeBot + MobileServer).
type FanoutRunner struct {
	inner     Runner
	mu        sync.RWMutex
	listeners []func(e parser.ParsedEvent)
}

func NewFanoutRunner(inner Runner) *FanoutRunner {
	fr := &FanoutRunner{
		inner:     inner,
		listeners: make([]func(e parser.ParsedEvent), 0, 4),
	}
	inner.OnEvent(fr.broadcast)
	return fr
}

func (f *FanoutRunner) broadcast(ev parser.ParsedEvent) {
	f.mu.RLock()
	subs := make([]func(e parser.ParsedEvent), len(f.listeners))
	copy(subs, f.listeners)
	f.mu.RUnlock()

	for _, cb := range subs {
		if cb != nil {
			cb(ev)
		}
	}
}

// OnEvent appends an event listener so multiple transports coexist safely.
func (f *FanoutRunner) OnEvent(cb func(e parser.ParsedEvent)) {
	if cb == nil {
		return
	}
	f.mu.Lock()
	f.listeners = append(f.listeners, cb)
	f.mu.Unlock()
}

func (f *FanoutRunner) Start(cwd, model, conversationID string) error {
	return f.inner.Start(cwd, model, conversationID)
}

func (f *FanoutRunner) WriteInstruction(text string) error {
	return f.inner.WriteInstruction(text)
}

func (f *FanoutRunner) SendApprovalResponse(allowed bool) error {
	return f.inner.SendApprovalResponse(allowed)
}

func (f *FanoutRunner) Interrupt() error {
	return f.inner.Interrupt()
}

func (f *FanoutRunner) Stop() error {
	return f.inner.Stop()
}

func (f *FanoutRunner) IsBusy() bool {
	return f.inner.IsBusy()
}

func (f *FanoutRunner) SetAutoApprove(enabled bool) {
	f.inner.SetAutoApprove(enabled)
}

func (f *FanoutRunner) IsAutoApprove() bool {
	return f.inner.IsAutoApprove()
}
