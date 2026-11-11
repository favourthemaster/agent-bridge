package db

import (
	"database/sql"
	"fmt"
	"time"
)

type ChatMessageRecord struct {
	ID              int64  `json:"id"`
	SessionID       string `json:"session_id"`
	ProjectName     string `json:"project_name"`
	Role            string `json:"role"` // user, assistant, system
	Content         string `json:"content"`
	Thinking        string `json:"thinking,omitempty"`
	StepsJSON       string `json:"steps_json,omitempty"`
	AttachmentsJSON string `json:"attachments_json,omitempty"`
	Model           string `json:"model,omitempty"`
	DurationMs      int64  `json:"duration_ms,omitempty"`
	CreatedAt       int64  `json:"created_at"`
}

type ScheduledTaskRecord struct {
	ID            string `json:"id"`
	ProjectName   string `json:"project_name"`
	Title         string `json:"title"`
	Prompt        string `json:"prompt"`
	ScheduleLabel string `json:"schedule_label"`
	IntervalMins  int    `json:"interval_mins"`
	Enabled       bool   `json:"enabled"`
	LastRunAt     int64  `json:"last_run_at"`
	CreatedAt     int64  `json:"created_at"`
}

type InboxEventRecord struct {
	ID          int64  `json:"id"`
	SessionID   string `json:"session_id"`
	ProjectName string `json:"project_name"`
	Kind        string `json:"kind"` // approval, turn_complete, artifact, error, scheduled
	Title       string `json:"title"`
	Body        string `json:"body"`
	PayloadJSON string `json:"payload_json,omitempty"`
	IsRead      bool   `json:"is_read"`
	CreatedAt   int64  `json:"created_at"`
}

func (d *DB) MigrateMobileTables() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	schema := `
	CREATE TABLE IF NOT EXISTS chat_messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT NOT NULL,
		project_name TEXT NOT NULL,
		role TEXT NOT NULL,
		content TEXT NOT NULL,
		thinking TEXT,
		steps_json TEXT,
		attachments_json TEXT,
		model TEXT,
		duration_ms INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_chat_messages_session ON chat_messages(session_id, id);

	CREATE TABLE IF NOT EXISTS scheduled_tasks (
		id TEXT PRIMARY KEY,
		project_name TEXT NOT NULL,
		title TEXT NOT NULL,
		prompt TEXT NOT NULL,
		schedule_label TEXT NOT NULL,
		interval_mins INTEGER NOT NULL DEFAULT 60,
		enabled INTEGER NOT NULL DEFAULT 1,
		last_run_at INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL
	);

	CREATE TABLE IF NOT EXISTS inbox_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT NOT NULL,
		project_name TEXT NOT NULL,
		kind TEXT NOT NULL,
		title TEXT NOT NULL,
		body TEXT NOT NULL,
		payload_json TEXT,
		is_read INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_inbox_events_id ON inbox_events(id);
	`
	_, err := d.db.Exec(schema)
	if err != nil {
		return fmt.Errorf("migrating mobile tables: %w", err)
	}
	return nil
}

