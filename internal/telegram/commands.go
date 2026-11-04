package telegram

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"agent-bridge/internal/config"
	"agent-bridge/internal/db"
	"agent-bridge/internal/formatter"
	"agent-bridge/internal/media"
	"agent-bridge/internal/runner"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

type CommandHandler struct {
	b       *bot.Bot
	cfg     *config.Config
	db      *db.DB
	runner  runner.Runner
	chatID  int64
	session *db.SessionRecord
	project string
}

func NewCommandHandler(b *bot.Bot, cfg *config.Config, database *db.DB, r runner.Runner, chatID int64, session *db.SessionRecord, project string) *CommandHandler {
	return &CommandHandler{
		b:       b,
		cfg:     cfg,
		db:      database,
		runner:  r,
		chatID:  chatID,
		session: session,
		project: project,
	}
}

func (c *CommandHandler) SetSession(session *db.SessionRecord, project string) {
	c.session = session
	c.project = project
}

func (c *CommandHandler) HandleCommand(ctx context.Context, cmdText string) bool {
	parts := strings.Fields(cmdText)
	if len(parts) == 0 {
		return false
	}

	cmd := strings.ToLower(parts[0])
	args := parts[1:]

	switch cmd {
	case "/help":
		c.cmdHelp(ctx)
		return true
	case "/status":
		c.cmdStatus(ctx)
		return true
	case "/stop":
		c.cmdStop(ctx)
		return true
	case "/queue":
		c.cmdQueue(ctx, args)
		return true
	case "/model":
		c.cmdModel(ctx, args)
		return true
	case "/project":
		c.cmdProject(ctx, args)
		return true
	case "/new":
		c.cmdNew(ctx)
		return true
	case "/get":
		c.cmdGet(ctx, args)
		return true
	case "/quiet":
		c.cmdSetVerbosity(ctx, "quiet")
		return true
	case "/normal":
		c.cmdSetVerbosity(ctx, "normal")
		return true
	case "/verbose":
		c.cmdSetVerbosity(ctx, "verbose")
		return true
	}

	return false
}

func (c *CommandHandler) sendMessage(ctx context.Context, params *bot.SendMessageParams) (*models.Message, error) {
	if c.b == nil {
		return nil, nil
	}
	return c.b.SendMessage(ctx, params)
}

func (c *CommandHandler) cmdHelp(ctx context.Context) {
	msg := `🤖 <b>agent-bridge Commands</b>

<b>Session & Project:</b>
/project &lt;name&gt; - Switch to a configured project folder
/new - Start a fresh session in the current project
/status - Current project, model, verbosity, and runner state
/stop - Interrupt the current task (queue will continue)

<b>Models & Progress:</b>
/model - Show model selector buttons
/model &lt;name&gt; - Switch directly to an allowed model
/quiet - Show only a 1-line progress indicator & final answer
/normal - Standard in-place activity card & expandable thinking
/verbose - Stream live tool stdout tails and full traces

<b>Files & Queue:</b>
/get &lt;path&gt; - Send a file from the project folder
/queue - Show messages waiting in the queue
/queue clear - Clear all waiting messages
/help - Show this command list`

	_, _ = c.sendMessage(ctx, &bot.SendMessageParams{
		ChatID:    c.chatID,
		Text:      msg,
		ParseMode: models.ParseModeHTML,
	})
}

func (c *CommandHandler) cmdStatus(ctx context.Context) {
	busyStr := "Idle"
	if c.runner != nil && c.runner.IsBusy() {
		busyStr = "Busy (executing task)"
	}

	pCfg := c.cfg.Projects[c.project]
	msg := fmt.Sprintf(`📊 <b>Bridge Status</b>

<b>Project:</b> <code>%s</code>
<b>Path:</b> <code>%s</code>
<b>Model:</b> <code>%s</code>
<b>Verbosity:</b> <code>%s</code>
<b>Runner State:</b> %s
<b>Session ID:</b> <code>%s</code>`,
		formatter.EscapeHTML(c.project),
		formatter.EscapeHTML(pCfg.Path),
		formatter.EscapeHTML(c.session.Model),
		formatter.EscapeHTML(c.session.Verbosity),
		busyStr,
		formatter.EscapeHTML(c.session.ID),
	)

	_, _ = c.sendMessage(ctx, &bot.SendMessageParams{
		ChatID:    c.chatID,
		Text:      msg,
		ParseMode: models.ParseModeHTML,
	})
}

func (c *CommandHandler) cmdStop(ctx context.Context) {
	if c.runner != nil {
		_ = c.runner.Interrupt()
	}
	_ = c.db.LogAudit("command_stop", fmt.Sprintf("session=%s", c.session.ID))

	_, _ = c.sendMessage(ctx, &bot.SendMessageParams{
		ChatID:    c.chatID,
		Text:      "🛑 <b>Task interrupted.</b> Proceeding to next queued message (if any)...",
		ParseMode: models.ParseModeHTML,
	})
}

