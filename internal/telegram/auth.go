package telegram

import (
	"context"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// AuthMiddleware creates a middleware that silently drops any update not originating
// from the configured numeric Telegram user ID.
func AuthMiddleware(allowedUserID int64) bot.Middleware {
	return func(next bot.HandlerFunc) bot.HandlerFunc {
		return func(ctx context.Context, b *bot.Bot, update *models.Update) {
			var senderID int64
			if update.Message != nil && update.Message.From != nil {
				senderID = update.Message.From.ID
			} else if update.CallbackQuery != nil && update.CallbackQuery.From.ID != 0 {
				senderID = update.CallbackQuery.From.ID
			}

			if senderID != allowedUserID {
				// Silently ignore unauthorized senders
				return
			}

			next(ctx, b, update)
		}
	}
}
