package telegram

import (
	"context"
	"path/filepath"
	"testing"

	"agent-bridge/internal/config"
	"agent-bridge/internal/db"
	"agent-bridge/internal/runner"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

func TestAuthMiddleware(t *testing.T) {
	allowedID := int64(123456789)
	middleware := AuthMiddleware(allowedID)

	var nextCalled bool
	handler := middleware(func(ctx context.Context, b *bot.Bot, update *models.Update) {
		nextCalled = true
	})

	// 1. Authorized user message
	nextCalled = false
	updateAuthorized := &models.Update{
		Message: &models.Message{
			From: &models.User{ID: allowedID},
		},
	}
	handler(context.Background(), nil, updateAuthorized)
	if !nextCalled {
		t.Errorf("expected authorized update to proceed to next handler")
	}

	// 2. Unauthorized user message
	nextCalled = false
	updateUnauthorized := &models.Update{
		Message: &models.Message{
			From: &models.User{ID: 999999999},
		},
	}
	handler(context.Background(), nil, updateUnauthorized)
	if nextCalled {
		t.Errorf("expected unauthorized update to be silently dropped")
	}

	// 3. Authorized callback query
	nextCalled = false
	updateCallback := &models.Update{
		CallbackQuery: &models.CallbackQuery{
			From: models.User{ID: allowedID},
		},
	}
	handler(context.Background(), nil, updateCallback)
	if !nextCalled {
		t.Errorf("expected authorized callback to proceed")
	}
}

func TestCommandHandler(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "cmd_test.db")

	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("failed opening db: %v", err)
	}
	defer database.Close()

	cfg := &config.Config{
		Telegram: config.TelegramConfig{AllowedUserID: 12345},
		Projects: map[string]config.ProjectConfig{
			"coachly": {Path: tempDir, DefaultModel: "gemini-3.8-flash-high"},
		},
		Models: []config.ModelConfig{
			{ID: "gemini-3.8-flash-high", DisplayName: "Gemini 3.8 Flash"},
			{ID: "claude-sonnet-4-6", DisplayName: "Claude Sonnet 4.6"},
		},
		Defaults: config.DefaultsConfig{Project: "coachly", Verbosity: "normal"},
	}

	fake := runner.NewFakeRunner()
	sess, _ := database.GetOrCreateSession("coachly", "gemini-3.8-flash-high", "normal")

	// Notice: b is nil in this test because we test command routing & state changes
	ch := NewCommandHandler(nil, cfg, database, fake, 12345, sess, "coachly")

	// Test verbosity change
	ch.cmdSetVerbosity(context.Background(), "quiet")
	if sess.Verbosity != "quiet" {
		t.Errorf("expected verbosity to be 'quiet', got %s", sess.Verbosity)
	}

	// Verify persistence in DB
	sessReloaded, _ := database.GetOrCreateSession("coachly", "gemini-3.8-flash-high", "normal")
	if sessReloaded.Verbosity != "quiet" {
		t.Errorf("expected persisted verbosity 'quiet', got %s", sessReloaded.Verbosity)
	}
}

func TestActionTickerLifecycle(t *testing.T) {
	ticker := &ActionTicker{chatID: 12345}
	// Verify Start and Stop do not panic or deadlock
	ticker.mu.Lock()
	ticker.running = true
	ticker.stopCh = make(chan struct{})
	ticker.mu.Unlock()

	ticker.Stop()
	if ticker.running {
		t.Errorf("expected ticker.running to be false after Stop()")
	}
}
