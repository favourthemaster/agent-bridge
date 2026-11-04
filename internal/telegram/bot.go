package telegram

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"agent-bridge/internal/activity"
	"agent-bridge/internal/config"
	"agent-bridge/internal/db"
	"agent-bridge/internal/formatter"
	"agent-bridge/internal/media"
	"agent-bridge/internal/parser"
	"agent-bridge/internal/redact"
	"agent-bridge/internal/runner"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

type BridgeBot struct {
	b             *bot.Bot
	cfg           *config.Config
	db            *db.DB
	runner        runner.Runner
	queue         queueManagerInterface
	redactor      *redact.Redactor
	commands      *CommandHandler
	approvals     *ApprovalManager
	actionTicker  *ActionTicker
	outboxWatcher *media.OutboxWatcher
	batcher       *media.AlbumBatcher

	curProject string
	curSession *db.SessionRecord

	activeCard      *activity.Card
	cardDebouncer   *activity.Debouncer
	cardMessageID   int
	isFirstCardSend bool

	turnStartTime  time.Time
	longTaskStopCh chan struct{}

	mu sync.Mutex
}

type queueManagerInterface interface {
	Submit(userID int64, text, filesJSON string) (EnqueueResult, error)
	DrainNext() (*db.QueueItem, error)
	PendingCount() (int, error)
	Clear() (int, error)
	SetSessionID(id string)
}

type EnqueueResult struct {
	Queued     bool
	AheadCount int
	MessageID  int64
}

// QueueAdapter adapts internal/queue.Manager to queueManagerInterface.
type QueueAdapter struct {
	SubmitFn       func(userID int64, text, filesJSON string) (EnqueueResult, error)
	DrainNextFn    func() (*db.QueueItem, error)
	PendingCountFn func() (int, error)
	ClearFn        func() (int, error)
	SetSessionIDFn func(id string)
}

func (q *QueueAdapter) Submit(userID int64, text, filesJSON string) (EnqueueResult, error) {
	return q.SubmitFn(userID, text, filesJSON)
}
func (q *QueueAdapter) DrainNext() (*db.QueueItem, error) {
	return q.DrainNextFn()
}
func (q *QueueAdapter) PendingCount() (int, error) {
	return q.PendingCountFn()
}
func (q *QueueAdapter) Clear() (int, error) {
	return q.ClearFn()
}
func (q *QueueAdapter) SetSessionID(id string) {
	if q.SetSessionIDFn != nil {
		q.SetSessionIDFn(id)
	}
}

func NewBridgeBot(
	cfg *config.Config,
	database *db.DB,
	r runner.Runner,
	q queueManagerInterface,
) (*BridgeBot, error) {
	bb := &BridgeBot{
		cfg:        cfg,
		db:         database,
		runner:     r,
		queue:      q,
		redactor:   redact.New(),
		curProject: cfg.Defaults.Project,
	}

	// Determine starting project
	if bb.curProject == "" {
		for name := range cfg.Projects {
			bb.curProject = name
			break
		}
	}
	pCfg := cfg.Projects[bb.curProject]
	defModel := pCfg.DefaultModel
	if defModel == "" && len(cfg.Models) > 0 {
		defModel = cfg.Models[0].ID
	}

	sess, err := database.GetOrCreateSession(bb.curProject, defModel, cfg.Defaults.Verbosity)
	if err != nil {
		return nil, fmt.Errorf("loading initial session: %w", err)
	}
	bb.curSession = sess
	if q != nil {
		q.SetSessionID(sess.ID)
	}

	// Initialize Bot client
	opts := []bot.Option{
		bot.WithMiddlewares(AuthMiddleware(cfg.Telegram.AllowedUserID)),
		bot.WithDefaultHandler(bb.handleUpdate),
	}

	b, err := bot.New(cfg.Telegram.BotToken, opts...)
	if err != nil {
		return nil, fmt.Errorf("initializing telegram bot: %w", err)
	}
	bb.b = b

	bb.actionTicker = NewActionTicker(b, cfg.Telegram.AllowedUserID)
	bb.approvals = NewApprovalManager(b, database, r, cfg.Telegram.AllowedUserID)
	bb.commands = NewCommandHandler(b, cfg, database, r, cfg.Telegram.AllowedUserID, sess, bb.curProject)

	// Album batcher (2 seconds debounce)
	bb.batcher = media.NewAlbumBatcher(2*time.Second, func(caption string, files []media.SavedFile) {
		prompt := media.BuildPromptForFiles(caption, files)
		bb.submitInstruction(prompt)
	})

	// Start outbox watcher for active project
	bb.initOutboxWatcher(pCfg.Path)

	// Prune old inbox files on startup
	retention := cfg.Limits.InboxRetentionDays
	if retention <= 0 {
		retention = 7
	}
	_, _ = media.PruneInbox(pCfg.Path, retention)

	// Wire runner events
	r.OnEvent(bb.handleRunnerEvent)

	return bb, nil
}

