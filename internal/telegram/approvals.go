package telegram

import (
	"context"
	"fmt"
	"sync"
	"time"

	"agent-bridge/internal/db"
	"agent-bridge/internal/formatter"
	"agent-bridge/internal/runner"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

type ApprovalManager struct {
	b       *bot.Bot
	db      *db.DB
	runner  runner.Runner
	chatID  int64
	timers  map[string]*time.Timer
	msgIDs  map[string]int
	mu      sync.Mutex
}

func NewApprovalManager(b *bot.Bot, database *db.DB, r runner.Runner, chatID int64) *ApprovalManager {
	return &ApprovalManager{
		b:      b,
		db:     database,
		runner: r,
		chatID: chatID,
		timers: make(map[string]*time.Timer),
		msgIDs: make(map[string]int),
	}
}

// RequestApproval sends an approval request message with inline keyboard buttons.
func (am *ApprovalManager) RequestApproval(ctx context.Context, sessionID, eventID, prompt string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}

	expiresAt := time.Now().Add(timeout).Unix()
	if err := am.db.CreateApproval(eventID, sessionID, prompt, expiresAt); err != nil {
		return fmt.Errorf("storing approval in db: %w", err)
	}

	kb := &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{
			{
				{Text: "✔ Allow", CallbackData: fmt.Sprintf("approve:%s", eventID)},
				{Text: "✖ Deny", CallbackData: fmt.Sprintf("deny:%s", eventID)},
			},
		},
	}

	msgText := fmt.Sprintf("⚠️ <b>Permission Request</b>\n\n<code>%s</code>\n\n<i>Reply within %d minutes (default Deny).</i>",
		formatter.EscapeHTML(prompt),
		int(timeout.Minutes()),
	)

	sentMsg, err := am.b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      am.chatID,
		Text:        msgText,
		ParseMode:   models.ParseModeHTML,
		ReplyMarkup: kb,
	})
	if err != nil {
		return fmt.Errorf("sending approval message: %w", err)
	}

	am.mu.Lock()
	am.msgIDs[eventID] = sentMsg.ID
	am.timers[eventID] = time.AfterFunc(timeout, func() {
		am.handleTimeout(eventID, prompt, sentMsg.ID)
	})
	am.mu.Unlock()

	return nil
}

// HandleCallback handles clicks on [ Allow ] or [ Deny ] buttons.
func (am *ApprovalManager) HandleCallback(ctx context.Context, callbackID, data string, messageID int) error {
	// Answer callback query immediately to stop the button spinner
	_, _ = am.b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
		CallbackQueryID: callbackID,
	})

	var eventID string
	var allowed bool
	if len(data) > 8 && data[:8] == "approve:" {
		eventID = data[8:]
		allowed = true
	} else if len(data) > 5 && data[:5] == "deny:" {
		eventID = data[5:]
		allowed = false
	} else {
		return nil
	}

	am.mu.Lock()
	if t, exists := am.timers[eventID]; exists {
		t.Stop()
		delete(am.timers, eventID)
	}
	delete(am.msgIDs, eventID)
	am.mu.Unlock()

	app, err := am.db.GetApproval(eventID)
	if err != nil || app == nil || app.Status != "pending" {
		return nil // already resolved or expired
	}

	status := "denied"
	statusText := "✖ <b>Denied by you</b>"
	if allowed {
		status = "allowed"
		statusText = "✔ <b>Allowed by you</b>"
	}

	_ = am.db.ResolveApproval(eventID, status)
	_ = am.db.LogAudit("approval_resolved", fmt.Sprintf("id=%s status=%s prompt=%s", eventID, status, app.Prompt))

	// Edit Telegram message to reflect decision
	editText := fmt.Sprintf("⚠️ <b>Permission Request</b>\n\n<code>%s</code>\n\n%s",
		formatter.EscapeHTML(app.Prompt),
		statusText,
	)
	_, _ = am.b.EditMessageText(ctx, &bot.EditMessageTextParams{
		ChatID:    am.chatID,
		MessageID: messageID,
		Text:      editText,
		ParseMode: models.ParseModeHTML,
	})

	// Inject decision into runner PTY
	return am.runner.SendApprovalResponse(allowed)
}

func (am *ApprovalManager) handleTimeout(eventID, prompt string, messageID int) {
	am.mu.Lock()
	delete(am.timers, eventID)
	delete(am.msgIDs, eventID)
	am.mu.Unlock()

	app, err := am.db.GetApproval(eventID)
	if err != nil || app == nil || app.Status != "pending" {
		return
	}

	_ = am.db.ResolveApproval(eventID, "expired")
	_ = am.db.LogAudit("approval_expired", fmt.Sprintf("id=%s prompt=%s", eventID, prompt))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	editText := fmt.Sprintf("⚠️ <b>Permission Request</b>\n\n<code>%s</code>\n\n✖ <b>Denied (timed out after 10m)</b>",
		formatter.EscapeHTML(prompt),
	)
	_, _ = am.b.EditMessageText(ctx, &bot.EditMessageTextParams{
		ChatID:    am.chatID,
		MessageID: messageID,
		Text:      editText,
		ParseMode: models.ParseModeHTML,
	})

	_ = am.runner.SendApprovalResponse(false)
}