func (c *CommandHandler) cmdQueue(ctx context.Context, args []string) {
	if len(args) > 0 && strings.ToLower(args[0]) == "clear" {
		cleared, err := c.db.ClearQueue(c.session.ID)
		if err != nil {
			_, _ = c.sendMessage(ctx, &bot.SendMessageParams{
				ChatID: c.chatID,
				Text:   fmt.Sprintf("Failed to clear queue: %v", err),
			})
			return
		}
		_, _ = c.sendMessage(ctx, &bot.SendMessageParams{
			ChatID:    c.chatID,
			Text:      fmt.Sprintf("🗑 <b>Cleared %d queued message(s).</b>", cleared),
			ParseMode: models.ParseModeHTML,
		})
		return
	}

	count, err := c.db.GetPendingQueueCount(c.session.ID)
	if err != nil {
		_, _ = c.sendMessage(ctx, &bot.SendMessageParams{
			ChatID: c.chatID,
			Text:   fmt.Sprintf("Failed to get queue count: %v", err),
		})
		return
	}

	msg := fmt.Sprintf("📬 <b>Message Queue:</b> %d message(s) waiting.\n<i>Use /queue clear to empty.</i>", count)
	_, _ = c.sendMessage(ctx, &bot.SendMessageParams{
		ChatID:    c.chatID,
		Text:      msg,
		ParseMode: models.ParseModeHTML,
	})
}

func (c *CommandHandler) cmdModel(ctx context.Context, args []string) {
	if len(args) > 0 {
		newModel := args[0]
		valid := false
		for _, m := range c.cfg.Models {
			if m.ID == newModel {
				valid = true
				break
			}
		}
		if !valid {
			var allowed []string
			for _, m := range c.cfg.Models {
				allowed = append(allowed, m.ID)
			}
			_, _ = c.sendMessage(ctx, &bot.SendMessageParams{
				ChatID:    c.chatID,
				Text:      fmt.Sprintf("❌ Unknown model <code>%s</code>.\nAllowed models: <code>%s</code>", formatter.EscapeHTML(newModel), strings.Join(allowed, ", ")),
				ParseMode: models.ParseModeHTML,
			})
			return
		}

		_ = c.db.UpdateSessionModel(c.session.ID, newModel)
		c.session.Model = newModel
		_, _ = c.sendMessage(ctx, &bot.SendMessageParams{
			ChatID:    c.chatID,
			Text:      fmt.Sprintf("✔ Switched model to <code>%s</code> for project <b>%s</b>.", formatter.EscapeHTML(newModel), formatter.EscapeHTML(c.project)),
			ParseMode: models.ParseModeHTML,
		})
		return
	}

	var rows [][]models.InlineKeyboardButton
	for _, m := range c.cfg.Models {
		label := m.DisplayName
		if m.ID == c.session.Model {
			label = "✔ " + label
		}
		rows = append(rows, []models.InlineKeyboardButton{
			{
				Text:         label,
				CallbackData: fmt.Sprintf("set_model:%s", m.ID),
			},
		})
	}

	kb := &models.InlineKeyboardMarkup{InlineKeyboard: rows}
	_, _ = c.sendMessage(ctx, &bot.SendMessageParams{
		ChatID:      c.chatID,
		Text:        fmt.Sprintf("🤖 <b>Select Model for %s:</b>\nCurrent: <code>%s</code>", formatter.EscapeHTML(c.project), formatter.EscapeHTML(c.session.Model)),
		ParseMode:   models.ParseModeHTML,
		ReplyMarkup: kb,
	})
}