func (bb *BridgeBot) Start(ctx context.Context) error {
	// Register commands in Telegram menu
	_ = bb.registerCommands(ctx)

	// Send silent session connected notice
	_, _ = bb.b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:              bb.cfg.Telegram.AllowedUserID,
		Text:                fmt.Sprintf("🟢 <b>Bridge connected</b> · Project: <b>%s</b> · Model: <code>%s</code>", formatter.EscapeHTML(bb.curProject), formatter.EscapeHTML(bb.curSession.Model)),
		ParseMode:           models.ParseModeHTML,
		DisableNotification: true,
	})

	// Start runner with initial session
	pCfg := bb.cfg.Projects[bb.curProject]
	_ = bb.runner.Start(pCfg.Path, bb.curSession.Model, bb.curSession.ConversationID)

	bb.b.Start(ctx)
	return nil
}

func (bb *BridgeBot) registerCommands(ctx context.Context) error {
	cmds := []models.BotCommand{
		{Command: "status", Description: "Show bridge status, project, and model"},
		{Command: "project", Description: "Switch or view active project"},
		{Command: "new", Description: "Start a fresh session in the project"},
		{Command: "stop", Description: "Interrupt current task"},
		{Command: "model", Description: "Switch AI model"},
		{Command: "queue", Description: "View or clear instruction queue"},
		{Command: "get", Description: "Download a file from the project"},
		{Command: "quiet", Description: "Set progress verbosity to quiet"},
		{Command: "normal", Description: "Set progress verbosity to normal"},
		{Command: "verbose", Description: "Set progress verbosity to verbose"},
		{Command: "help", Description: "List all commands"},
	}
	_, err := bb.b.SetMyCommands(ctx, &bot.SetMyCommandsParams{
		Commands: cmds,
	})
	return err
}

func (bb *BridgeBot) initOutboxWatcher(projectPath string) {
	if bb.outboxWatcher != nil {
		_ = bb.outboxWatcher.Stop()
	}

	ow, err := media.NewOutboxWatcher(projectPath, 2*time.Second, func(path string, cat media.FileCategory) error {
		return bb.sendFileToTelegram(path, cat)
	})
	if err == nil {
		bb.outboxWatcher = ow
	}
}

func (bb *BridgeBot) sendFileToTelegram(path string, cat media.FileCategory) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	filename := f.Name()
	inputUpload := &models.InputFileUpload{
		Filename: filename,
		Data:     f,
	}

	switch cat {
	case media.CategoryPhoto:
		_, err = bb.b.SendPhoto(ctx, &bot.SendPhotoParams{
			ChatID:  bb.cfg.Telegram.AllowedUserID,
			Photo:   inputUpload,
			Caption: filename,
		})
	case media.CategoryAudio:
		_, err = bb.b.SendAudio(ctx, &bot.SendAudioParams{
			ChatID:  bb.cfg.Telegram.AllowedUserID,
			Audio:   inputUpload,
			Caption: filename,
		})
	case media.CategoryVideo:
		_, err = bb.b.SendVideo(ctx, &bot.SendVideoParams{
			ChatID:  bb.cfg.Telegram.AllowedUserID,
			Video:   inputUpload,
			Caption: filename,
		})
	default:
		_, err = bb.b.SendDocument(ctx, &bot.SendDocumentParams{
			ChatID:   bb.cfg.Telegram.AllowedUserID,
			Document: inputUpload,
			Caption:  filename,
		})
	}
	return err
}

