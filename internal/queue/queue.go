package queue

import (
	"fmt"
	"sync"

	"agent-bridge/internal/db"
	"agent-bridge/internal/runner"
)

type Manager struct {
	db        *db.DB
	runner    runner.Runner
	sessionID string
	mu        sync.Mutex
}

func NewManager(database *db.DB, r runner.Runner, sessionID string) *Manager {
	return &Manager{
		db:        database,
		runner:    r,
		sessionID: sessionID,
	}
}

func (m *Manager) SetSessionID(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessionID = sessionID
}

// EnqueueResult holds the status of a submitted instruction.
type EnqueueResult struct {
	Queued     bool
	AheadCount int
	MessageID  int64
}

// Submit submits an instruction. If the runner is currently busy, it is enqueued.
// If idle, it is executed immediately.
func (m *Manager) Submit(userID int64, text, filesJSON string) (EnqueueResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.runner.IsBusy() {
		id, ahead, err := m.db.EnqueueMessage(m.sessionID, userID, text, filesJSON)
		if err != nil {
			return EnqueueResult{}, fmt.Errorf("queueing message: %w", err)
		}
		return EnqueueResult{
			Queued:     true,
			AheadCount: ahead,
			MessageID:  id,
		}, nil
	}

	// Runner is idle: execute directly
	if err := m.runner.WriteInstruction(text); err != nil {
		return EnqueueResult{}, fmt.Errorf("dispatching instruction: %w", err)
	}

	return EnqueueResult{
		Queued:     false,
		AheadCount: 0,
	}, nil
}

// DrainNext checks if there are pending messages in the queue and dispatches the next one.
func (m *Manager) DrainNext() (*db.QueueItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.runner.IsBusy() {
		return nil, nil // busy, cannot drain yet
	}

	item, err := m.db.GetNextPendingMessage(m.sessionID)
	if err != nil {
		return nil, nil // no pending messages
	}
	if item == nil {
		return nil, nil
	}

	if err := m.runner.WriteInstruction(item.Text); err != nil {
		_ = m.db.MarkMessageStatus(item.ID, "failed")
		return nil, fmt.Errorf("dispatching queued message %d: %w", item.ID, err)
	}

	_ = m.db.MarkMessageStatus(item.ID, "processing")
	return item, nil
}

func (m *Manager) PendingCount() (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.db.GetPendingQueueCount(m.sessionID)
}

func (m *Manager) Clear() (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.db.ClearQueue(m.sessionID)
}