func (d *DB) SaveChatMessage(msg ChatMessageRecord) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := msg.CreatedAt
	if now == 0 {
		now = time.Now().Unix()
	}
	res, err := d.db.Exec(`
		INSERT INTO chat_messages (session_id, project_name, role, content, thinking, steps_json, attachments_json, model, duration_ms, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, msg.SessionID, msg.ProjectName, msg.Role, msg.Content, msg.Thinking, msg.StepsJSON, msg.AttachmentsJSON, msg.Model, msg.DurationMs, now)
	if err != nil {
		return 0, err
	}
	_, _ = d.db.Exec("UPDATE sessions SET updated_at = ? WHERE id = ?", now, msg.SessionID)
	return res.LastInsertId()
}

func (d *DB) ListSessionMessages(sessionID string, limit int) ([]ChatMessageRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if limit <= 0 {
		limit = 200
	}
	rows, err := d.db.Query(`
		SELECT id, session_id, project_name, role, content, COALESCE(thinking, ''), COALESCE(steps_json, ''), COALESCE(attachments_json, ''), COALESCE(model, ''), duration_ms, created_at
		FROM chat_messages
		WHERE session_id = ?
		ORDER BY id ASC
		LIMIT ?
	`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ChatMessageRecord
	for rows.Next() {
		var m ChatMessageRecord
		if err := rows.Scan(&m.ID, &m.SessionID, &m.ProjectName, &m.Role, &m.Content, &m.Thinking, &m.StepsJSON, &m.AttachmentsJSON, &m.Model, &m.DurationMs, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

type SessionSummary struct {
	ID             string `json:"id"`
	ProjectName    string `json:"project_name"`
	ConversationID string `json:"conversation_id"`
	Title          string `json:"title"`
	Model          string `json:"model"`
	Verbosity      string `json:"verbosity"`
	Status         string `json:"status"`
	CreatedAt      int64  `json:"created_at"`
	UpdatedAt      int64  `json:"updated_at"`
}

func (d *DB) ListSessions(limit int) ([]SessionSummary, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if limit <= 0 {
		limit = 50
	}
	rows, err := d.db.Query(`
		SELECT s.id, s.project_name, COALESCE(s.conversation_id, ''), s.model, s.verbosity, s.status, s.created_at, s.updated_at,
		       COALESCE((SELECT content FROM chat_messages m WHERE m.session_id = s.id AND m.role = 'user' ORDER BY m.id ASC LIMIT 1), '')
		FROM sessions s
		ORDER BY s.updated_at DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []SessionSummary
	for rows.Next() {
		var s SessionSummary
		var firstPrompt string
		if err := rows.Scan(&s.ID, &s.ProjectName, &s.ConversationID, &s.Model, &s.Verbosity, &s.Status, &s.CreatedAt, &s.UpdatedAt, &firstPrompt); err != nil {
			return nil, err
		}
		title := firstPrompt
		if title == "" {
			title = fmt.Sprintf("%s session", s.ProjectName)
		} else if len(title) > 54 {
			title = title[:54] + "..."
		}
		s.Title = title
		list = append(list, s)
	}
	return list, nil
}