func (bb *BridgeBot) handleUpdate(ctx context.Context, b *bot.Bot, update *models.Update) {
	// Persist update ID
	_ = bb.db.SetHandledUpdateID(update.ID)

	// Callback queries (buttons)
	if update.CallbackQuery != nil {
		bb.handleCallback(ctx, update.CallbackQuery)
		return
	}

	if update.Message == nil {
		return
	}

	msg := update.Message

	// Check for commands
	if msg.Text != "" && strings.HasPrefix(msg.Text, "/") {
		if bb.commands.HandleCommand(ctx, msg.Text) {
			return
		}
	}

	// Check for voice note / audio / photo / document
	if msg.Voice != nil {
		bb.downloadAndQueueMedia(ctx, msg.Voice.FileID, "voice_note.ogg", msg.Caption)
		return
	}
	if msg.Audio != nil {
		name := msg.Audio.FileName
		if name == "" {
			name = "audio.mp3"
		}
		bb.downloadAndQueueMedia(ctx, msg.Audio.FileID, name, msg.Caption)
		return
	}
	if len(msg.Photo) > 0 {
		// Get highest resolution photo
		photo := msg.Photo[len(msg.Photo)-1]
		bb.downloadAndQueueMedia(ctx, photo.FileID, "photo.jpg", msg.Caption)
		return
	}
	if msg.Document != nil {
		bb.downloadAndQueueMedia(ctx, msg.Document.FileID, msg.Document.FileName, msg.Caption)
		return
	}

	// Standard text instruction
	if msg.Text != "" {
		bb.submitInstruction(msg.Text)
	}
}

func (bb *BridgeBot) submitInstruction(text string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := bb.queue.Submit(bb.cfg.Telegram.AllowedUserID, text, "")
	if err != nil {
		_, _ = bb.b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: bb.cfg.Telegram.AllowedUserID,
			Text:   fmt.Sprintf("❌ Error submitting instruction: %v", err),
		})
		return
	}

	if res.Queued {
		_, _ = bb.b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:              bb.cfg.Telegram.AllowedUserID,
			Text:                fmt.Sprintf("⏳ <i>Queued (%d ahead)</i>", res.AheadCount),
			ParseMode:           models.ParseModeHTML,
			DisableNotification: true,
		})
		return
	}

	// Started directly: setup activity card & typing indicator
	bb.startTurnUI()
}

func (bb *BridgeBot) startTurnUI() {
	bb.mu.Lock()
	bb.activeCard = activity.NewCard(bb.curProject, bb.curSession.Model, bb.curSession.Verbosity)
	bb.cardMessageID = 0
	bb.isFirstCardSend = true
	bb.turnStartTime = time.Now()

	if bb.longTaskStopCh != nil {
		close(bb.longTaskStopCh)
	}
	stopCh := make(chan struct{})
	bb.longTaskStopCh = stopCh

	debounceInterval := time.Duration(bb.cfg.Limits.CardDebounceSeconds) * time.Second
	if debounceInterval <= 0 {
		debounceInterval = 3 * time.Second
	}

	bb.cardDebouncer = activity.NewDebouncer(bb.activeCard, debounceInterval, func(html string) error {
		return bb.renderCardUpdate(html)
	})
	bb.mu.Unlock()

	bb.actionTicker.Start()

	// Task running longer than 10 minutes notice (repeats every 10 min, silent)
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case t := <-ticker.C:
				bb.mu.Lock()
				proj := bb.curProject
				start := bb.turnStartTime
				bb.mu.Unlock()

				elapsed := t.Sub(start).Round(time.Minute)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				_, _ = bb.b.SendMessage(ctx, &bot.SendMessageParams{
					ChatID:              bb.cfg.Telegram.AllowedUserID,
					Text:                fmt.Sprintf("⏳ <i>Task still running on <b>%s</b> (%s elapsed)</i>", formatter.EscapeHTML(proj), elapsed),
					ParseMode:           models.ParseModeHTML,
					DisableNotification: true,
				})
				cancel()
			}
		}
	}()
}

