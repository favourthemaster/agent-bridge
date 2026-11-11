package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type DB struct {
	db *sql.DB
	mu sync.Mutex
}

type SessionRecord struct {
	ID             string
	ProjectName    string
	ConversationID string
	Model          string
	Verbosity      string
	Status         string
	CreatedAt      int64
	UpdatedAt      int64
}

type QueueItem struct {
	ID        int64
	SessionID string
	UserID    int64
	Text      string
	FilesJSON string
	Status    string
	CreatedAt int64
}

type ApprovalRecord struct {
	ID        string
	SessionID string
	Prompt    string
	Status    string
	CreatedAt int64
	ExpiresAt int64
}

// Open initializes the SQLite database with WAL mode and tables.
func Open(path string) (*DB, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("creating db directory: %w", err)
	}

	sqlDB, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("opening sqlite: %w", err)
	}

	d := &DB{db: sqlDB}
	if err := d.migrate(); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	if err := d.MigrateMobileTables(); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}

	return d, nil
}

func (d *DB) Close() error {
	return d.db.Close()
}

func (d *DB) migrate() error {
	schema := `
	CREATE TABLE IF NOT EXISTS sessions (
		id TEXT PRIMARY KEY,
		project_name TEXT NOT NULL,
		conversation_id TEXT,
		model TEXT NOT NULL,
		verbosity TEXT NOT NULL DEFAULT 'normal',
		status TEXT NOT NULL DEFAULT 'active',
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	);

	CREATE TABLE IF NOT EXISTS message_queue (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT NOT NULL,
		user_id INTEGER NOT NULL,
		text TEXT NOT NULL,
		files_json TEXT,
		status TEXT NOT NULL DEFAULT 'pending',
		priority INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL
	);

	CREATE TABLE IF NOT EXISTS approvals (
		id TEXT PRIMARY KEY,
		session_id TEXT NOT NULL,
		prompt TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'pending',
		created_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL
	);

	CREATE TABLE IF NOT EXISTS telegram_updates (
		update_id INTEGER PRIMARY KEY,
		handled_at INTEGER NOT NULL
	);

	CREATE TABLE IF NOT EXISTS audit_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp INTEGER NOT NULL,
		action TEXT NOT NULL,
		details TEXT NOT NULL
	);

	CREATE INDEX IF NOT EXISTS idx_queue_pending ON message_queue(session_id, status, id);
	`
	_, err := d.db.Exec(schema)
	if err != nil {
		return fmt.Errorf("running migrations: %w", err)
	}

	// Ensure priority column exists on existing installations
	_, _ = d.db.Exec("ALTER TABLE message_queue ADD COLUMN priority INTEGER NOT NULL DEFAULT 0")

	return nil
}

func (d *DB) GetLastHandledUpdateID() (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	var lastID sql.NullInt64
	err := d.db.QueryRow("SELECT MAX(update_id) FROM telegram_updates").Scan(&lastID)
	if err != nil {
		return 0, err
	}
	if !lastID.Valid {
		return 0, nil
	}
	return lastID.Int64, nil
}

func (d *DB) SetHandledUpdateID(updateID int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now().Unix()
	_, err := d.db.Exec("INSERT OR REPLACE INTO telegram_updates (update_id, handled_at) VALUES (?, ?)", updateID, now)
	return err
}

func (d *DB) GetOrCreateSession(projectName, defaultModel, defaultVerbosity string) (*SessionRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	var s SessionRecord
	var convID sql.NullString
	err := d.db.QueryRow(`
		SELECT id, project_name, conversation_id, model, verbosity, status, created_at, updated_at
		FROM sessions
		WHERE project_name = ? AND status = 'active'
		ORDER BY updated_at DESC LIMIT 1
	`, projectName).Scan(&s.ID, &s.ProjectName, &convID, &s.Model, &s.Verbosity, &s.Status, &s.CreatedAt, &s.UpdatedAt)

	if err == nil {
		if convID.Valid {
			s.ConversationID = convID.String
		}
		return &s, nil
	} else if err != sql.ErrNoRows {
		return nil, err
	}

	// Create new session
	now := time.Now().Unix()
	sessionID := fmt.Sprintf("sess_%d_%s", now, projectName)
	_, err = d.db.Exec(`
		INSERT INTO sessions (id, project_name, model, verbosity, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'active', ?, ?)
	`, sessionID, projectName, defaultModel, defaultVerbosity, now, now)
	if err != nil {
		return nil, err
	}

	return &SessionRecord{
		ID:          sessionID,
		ProjectName: projectName,
		Model:       defaultModel,
		Verbosity:   defaultVerbosity,
		Status:      "active",
		CreatedAt:   now,
		UpdatedAt:   now,
	}, nil
}

func (d *DB) ResetSession(projectName, defaultModel, defaultVerbosity string) (*SessionRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now().Unix()
	// mark existing active sessions as closed
	_, _ = d.db.Exec("UPDATE sessions SET status = 'closed', updated_at = ? WHERE project_name = ? AND status = 'active'", now, projectName)

	sessionID := fmt.Sprintf("sess_%d_%s", now, projectName)
	_, err := d.db.Exec(`
		INSERT INTO sessions (id, project_name, model, verbosity, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'active', ?, ?)
	`, sessionID, projectName, defaultModel, defaultVerbosity, now, now)
	if err != nil {
		return nil, err
	}

	return &SessionRecord{
		ID:          sessionID,
		ProjectName: projectName,
		Model:       defaultModel,
		Verbosity:   defaultVerbosity,
		Status:      "active",
		CreatedAt:   now,
		UpdatedAt:   now,
	}, nil
}