func (d *DB) GetSessionByID(sessionID string) (*SessionRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	var s SessionRecord
	var convID sql.NullString
	err := d.db.QueryRow(`
		SELECT id, project_name, conversation_id, model, verbosity, status, created_at, updated_at
		FROM sessions WHERE id = ?
	`, sessionID).Scan(&s.ID, &s.ProjectName, &convID, &s.Model, &s.Verbosity, &s.Status, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if convID.Valid {
		s.ConversationID = convID.String
	}
	return &s, nil
}

func (d *DB) ActivateSession(sessionID, projectName string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now().Unix()
	_, _ = d.db.Exec("UPDATE sessions SET status = 'closed' WHERE project_name = ? AND id != ?", projectName, sessionID)
	_, err := d.db.Exec("UPDATE sessions SET status = 'active', updated_at = ? WHERE id = ?", now, sessionID)
	return err
}

func (d *DB) ListPendingQueueItems(sessionID string) ([]QueueItem, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	rows, err := d.db.Query(`
		SELECT id, session_id, user_id, text, COALESCE(files_json, ''), status, created_at
		FROM message_queue
		WHERE session_id = ? AND status = 'pending'
		ORDER BY priority DESC, id ASC
	`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []QueueItem
	for rows.Next() {
		var item QueueItem
		if err := rows.Scan(&item.ID, &item.SessionID, &item.UserID, &item.Text, &item.FilesJSON, &item.Status, &item.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func (d *DB) ListPendingApprovals() ([]ApprovalRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now().Unix()
	rows, err := d.db.Query(`
		SELECT id, session_id, prompt, status, created_at, expires_at
		FROM approvals
		WHERE status = 'pending' AND expires_at > ?
		ORDER BY created_at DESC
	`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ApprovalRecord
	for rows.Next() {
		var a ApprovalRecord
		if err := rows.Scan(&a.ID, &a.SessionID, &a.Prompt, &a.Status, &a.CreatedAt, &a.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

func (d *DB) CreateScheduledTask(t ScheduledTaskRecord) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now().Unix()
	enabledInt := 0
	if t.Enabled {
		enabledInt = 1
	}
	if t.IntervalMins <= 0 {
		t.IntervalMins = 60
	}
	_, err := d.db.Exec(`
		INSERT INTO scheduled_tasks (id, project_name, title, prompt, schedule_label, interval_mins, enabled, last_run_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, t.ID, t.ProjectName, t.Title, t.Prompt, t.ScheduleLabel, t.IntervalMins, enabledInt, t.LastRunAt, now)
	return err
}

func (d *DB) ToggleScheduledTask(id string, enabled bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	val := 0
	if enabled {
		val = 1
	}
	_, err := d.db.Exec("UPDATE scheduled_tasks SET enabled = ? WHERE id = ?", val, id)
	return err
}

func (d *DB) DeleteScheduledTask(id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec("DELETE FROM scheduled_tasks WHERE id = ?", id)
	return err
}

func (d *DB) MarkScheduledTaskRun(id string, ts int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec("UPDATE scheduled_tasks SET last_run_at = ? WHERE id = ?", ts, id)
	return err
}

func (d *DB) ListScheduledTasks() ([]ScheduledTaskRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	rows, err := d.db.Query(`
		SELECT id, project_name, title, prompt, schedule_label, interval_mins, enabled, last_run_at, created_at
		FROM scheduled_tasks
		ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ScheduledTaskRecord
	for rows.Next() {
		var t ScheduledTaskRecord
		var en int
		if err := rows.Scan(&t.ID, &t.ProjectName, &t.Title, &t.Prompt, &t.ScheduleLabel, &t.IntervalMins, &en, &t.LastRunAt, &t.CreatedAt); err != nil {
			return nil, err
		}
		t.Enabled = en != 0
		out = append(out, t)
	}
	return out, nil
}

func (d *DB) CreateInboxEvent(ev InboxEventRecord) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := ev.CreatedAt
	if now == 0 {
		now = time.Now().Unix()
	}
	res, err := d.db.Exec(`
		INSERT INTO inbox_events (session_id, project_name, kind, title, body, payload_json, is_read, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 0, ?)
	`, ev.SessionID, ev.ProjectName, ev.Kind, ev.Title, ev.Body, ev.PayloadJSON, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (d *DB) ListInboxEventsSince(sinceID int64, limit int) ([]InboxEventRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if limit <= 0 {
		limit = 50
	}
	var rows *sql.Rows
	var err error
	if sinceID > 0 {
		rows, err = d.db.Query(`
			SELECT id, session_id, project_name, kind, title, body, COALESCE(payload_json, ''), is_read, created_at
			FROM inbox_events
			WHERE id > ?
			ORDER BY id ASC
			LIMIT ?
		`, sinceID, limit)
	} else {
		rows, err = d.db.Query(`
			SELECT id, session_id, project_name, kind, title, body, COALESCE(payload_json, ''), is_read, created_at
			FROM inbox_events
			ORDER BY id DESC
			LIMIT ?
		`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []InboxEventRecord
	for rows.Next() {
		var e InboxEventRecord
		var isRead int
		if err := rows.Scan(&e.ID, &e.SessionID, &e.ProjectName, &e.Kind, &e.Title, &e.Body, &e.PayloadJSON, &isRead, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.IsRead = isRead != 0
		list = append(list, e)
	}
	return list, nil
}

func (d *DB) MarkInboxRead(upToID int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if upToID > 0 {
		_, err := d.db.Exec("UPDATE inbox_events SET is_read = 1 WHERE id <= ?", upToID)
		return err
	}
	_, err := d.db.Exec("UPDATE inbox_events SET is_read = 1")
	return err
}

func (d *DB) UnreadInboxCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	var c int
	_ = d.db.QueryRow("SELECT COUNT(*) FROM inbox_events WHERE is_read = 0").Scan(&c)
	return c
}