func (bb *BridgeBot) renderCardUpdate(html string) error {
	bb.mu.Lock()
	defer bb.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if bb.isFirstCardSend || bb.cardMessageID == 0 {
		bb.isFirstCardSend = false
		sent, err := bb.b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:              bb.cfg.Telegram.AllowedUserID,
			Text:                html,
			ParseMode:           models.ParseModeHTML,
			DisableNotification: true,
		})
		if err == nil && sent != nil {
			bb.cardMessageID = sent.ID
		}
		return err
	}

	_, err := bb.b.EditMessageText(ctx, &bot.EditMessageTextParams{
		ChatID:    bb.cfg.Telegram.AllowedUserID,
		MessageID: bb.cardMessageID,
		Text:      html,
		ParseMode: models.ParseModeHTML,
	})
	return err
}

func (bb *BridgeBot) handleRunnerEvent(ev parser.ParsedEvent) {
	switch ev.Type {
	case parser.EventToolStart:
		bb.mu.Lock()
		if bb.activeCard != nil {
			bb.activeCard.AddOrUpdateStep(ev.ID, ev.Text, activity.StepRunning, 0, 0, "")
		}
		deb := bb.cardDebouncer
		bb.mu.Unlock()

		if deb != nil {
			deb.Trigger()
		}

	case parser.EventToolEnd:
		bb.mu.Lock()
		if bb.activeCard != nil {
			status := activity.StepDone
			if !ev.Ok {
				status = activity.StepFailed
			}
			dur := time.Duration(ev.DurationMs) * time.Millisecond
			bb.activeCard.AddOrUpdateStep(ev.ID, ev.Text, status, dur, ev.ExitCode, ev.StderrTail)
		}
		deb := bb.cardDebouncer
		bb.mu.Unlock()

		if deb != nil {
			deb.Trigger()
		}

	case parser.EventApprovalReq:
		bb.actionTicker.Stop()
		bb.mu.Lock()
		if bb.cardDebouncer != nil {
			bb.cardDebouncer.Flush()
		}
		bb.mu.Unlock()

		timeout := time.Duration(bb.cfg.Limits.ApprovalTimeoutMinutes) * time.Minute
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = bb.approvals.RequestApproval(ctx, bb.curSession.ID, ev.ID, ev.Text, timeout)

	case parser.EventPromptReady:
		bb.finishTurn(ev.Text)

	case parser.EventError:
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = bb.b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:              bb.cfg.Telegram.AllowedUserID,
			Text:                fmt.Sprintf("⚠️ <b>Runner Alert</b>: %s", formatter.EscapeHTML(ev.Text)),
			ParseMode:           models.ParseModeHTML,
			DisableNotification: false,
		})
	}
}

