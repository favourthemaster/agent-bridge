package telegram

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

func TestExtractImages(t *testing.T) {
	tempDir := t.TempDir()
	img1Path := filepath.Join(tempDir, "screen1.png")
	img2Path := filepath.Join(tempDir, "screen2.png")

	if err := os.WriteFile(img1Path, []byte("fake png 1"), 0644); err != nil {
		t.Fatalf("failed creating test image 1: %v", err)
	}
	if err := os.WriteFile(img2Path, []byte("fake png 2"), 0644); err != nil {
		t.Fatalf("failed creating test image 2: %v", err)
	}

	rawText := "Here is the verification result:\n\n" +
		"````carousel\n" +
		"![Original App Launch](file://" + img1Path + ")\n" +
		"<!-- slide -->\n" +
		"![Hot Reload with (Live Test)](" + img2Path + ")\n" +
		"````\n\n" +
		"### What We Tested\n" +
		"All verification checks passed."

	images, cleaned := extractImages(rawText, tempDir)

	if len(images) != 2 {
		t.Fatalf("expected 2 extracted images, got %d", len(images))
	}

	if images[0].Caption != "Original App Launch" || images[0].Path != img1Path {
		t.Errorf("unexpected image 0: %+v", images[0])
	}
	if images[1].Caption != "Hot Reload with (Live Test)" || images[1].Path != img2Path {
		t.Errorf("unexpected image 1: %+v", images[1])
	}

	if strings.Contains(cleaned, "carousel") {
		t.Errorf("expected cleaned text not to contain 'carousel', got:\n%s", cleaned)
	}
	if strings.Contains(cleaned, "<!-- slide -->") {
		t.Errorf("expected cleaned text not to contain slide comment, got:\n%s", cleaned)
	}
	if !strings.Contains(cleaned, "Here is the verification result:") {
		t.Errorf("expected preamble text preserved, got:\n%s", cleaned)
	}
	if !strings.Contains(cleaned, "### What We Tested") {
		t.Errorf("expected trailing markdown preserved, got:\n%s", cleaned)
	}
}