func (c *CommandHandler) cmdProject(ctx context.Context, args []string) {
	if len(args) == 0 {
		var lines []string
		for name, p := range c.cfg.Projects {
			cur := ""
			if name == c.project {
				cur = " (current)"
			}
			lines = append(lines, fmt.Sprintf("• <b>%s</b>%s: <code>%s</code>", formatter.EscapeHTML(name), cur, formatter.EscapeHTML(p.Path)))
		}
		msg := fmt.Sprintf("📂 <b>Configured Projects:</b>\n\n%s\n\n<i>Switch with /project &lt;name&gt;</i>", strings.Join(lines, "\n"))
		_, _ = c.sendMessage(ctx, &bot.SendMessageParams{
			ChatID:    c.chatID,
			Text:      msg,
			ParseMode: models.ParseModeHTML,
		})
		return
	}

	target := args[0]
	pCfg, exists := c.cfg.Projects[target]
	if !exists {
		_, _ = c.sendMessage(ctx, &bot.SendMessageParams{
			ChatID:    c.chatID,
			Text:      fmt.Sprintf("❌ Unknown project <code>%s</code>. Use /project to list available projects.", formatter.EscapeHTML(target)),
			ParseMode: models.ParseModeHTML,
		})
		return
	}

	defModel := pCfg.DefaultModel
	if defModel == "" && len(c.cfg.Models) > 0 {
		defModel = c.cfg.Models[0].ID
	}
	sess, err := c.db.GetOrCreateSession(target, defModel, c.cfg.Defaults.Verbosity)
	if err != nil {
		_, _ = c.sendMessage(ctx, &bot.SendMessageParams{
			ChatID: c.chatID,
			Text:   fmt.Sprintf("Failed to load project session: %v", err),
		})
		return
	}

	c.SetSession(sess, target)
	if c.runner != nil {
		_ = c.runner.Start(pCfg.Path, sess.Model, sess.ConversationID)
	}

	_, _ = c.sendMessage(ctx, &bot.SendMessageParams{
		ChatID:    c.chatID,
		Text:      fmt.Sprintf("📂 <b>Switched to project %s</b>\nPath: <code>%s</code>\nModel: <code>%s</code>", formatter.EscapeHTML(target), formatter.EscapeHTML(pCfg.Path), formatter.EscapeHTML(sess.Model)),
		ParseMode: models.ParseModeHTML,
	})
}

func (c *CommandHandler) cmdNew(ctx context.Context) {
	pCfg := c.cfg.Projects[c.project]
	defModel := c.session.Model
	sess, err := c.db.ResetSession(c.project, defModel, c.session.Verbosity)
	if err != nil {
		_, _ = c.sendMessage(ctx, &bot.SendMessageParams{
			ChatID: c.chatID,
			Text:   fmt.Sprintf("Failed to reset session: %v", err),
		})
		return
	}

	c.SetSession(sess, c.project)
	if c.runner != nil {
		_ = c.runner.Start(pCfg.Path, sess.Model, "")
	}

	_, _ = c.sendMessage(ctx, &bot.SendMessageParams{
		ChatID:    c.chatID,
		Text:      fmt.Sprintf("✨ <b>Started fresh session for project %s.</b>", formatter.EscapeHTML(c.project)),
		ParseMode: models.ParseModeHTML,
	})
}

func (c *CommandHandler) cmdGet(ctx context.Context, args []string) {
	if len(args) == 0 {
		_, _ = c.sendMessage(ctx, &bot.SendMessageParams{
			ChatID:    c.chatID,
			Text:      "Usage: <code>/get &lt;path&gt;</code> (e.g. <code>/get README.md</code>)",
			ParseMode: models.ParseModeHTML,
		})
		return
	}

	reqPath := args[0]
	pCfg := c.cfg.Projects[c.project]
	maxSize := int64(c.cfg.Limits.MaxFileSizeMB) * 1024 * 1024
	if maxSize <= 0 {
		maxSize = 50 * 1024 * 1024
	}

	absPath, err := media.ValidateGetPath(pCfg.Path, reqPath, maxSize)
	if err != nil {
		_, _ = c.sendMessage(ctx, &bot.SendMessageParams{
			ChatID:    c.chatID,
			Text:      fmt.Sprintf("❌ <b>Get rejected:</b> %s", formatter.EscapeHTML(err.Error())),
			ParseMode: models.ParseModeHTML,
		})
		return
	}

	f, err := os.Open(absPath)
	if err != nil {
		_, _ = c.sendMessage(ctx, &bot.SendMessageParams{
			ChatID:    c.chatID,
			Text:      fmt.Sprintf("❌ <b>Could not open file:</b> %v", err),
			ParseMode: models.ParseModeHTML,
		})
		return
	}
	defer f.Close()

	if c.b == nil {
		return
	}

	filename := filepath.Base(absPath)
	_, err = c.b.SendDocument(ctx, &bot.SendDocumentParams{
		ChatID:   c.chatID,
		Document: &models.InputFileUpload{Filename: filename, Data: f},
		Caption:  filename,
	})
	if err != nil {
		_, _ = c.sendMessage(ctx, &bot.SendMessageParams{
			ChatID:    c.chatID,
			Text:      fmt.Sprintf("❌ <b>Failed sending file:</b> %v", err),
			ParseMode: models.ParseModeHTML,
		})
	}
}

func (c *CommandHandler) cmdSetVerbosity(ctx context.Context, v string) {
	_ = c.db.UpdateSessionVerbosity(c.session.ID, v)
	c.session.Verbosity = v
	_, _ = c.sendMessage(ctx, &bot.SendMessageParams{
		ChatID:    c.chatID,
		Text:      fmt.Sprintf("✔ Verbosity set to <b>%s</b> for project <b>%s</b>.", v, formatter.EscapeHTML(c.project)),
		ParseMode: models.ParseModeHTML,
	})
}
