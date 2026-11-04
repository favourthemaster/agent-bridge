package runner

import (
	"agent-bridge/internal/parser"
)

// Runner represents the interface to an agent execution session.
type Runner interface {
	Start(cwd, model, conversationID string) error
	WriteInstruction(text string) error
	SendApprovalResponse(allowed bool) error
	Interrupt() error
	Stop() error
	OnEvent(cb func(e parser.ParsedEvent))
	IsBusy() bool
}
