package telegram

import (
	"context"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

type ActionTicker struct {
	b       *bot.Bot
	chatID  int64
	ticker  *time.Ticker
	stopCh  chan struct{}
	running bool
	mu      sync.Mutex
}

func NewActionTicker(b *bot.Bot, chatID int64) *ActionTicker {
	return &ActionTicker{
		b:      b,
		chatID: chatID,
	}
}

// Start begins sending typing action within 1s and refreshes every 4s while busy.
func (a *ActionTicker) Start() {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.running {
		return
	}

	a.running = true
	a.stopCh = make(chan struct{})
	a.ticker = time.NewTicker(4 * time.Second)

	// Send initial chat action immediately
	go a.sendAction()

	go func() {
		for {
			select {
			case <-a.stopCh:
				return
			case <-a.ticker.C:
				a.sendAction()
			}
		}
	}()
}

func (a *ActionTicker) Stop() {
	a.mu.Lock()
	defer a.mu.Unlock()

	if !a.running {
		return
	}

	a.running = false
	if a.ticker != nil {
		a.ticker.Stop()
	}
	close(a.stopCh)
}

func (a *ActionTicker) sendAction() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, _ = a.b.SendChatAction(ctx, &bot.SendChatActionParams{
		ChatID: a.chatID,
		Action: models.ChatActionTyping,
	})
}