func (d *DB) UpdateSessionModel(sessionID, model string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now().Unix()
	_, err := d.db.Exec("UPDATE sessions SET model = ?, updated_at = ? WHERE id = ?", model, now, sessionID)
	return err
}

func (d *DB) UpdateSessionVerbosity(sessionID, verbosity string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now().Unix()
	_, err := d.db.Exec("UPDATE sessions SET verbosity = ?, updated_at = ? WHERE id = ?", verbosity, now, sessionID)
	return err
}

func (d *DB) UpdateSessionConversationID(sessionID, conversationID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now().Unix()
	_, err := d.db.Exec("UPDATE sessions SET conversation_id = ?, updated_at = ? WHERE id = ?", conversationID, now, sessionID)
	return err
}

func (d *DB) EnqueueMessage(sessionID string, userID int64, text, filesJSON string) (int64, int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now().Unix()
	res, err := d.db.Exec(`
		INSERT INTO message_queue (session_id, user_id, text, files_json, status, created_at)
		VALUES (?, ?, ?, ?, 'pending', ?)
	`, sessionID, userID, text, filesJSON, now)
	if err != nil {
		return 0, 0, err
	}

	id, err := res.LastInsertId()
	if err != nil {
		return 0, 0, err
	}

	// Count messages ahead
	var count int
	_ = d.db.QueryRow("SELECT COUNT(*) FROM message_queue WHERE session_id = ? AND status = 'pending' AND id < ?", sessionID, id).Scan(&count)

	return id, count, nil
}

func (d *DB) GetNextPendingMessage(sessionID string) (*QueueItem, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	var item QueueItem
	var files sql.NullString
	err := d.db.QueryRow(`
		SELECT id, session_id, user_id, text, files_json, status, created_at
		FROM message_queue
		WHERE session_id = ? AND status = 'pending'
		ORDER BY priority DESC, id ASC LIMIT 1
	`, sessionID).Scan(&item.ID, &item.SessionID, &item.UserID, &item.Text, &files, &item.Status, &item.CreatedAt)

	if err != nil {
		return nil, err
	}
	if files.Valid {
		item.FilesJSON = files.String
	}
	return &item, nil
}

func (d *DB) PrioritizeMessage(id int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec("UPDATE message_queue SET priority = 100 WHERE id = ?", id)
	return err
}

func (d *DB) CancelMessage(id int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec("UPDATE message_queue SET status = 'cancelled' WHERE id = ?", id)
	return err
}

func (d *DB) GetMessageByID(id int64) (*QueueItem, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	var item QueueItem
	var files sql.NullString
	err := d.db.QueryRow(`
		SELECT id, session_id, user_id, text, files_json, status, created_at
		FROM message_queue
		WHERE id = ?
	`, id).Scan(&item.ID, &item.SessionID, &item.UserID, &item.Text, &files, &item.Status, &item.CreatedAt)
	if err != nil {
		return nil, err
	}
	if files.Valid {
		item.FilesJSON = files.String
	}
	return &item, nil
}

func (d *DB) GetPendingQueueCount(sessionID string) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	var count int
	err := d.db.QueryRow("SELECT COUNT(*) FROM message_queue WHERE session_id = ? AND status = 'pending'", sessionID).Scan(&count)
	return count, err
}

func (d *DB) ClearQueue(sessionID string) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	res, err := d.db.Exec("UPDATE message_queue SET status = 'cancelled' WHERE session_id = ? AND status = 'pending'", sessionID)
	if err != nil {
		return 0, err
	}
	rows, err := res.RowsAffected()
	return int(rows), err
}

func (d *DB) MarkMessageStatus(id int64, status string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec("UPDATE message_queue SET status = ? WHERE id = ?", status, id)
	return err
}

func (d *DB) CreateApproval(id, sessionID, prompt string, expiresAt int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now().Unix()
	_, err := d.db.Exec(`
		INSERT INTO approvals (id, session_id, prompt, status, created_at, expires_at)
		VALUES (?, ?, ?, 'pending', ?, ?)
	`, id, sessionID, prompt, now, expiresAt)
	return err
}

func (d *DB) GetApproval(id string) (*ApprovalRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	var a ApprovalRecord
	err := d.db.QueryRow("SELECT id, session_id, prompt, status, created_at, expires_at FROM approvals WHERE id = ?", id).
		Scan(&a.ID, &a.SessionID, &a.Prompt, &a.Status, &a.CreatedAt, &a.ExpiresAt)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (d *DB) ResolveApproval(id, status string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec("UPDATE approvals SET status = ? WHERE id = ?", status, id)
	return err
}

func (d *DB) LogAudit(action, details string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now().Unix()
	_, err := d.db.Exec("INSERT INTO audit_log (timestamp, action, details) VALUES (?, ?, ?)", now, action, details)
	return err
}