func (bb *BridgeBot) finishTurn(finalText string) {
	bb.actionTicker.Stop()

	bb.mu.Lock()
	if bb.longTaskStopCh != nil {
		close(bb.longTaskStopCh)
		bb.longTaskStopCh = nil
	}
	if bb.activeCard != nil {
		bb.activeCard.MarkDone("Done")
	}
	if bb.cardDebouncer != nil {
		bb.cardDebouncer.Flush()
	}
	bb.mu.Unlock()

	// Prepare outbound message
	rawAnswer := strings.TrimSpace(finalText)
	if rawAnswer == "" {
		rawAnswer = "Done."
	}

	// Redaction pass
	safeAnswer := bb.redactor.Redact(rawAnswer)

	// Format as HTML
	formattedHTML := formatter.MarkdownToHTML(safeAnswer)

	// Split message if > 4,096 chars
	chunks := formatter.SplitMessage(formattedHTML, 4000)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, chunk := range chunks {
		_, err := bb.b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:    bb.cfg.Telegram.AllowedUserID,
			Text:      chunk,
			ParseMode: models.ParseModeHTML,
		})
		if err != nil {
			// Fallback: send plain text if Telegram rejects markup
			plain := formatter.StripHTML(chunk)
			_, _ = bb.b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: bb.cfg.Telegram.AllowedUserID,
				Text:   plain,
			})
		}
	}

	// Drain next message from queue if waiting
	go func() {
		time.Sleep(500 * time.Millisecond)
		item, err := bb.queue.DrainNext()
		if err == nil && item != nil {
			bb.startTurnUI()
		}
	}()
}

func (bb *BridgeBot) handleCallback(ctx context.Context, cb *models.CallbackQuery) {
	data := cb.Data

	// Handle approvals
	if strings.HasPrefix(data, "approve:") || strings.HasPrefix(data, "deny:") {
		msgID := 0
		if cb.Message.Message != nil {
			msgID = cb.Message.Message.ID
		}
		_ = bb.approvals.HandleCallback(ctx, cb.ID, data, msgID)
		return
	}

	// Handle model selection
	if strings.HasPrefix(data, "set_model:") {
		modelID := strings.TrimPrefix(data, "set_model:")
		_, _ = bb.b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cb.ID})
		_ = bb.db.UpdateSessionModel(bb.curSession.ID, modelID)
		bb.curSession.Model = modelID

		msgID := 0
		if cb.Message.Message != nil {
			msgID = cb.Message.Message.ID
		}
		_, _ = bb.b.EditMessageText(ctx, &bot.EditMessageTextParams{
			ChatID:    bb.cfg.Telegram.AllowedUserID,
			MessageID: msgID,
			Text:      fmt.Sprintf("✔ Switched model to <code>%s</code> for project <b>%s</b>.", formatter.EscapeHTML(modelID), formatter.EscapeHTML(bb.curProject)),
			ParseMode: models.ParseModeHTML,
		})
	}
}

func (bb *BridgeBot) downloadAndQueueMedia(ctx context.Context, fileID, filename, caption string) {
	pCfg := bb.cfg.Projects[bb.curProject]
	fileObj, err := bb.b.GetFile(ctx, &bot.GetFileParams{FileID: fileID})
	if err != nil {
		_, _ = bb.b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: bb.cfg.Telegram.AllowedUserID,
			Text:   fmt.Sprintf("❌ Could not get file details: %v", err),
		})
		return
	}

	downloadURL := bb.b.FileDownloadLink(fileObj)
	resp, err := http.Get(downloadURL)
	if err != nil {
		_, _ = bb.b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: bb.cfg.Telegram.AllowedUserID,
			Text:   fmt.Sprintf("❌ Download failed: %v", err),
		})
		return
	}
	defer resp.Body.Close()

	maxBytes := int64(bb.cfg.Limits.MaxFileSizeMB) * 1024 * 1024
	if maxBytes <= 0 {
		maxBytes = 20 * 1024 * 1024
	}

	saved, err := media.SaveInboxFile(pCfg.Path, filename, io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		_, _ = bb.b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: bb.cfg.Telegram.AllowedUserID,
			Text:   fmt.Sprintf("❌ Error saving file to inbox: %v", err),
		})
		return
	}

	if saved.Size > maxBytes {
		_ = os.Remove(saved.Path)
		_, _ = bb.b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: bb.cfg.Telegram.AllowedUserID,
			Text:   fmt.Sprintf("❌ File exceeds the %d MB download limit.", bb.cfg.Limits.MaxFileSizeMB),
		})
		return
	}

	// Add to album batcher
	bb.batcher.Add(*saved, caption)
}
