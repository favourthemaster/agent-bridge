package mobile

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"agent-bridge/internal/config"
	"agent-bridge/internal/db"
	"agent-bridge/internal/media"
	"agent-bridge/internal/parser"
	"agent-bridge/internal/queue"
	"agent-bridge/internal/redact"
	"agent-bridge/internal/runner"

	"github.com/gorilla/websocket"
)

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type StepRecord struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	Category   string `json:"category"` // explore, subagent, edit, command, tool
	Status     string `json:"status"`   // running, done, error
	DurationMs int64  `json:"duration_ms"`
	ExitCode   int    `json:"exit_code,omitempty"`
	Output     string `json:"output,omitempty"`
}

type AttachmentRef struct {
	Name      string `json:"name"`
	RelPath   string `json:"rel_path"`
	Category  string `json:"category"`
	SizeBytes int64  `json:"size_bytes"`
	IsVoice   bool   `json:"is_voice,omitempty"`
	Caption   string `json:"caption,omitempty"`
}

type ServerMessage struct {
	Event string      `json:"event"`
	Data  interface{} `json:"data,omitempty"`
}

type ClientAction struct {
	Action        string          `json:"action"`
	Text          string          `json:"text,omitempty"`
	Project       string          `json:"project,omitempty"`
	SessionID     string          `json:"session_id,omitempty"`
	Model         string          `json:"model,omitempty"`
	Verbosity     string          `json:"verbosity,omitempty"`
	AutoApprove   *bool           `json:"auto_approve,omitempty"`
	ApprovalID    string          `json:"approval_id,omitempty"`
	Allowed       *bool           `json:"allowed,omitempty"`
	QueueItemID   int64           `json:"queue_item_id,omitempty"`
	ConnectorName string          `json:"connector_name,omitempty"`
	Enabled       *bool           `json:"enabled,omitempty"`
	Attachments   []AttachmentRef `json:"attachments,omitempty"`
	TaskID        string          `json:"task_id,omitempty"`
	Title         string          `json:"title,omitempty"`
	ScheduleLabel string          `json:"schedule_label,omitempty"`
	IntervalMins  int             `json:"interval_mins,omitempty"`
}

type wsClient struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func (c *wsClient) send(msg ServerMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(8 * time.Second))
	return c.conn.WriteJSON(msg)
}

type Server struct {
	cfg           *config.Config
	db            *db.DB
	runner        runner.Runner
	queue         *queue.Manager
	redactor      *redact.Redactor
	outboxWatcher *media.OutboxWatcher

	mu             sync.Mutex
	curProject     string
	curSession     *db.SessionRecord
	clients        map[*wsClient]struct{}
	approvalTimers map[string]*time.Timer

	// Live in-flight turn accumulator
	turnActive    bool
	turnStartTime time.Time
	turnThinking  strings.Builder
	turnText      strings.Builder
	turnSteps     []StepRecord
	startedAt     time.Time
}

func NewServer(cfg *config.Config, database *db.DB, r runner.Runner, q *queue.Manager) (*Server, error) {
	s := &Server{
		cfg:            cfg,
		db:             database,
		runner:         r,
		queue:          q,
		redactor:       redact.New(),
		curProject:     cfg.Defaults.Project,
		clients:        make(map[*wsClient]struct{}),
		approvalTimers: make(map[string]*time.Timer),
		startedAt:      time.Now(),
	}

	if s.curProject == "" {
		var names []string
		for name := range cfg.Projects {
			names = append(names, name)
		}
		sort.Strings(names)
		if len(names) > 0 {
			s.curProject = names[0]
		}
	}

	pCfg := cfg.Projects[s.curProject]
	defModel := pCfg.DefaultModel
	if defModel == "" && len(cfg.Models) > 0 {
		defModel = cfg.Models[0].ID
	}

	sess, err := database.GetOrCreateSession(s.curProject, defModel, cfg.Defaults.Verbosity)
	if err != nil {
		return nil, fmt.Errorf("loading mobile initial session: %w", err)
	}
	s.curSession = sess
	if q != nil {
		q.SetSessionID(sess.ID)
	}

	r.SetAutoApprove(pCfg.AutoApprove || cfg.Defaults.AutoApprove)
	_ = r.Start(pCfg.Path, sess.Model, sess.ConversationID)
	r.OnEvent(s.handleRunnerEvent)

	// Seed default scheduled tasks if table is empty so Scheduled Tasks view has helpful presets ready (disabled by default)
	if existing, err := database.ListScheduledTasks(); err == nil && len(existing) == 0 {
		_ = database.CreateScheduledTask(db.ScheduledTaskRecord{
			ID:            "sched_ci_check",
			ProjectName:   s.curProject,
			Title:         "Check CI status for open PRs, fix, merge",
			Prompt:        "Check GitHub Actions CI status on open PRs and summarize any failing checks.",
			ScheduleLabel: "Hourly",
			IntervalMins:  60,
			Enabled:       false,
		})
		_ = database.CreateScheduledTask(db.ScheduledTaskRecord{
			ID:            "sched_daily_commits",
			ProjectName:   s.curProject,
			Title:         "View commits from yesterday, explain overview of changes",
			Prompt:        "Inspect git log since yesterday and summarize architectural changes and open risks.",
			ScheduleLabel: "Daily around 9:00 AM",
			IntervalMins:  1440,
			Enabled:       false,
		})
	}

	return s, nil
}

// InitOutboxWatcher attaches an outbox watcher when Telegram is not running its own watcher.
func (s *Server) InitOutboxWatcher() {
	s.mu.Lock()
	pCfg := s.cfg.Projects[s.curProject]
	if s.outboxWatcher != nil {
		_ = s.outboxWatcher.Stop()
		s.outboxWatcher = nil
	}
	s.mu.Unlock()

	if pCfg.Path == "" {
		return
	}
	ow, err := media.NewOutboxWatcher(pCfg.Path, 1500*time.Millisecond, func(path string, cat media.FileCategory) error {
		s.NotifyOutboxFile(path, string(cat))
		return nil
	})
	if err == nil {
		s.mu.Lock()
		s.outboxWatcher = ow
		s.mu.Unlock()
	}
}

func (s *Server) NotifyOutboxFile(absPath, category string) {
	s.mu.Lock()
	proj := s.curProject
	sessID := s.curSession.ID
	s.mu.Unlock()

	base := filepath.Base(absPath)
	rel := filepath.ToSlash(filepath.Join(".bridge", "sent", base))

	evRec := db.InboxEventRecord{
		SessionID:   sessID,
		ProjectName: proj,
		Kind:        "artifact",
		Title:       fmt.Sprintf("%s · New deliverable", proj),
		Body:        base,
		CreatedAt:   time.Now().Unix(),
	}
	id, _ := s.db.CreateInboxEvent(evRec)
	evRec.ID = id

	s.broadcast(ServerMessage{
		Event: "outbox_file",
		Data: map[string]interface{}{
			"project":  proj,
			"filename": base,
			"rel_path": rel,
			"category": category,
		},
	})
	s.broadcast(ServerMessage{
		Event: "inbox_event",
		Data:  evRec,
	})
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/health", s.handleHealth)
	mux.HandleFunc("/api/v1/ws", s.withAuth(s.handleWS))
	mux.HandleFunc("/api/v1/events", s.withAuth(s.handleEvents))
	mux.HandleFunc("/api/v1/inbox/read", s.withAuth(s.handleMarkInboxRead))
	mux.HandleFunc("/api/v1/approvals/resolve", s.withAuth(s.handleResolveApprovalHTTP))
	mux.HandleFunc("/api/v1/upload", s.withAuth(s.handleUpload))
	mux.HandleFunc("/api/v1/files/tree", s.withAuth(s.handleFilesTree))
	mux.HandleFunc("/api/v1/files/content", s.withAuth(s.handleFileContent))
	mux.HandleFunc("/api/v1/artifacts", s.withAuth(s.handleArtifacts))
	return withCORS(mux)
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) checkAuth(r *http.Request) bool {
	expected := strings.TrimSpace(s.cfg.Mobile.AuthToken)
	if expected == "" {
		return true
	}
	got := strings.TrimSpace(r.URL.Query().Get("token"))
	if got == "" {
		authHdr := strings.TrimSpace(r.Header.Get("Authorization"))
		if strings.HasPrefix(strings.ToLower(authHdr), "bearer ") {
			got = strings.TrimSpace(authHdr[7:])
		}
	}
	if len(got) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(expected)) == 1
}

func (s *Server) withAuth(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.checkAuth(r) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		fn(w, r)
	}
}

func (s *Server) Start(ctx context.Context) error {
	srv := &http.Server{
		Addr:    s.cfg.Mobile.ListenAddr,
		Handler: s.Handler(),
	}

	go s.runSchedulerLoop(ctx)

	errCh := make(chan error, 1)
	go func() {
		log.Printf("[mobile] Antigravity mobile server listening on %s", s.cfg.Mobile.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		s.mu.Lock()
		if s.outboxWatcher != nil {
			_ = s.outboxWatcher.Stop()
		}
		s.mu.Unlock()
		return nil
	case err := <-errCh:
		return err
	}
}

func (s *Server) runSchedulerLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tasks, err := s.db.ListScheduledTasks()
			if err != nil {
				continue
			}
			now := time.Now().Unix()
			for _, t := range tasks {
				if !t.Enabled {
					continue
				}
				intervalSec := int64(t.IntervalMins) * 60
				if intervalSec <= 0 {
					intervalSec = 3600
				}
				if t.LastRunAt == 0 || (now-t.LastRunAt) >= intervalSec {
					_ = s.db.MarkScheduledTaskRun(t.ID, now)
					s.submitInstruction(fmt.Sprintf("[Scheduled: %s]\n%s", t.Title, t.Prompt), nil)
				}
			}
		}
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	proj := s.curProject
	sessID := ""
	model := ""
	if s.curSession != nil {
		sessID = s.curSession.ID
		model = s.curSession.Model
	}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":     "ok",
		"project":    proj,
		"session_id": sessID,
		"model":      model,
		"busy":       s.runner.IsBusy(),
		"uptime_sec": int(time.Since(s.startedAt).Seconds()),
	})
}

func (s *Server) buildHelloPayload() map[string]interface{} {
	s.mu.Lock()
	proj := s.curProject
	sess := *s.curSession
	turnActive := s.turnActive
	thinkingSoFar := s.turnThinking.String()
	textSoFar := s.turnText.String()
	stepsSoFar := make([]StepRecord, len(s.turnSteps))
	copy(stepsSoFar, s.turnSteps)
	var elapsedMs int64
	if turnActive && !s.turnStartTime.IsZero() {
		elapsedMs = time.Since(s.turnStartTime).Milliseconds()
	}
	s.mu.Unlock()

	type ProjectItem struct {
		Name         string `json:"name"`
		Path         string `json:"path"`
		DefaultModel string `json:"default_model"`
		AutoApprove  bool   `json:"auto_approve"`
	}
	var projects []ProjectItem
	for name, p := range s.cfg.Projects {
		projects = append(projects, ProjectItem{
			Name:         name,
			Path:         p.Path,
			DefaultModel: p.DefaultModel,
			AutoApprove:  p.AutoApprove,
		})
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].Name < projects[j].Name })

	var models []map[string]string
	for _, m := range s.cfg.Models {
		models = append(models, map[string]string{
			"id":           m.ID,
			"display_name": m.DisplayName,
		})
	}
	if len(models) == 0 {
		models = []map[string]string{
			{"id": "gemini-3.8-flash-high", "display_name": "Gemini 3.8 Flash (High)"},
			{"id": "gemini-3.8-flash-medium", "display_name": "Gemini 3.8 Flash (Medium)"},
			{"id": "gemini-3.1-pro-high", "display_name": "Gemini 3.1 Pro (High)"},
			{"id": "claude-sonnet-4-6", "display_name": "Claude Sonnet 4.6"},
			{"id": "claude-opus-4-6-thinking", "display_name": "Claude Opus 4.6"},
		}
	}

	messages, _ := s.db.ListSessionMessages(sess.ID, 200)
	sessions, _ := s.db.ListSessions(50)
	queueItems, _ := s.db.ListPendingQueueItems(sess.ID)
	approvals, _ := s.db.ListPendingApprovals()
	scheduled, _ := s.db.ListScheduledTasks()
	inbox, _ := s.db.ListInboxEventsSince(0, 30)

	return map[string]interface{}{
		"active_project": proj,
		"session": map[string]interface{}{
			"id":              sess.ID,
			"project_name":    sess.ProjectName,
			"conversation_id": sess.ConversationID,
			"model":           sess.Model,
			"verbosity":       sess.Verbosity,
		},
		"auto_approve": s.runner.IsAutoApprove(),
		"busy":         s.runner.IsBusy() || turnActive,
		"in_flight": map[string]interface{}{
			"active":     turnActive,
			"thinking":   thinkingSoFar,
			"text":       textSoFar,
			"steps":      stepsSoFar,
			"elapsed_ms": elapsedMs,
		},
		"projects":          projects,
		"models":            models,
		"connectors":        ListMCPConnectors(),
		"messages":          messages,
		"sessions":          sessions,
		"queue":             queueItems,
		"pending_approvals": approvals,
		"scheduled_tasks":   scheduled,
		"inbox_events":      inbox,
		"unread_inbox":      s.db.UnreadInboxCount(),
	}
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	client := &wsClient{conn: conn}

	s.mu.Lock()
	s.clients[client] = struct{}{}
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.clients, client)
		s.mu.Unlock()
		_ = conn.Close()
	}()

	// Send initial hello state
	_ = client.send(ServerMessage{
		Event: "hello",
		Data:  s.buildHelloPayload(),
	})

	// Catch up missed notification events if client supplied since_event_id
	if sinceStr := r.URL.Query().Get("since_event_id"); sinceStr != "" {
		if sinceID, err := strconv.ParseInt(sinceStr, 10, 64); err == nil && sinceID > 0 {
			if missed, err := s.db.ListInboxEventsSince(sinceID, 50); err == nil {
				for _, ev := range missed {
					_ = client.send(ServerMessage{Event: "inbox_event", Data: ev})
				}
			}
		}
	}

	for {
		_, rawBytes, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var act ClientAction
		if err := json.Unmarshal(rawBytes, &act); err != nil {
			_ = client.send(ServerMessage{Event: "error", Data: map[string]string{"message": "invalid json"}})
			continue
		}
		s.handleClientAction(client, act)
	}
}

func (s *Server) broadcast(msg ServerMessage) {
	s.mu.Lock()
	list := make([]*wsClient, 0, len(s.clients))
	for c := range s.clients {
		list = append(list, c)
	}
	s.mu.Unlock()

	for _, c := range list {
		_ = c.send(msg)
	}
}

func (s *Server) handleClientAction(client *wsClient, act ClientAction) {
	switch act.Action {
	case "ping":
		_ = client.send(ServerMessage{Event: "pong", Data: map[string]int64{"ts": time.Now().UnixMilli()}})

	case "refresh":
		_ = client.send(ServerMessage{Event: "hello", Data: s.buildHelloPayload()})

	case "send_prompt":
		text := strings.TrimSpace(act.Text)
		if text == "" && len(act.Attachments) == 0 {
			return
		}
		s.submitInstruction(text, act.Attachments)

	case "cancel_turn":
		_ = s.runner.Interrupt()
		s.mu.Lock()
		s.turnActive = false
		s.mu.Unlock()
		s.broadcast(ServerMessage{
			Event: "turn_cancelled",
			Data:  map[string]string{"status": "interrupted"},
		})
		go s.drainNextQueued()

	case "new_session":
		s.mu.Lock()
		proj := s.curProject
		model := s.curSession.Model
		verb := s.curSession.Verbosity
		pCfg := s.cfg.Projects[proj]
		s.mu.Unlock()

		sess, err := s.db.ResetSession(proj, model, verb)
		if err != nil {
			return
		}
		s.mu.Lock()
		s.curSession = sess
		s.turnActive = false
		s.turnThinking.Reset()
		s.turnText.Reset()
		s.turnSteps = nil
		s.mu.Unlock()

		if s.queue != nil {
			s.queue.SetSessionID(sess.ID)
		}
		_ = s.runner.Start(pCfg.Path, sess.Model, "")
		s.broadcast(ServerMessage{Event: "hello", Data: s.buildHelloPayload()})

	case "resume_session":
		if act.SessionID == "" {
			return
		}
		sess, err := s.db.GetSessionByID(act.SessionID)
		if err != nil || sess == nil {
			return
		}
		pCfg, ok := s.cfg.Projects[sess.ProjectName]
		if !ok {
			return
		}
		_ = s.db.ActivateSession(sess.ID, sess.ProjectName)

		s.mu.Lock()
		s.curProject = sess.ProjectName
		s.curSession = sess
		s.mu.Unlock()

		if s.queue != nil {
			s.queue.SetSessionID(sess.ID)
		}
		_ = s.runner.Start(pCfg.Path, sess.Model, sess.ConversationID)
		s.broadcast(ServerMessage{Event: "hello", Data: s.buildHelloPayload()})

	case "switch_project":
		target := strings.TrimSpace(act.Project)
		pCfg, exists := s.cfg.Projects[target]
		if !exists {
			return
		}
		defModel := pCfg.DefaultModel
		if defModel == "" && len(s.cfg.Models) > 0 {
			defModel = s.cfg.Models[0].ID
		}
		sess, err := s.db.GetOrCreateSession(target, defModel, s.cfg.Defaults.Verbosity)
		if err != nil {
			return
		}
		s.mu.Lock()
		s.curProject = target
		s.curSession = sess
		hasWatcher := s.outboxWatcher != nil
		s.mu.Unlock()

		if s.queue != nil {
			s.queue.SetSessionID(sess.ID)
		}
		s.runner.SetAutoApprove(pCfg.AutoApprove || s.cfg.Defaults.AutoApprove)
		_ = s.runner.Start(pCfg.Path, sess.Model, sess.ConversationID)
		if hasWatcher {
			s.InitOutboxWatcher()
		}
		s.broadcast(ServerMessage{Event: "hello", Data: s.buildHelloPayload()})

	case "set_model":
		modelID := strings.TrimSpace(act.Model)
		if modelID == "" {
			return
		}
		s.mu.Lock()
		sessID := s.curSession.ID
		s.curSession.Model = modelID
		proj := s.curProject
		convID := s.curSession.ConversationID
		pCfg := s.cfg.Projects[proj]
		s.mu.Unlock()

		_ = s.db.UpdateSessionModel(sessID, modelID)
		if !s.runner.IsBusy() {
			_ = s.runner.Start(pCfg.Path, modelID, convID)
		}
		s.broadcast(ServerMessage{
			Event: "session_updated",
			Data:  map[string]interface{}{"model": modelID, "project": proj},
		})

	case "set_verbosity":
		v := strings.ToLower(strings.TrimSpace(act.Verbosity))
		if v != "quiet" && v != "normal" && v != "verbose" {
			return
		}
		s.mu.Lock()
		sessID := s.curSession.ID
		s.curSession.Verbosity = v
		s.mu.Unlock()
		_ = s.db.UpdateSessionVerbosity(sessID, v)
		s.broadcast(ServerMessage{
			Event: "session_updated",
			Data:  map[string]interface{}{"verbosity": v},
		})

	case "set_auto_approve":
		if act.AutoApprove == nil {
			return
		}
		s.runner.SetAutoApprove(*act.AutoApprove)
		s.broadcast(ServerMessage{
			Event: "session_updated",
			Data:  map[string]interface{}{"auto_approve": *act.AutoApprove},
		})

	case "resolve_approval":
		if act.ApprovalID == "" || act.Allowed == nil {
			return
		}
		s.resolveApproval(act.ApprovalID, *act.Allowed)

	case "queue_force":
		if act.QueueItemID <= 0 {
			return
		}
		_ = s.db.PrioritizeMessage(act.QueueItemID)
		_ = s.runner.Interrupt()
		s.broadcastQueue()

	case "queue_cancel":
		if act.QueueItemID <= 0 {
			return
		}
		_ = s.db.CancelMessage(act.QueueItemID)
		s.broadcastQueue()

	case "queue_clear":
		if s.queue != nil {
			_, _ = s.queue.Clear()
		}
		s.broadcastQueue()

	case "toggle_connector":
		if act.ConnectorName != "" && act.Enabled != nil {
			_ = ToggleMCPConnector(act.ConnectorName, *act.Enabled)
			s.broadcast(ServerMessage{
				Event: "connectors_updated",
				Data:  ListMCPConnectors(),
			})
		}

	case "scheduled_create":
		title := strings.TrimSpace(act.Title)
		prompt := strings.TrimSpace(act.Text)
		if title == "" || prompt == "" {
			return
		}
		s.mu.Lock()
		proj := act.Project
		if proj == "" {
			proj = s.curProject
		}
		s.mu.Unlock()

		label := act.ScheduleLabel
		if label == "" {
			label = "Daily around 9:00 AM"
		}
		mins := act.IntervalMins
		if mins <= 0 {
			mins = 1440
		}
		rec := db.ScheduledTaskRecord{
			ID:            fmt.Sprintf("sched_%d", time.Now().UnixNano()),
			ProjectName:   proj,
			Title:         title,
			Prompt:        prompt,
			ScheduleLabel: label,
			IntervalMins:  mins,
			Enabled:       true,
		}
		_ = s.db.CreateScheduledTask(rec)
		tasks, _ := s.db.ListScheduledTasks()
		s.broadcast(ServerMessage{Event: "scheduled_updated", Data: tasks})

	case "scheduled_toggle":
		if act.TaskID != "" && act.Enabled != nil {
			_ = s.db.ToggleScheduledTask(act.TaskID, *act.Enabled)
			tasks, _ := s.db.ListScheduledTasks()
			s.broadcast(ServerMessage{Event: "scheduled_updated", Data: tasks})
		}

	case "scheduled_delete":
		if act.TaskID != "" {
			_ = s.db.DeleteScheduledTask(act.TaskID)
			tasks, _ := s.db.ListScheduledTasks()
			s.broadcast(ServerMessage{Event: "scheduled_updated", Data: tasks})
		}

	case "scheduled_run_now":
		if act.TaskID != "" {
			tasks, _ := s.db.ListScheduledTasks()
			for _, t := range tasks {
				if t.ID == act.TaskID {
					_ = s.db.MarkScheduledTaskRun(t.ID, time.Now().Unix())
					s.submitInstruction(t.Prompt, nil)
					break
				}
			}
		}
	}
}

func (s *Server) submitInstruction(text string, attachments []AttachmentRef) {
	s.mu.Lock()
	proj := s.curProject
	sess := *s.curSession
	pCfg := s.cfg.Projects[proj]
	s.mu.Unlock()

	dispatchPrompt := text
	if len(attachments) > 0 {
		var savedFiles []media.SavedFile
		for _, a := range attachments {
			absPath := filepath.Join(pCfg.Path, filepath.FromSlash(a.RelPath))
			savedFiles = append(savedFiles, media.SavedFile{
				Path:     absPath,
				Filename: a.Name,
				Size:     a.SizeBytes,
				IsVoice:  a.IsVoice,
			})
		}
		dispatchPrompt = media.BuildPromptForFiles(text, savedFiles)
	}

	attachJSON := ""
	if len(attachments) > 0 {
		if b, err := json.Marshal(attachments); err == nil {
			attachJSON = string(b)
		}
	}

	displayContent := text
	if displayContent == "" && len(attachments) > 0 {
		if attachments[0].IsVoice {
			displayContent = "Voice note instruction"
		} else {
			displayContent = fmt.Sprintf("Attached %d file(s)", len(attachments))
		}
	}

	msgRec := db.ChatMessageRecord{
		SessionID:       sess.ID,
		ProjectName:     proj,
		Role:            "user",
		Content:         displayContent,
		AttachmentsJSON: attachJSON,
		Model:           sess.Model,
		CreatedAt:       time.Now().Unix(),
	}
	msgID, _ := s.db.SaveChatMessage(msgRec)
	msgRec.ID = msgID

	s.broadcast(ServerMessage{
		Event: "user_message",
		Data:  msgRec,
	})

	if s.queue != nil {
		res, err := s.queue.Submit(1, dispatchPrompt, attachJSON)
		if err != nil {
			s.broadcast(ServerMessage{Event: "error", Data: map[string]string{"message": err.Error()}})
			return
		}
		if res.Queued {
			s.broadcastQueue()
			return
		}
	} else {
		if err := s.runner.WriteInstruction(dispatchPrompt); err != nil {
			s.broadcast(ServerMessage{Event: "error", Data: map[string]string{"message": err.Error()}})
			return
		}
	}

	s.beginTurnState(sess, proj, displayContent)
}

func (s *Server) beginTurnState(sess db.SessionRecord, proj, prompt string) {
	s.mu.Lock()
	s.turnActive = true
	s.turnStartTime = time.Now()
	s.turnThinking.Reset()
	s.turnText.Reset()
	s.turnSteps = nil
	s.mu.Unlock()

	s.broadcast(ServerMessage{
		Event: "turn_start",
		Data: map[string]interface{}{
			"session_id": sess.ID,
			"project":    proj,
			"model":      sess.Model,
			"prompt":     prompt,
			"started_at": time.Now().UnixMilli(),
		},
	})
}

func classifyStepCategory(label string) string {
	l := strings.ToLower(strings.TrimSpace(label))
	switch {
	case strings.HasPrefix(l, "agent:") || strings.Contains(l, "subagent") || strings.Contains(l, "researcher"):
		return "subagent"
	case strings.HasPrefix(l, "read ") || strings.HasPrefix(l, "grep") || strings.HasPrefix(l, "glob") || strings.HasPrefix(l, "find") || strings.HasPrefix(l, "analyzed "):
		return "explore"
	case strings.HasPrefix(l, "edit ") || strings.HasPrefix(l, "write "):
		return "edit"
	case strings.HasPrefix(l, "run:") || strings.Contains(l, ": "):
		return "command"
	default:
		return "tool"
	}
}

var mdImgRegex = regexp.MustCompile(`!\[([^\]]*)\]\((?:file:\/\/)?([^)]+\.(?:png|jpg|jpeg|webp|gif|bmp))\)`)

func (s *Server) extractInlineAttachmentRefs(text, projectDir string) []AttachmentRef {
	if projectDir == "" {
		return nil
	}
	cleanBase, err := filepath.Abs(projectDir)
	if err != nil {
		return nil
	}

	var refs []AttachmentRef
	seen := make(map[string]bool)
	matches := mdImgRegex.FindAllStringSubmatch(text, -1)
	for _, m := range matches {
		if len(m) < 3 {
			continue
		}
		caption := strings.TrimSpace(m[1])
		rawPath := strings.TrimPrefix(strings.TrimSpace(m[2]), "file://")
		absPath := rawPath
		if !filepath.IsAbs(absPath) {
			absPath = filepath.Join(cleanBase, rawPath)
		}
		cleanTarget, err := filepath.Abs(absPath)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(cleanBase, cleanTarget)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		fi, err := os.Stat(cleanTarget)
		if err != nil || fi.IsDir() || seen[cleanTarget] {
			continue
		}
		seen[cleanTarget] = true
		refs = append(refs, AttachmentRef{
			Name:      filepath.Base(cleanTarget),
			RelPath:   filepath.ToSlash(rel),
			Category:  "photo",
			SizeBytes: fi.Size(),
			Caption:   caption,
		})
	}
	return refs
}

func (s *Server) handleRunnerEvent(ev parser.ParsedEvent) {
	switch ev.Type {
	case parser.EventThinkingChunk:
		safe := s.redactor.Redact(ev.Text)
		s.mu.Lock()
		if !s.turnActive {
			s.turnActive = true
			s.turnStartTime = time.Now()
		}
		s.turnThinking.WriteString(safe)
		elapsed := time.Since(s.turnStartTime).Milliseconds()
		s.mu.Unlock()

		s.broadcast(ServerMessage{
			Event: "thinking_delta",
			Data: map[string]interface{}{
				"delta":      safe,
				"elapsed_ms": elapsed,
			},
		})

	case parser.EventToolStart:
		cat := classifyStepCategory(ev.Text)
		step := StepRecord{
			ID:       ev.ID,
			Label:    ev.Text,
			Category: cat,
			Status:   "running",
		}
		s.mu.Lock()
		if !s.turnActive {
			s.turnActive = true
			s.turnStartTime = time.Now()
		}
		updated := false
		for i := range s.turnSteps {
			if s.turnSteps[i].ID == ev.ID && ev.ID != "" {
				s.turnSteps[i] = step
				updated = true
				break
			}
		}
		if !updated {
			s.turnSteps = append(s.turnSteps, step)
		}
		s.mu.Unlock()

		s.broadcast(ServerMessage{
			Event: "tool_start",
			Data:  step,
		})

	case parser.EventToolEnd:
		status := "done"
		if !ev.Ok {
			status = "error"
		}
		outSafe := s.redactor.Redact(ev.StderrTail)
		step := StepRecord{
			ID:         ev.ID,
			Label:      ev.Text,
			Category:   classifyStepCategory(ev.Text),
			Status:     status,
			DurationMs: ev.DurationMs,
			ExitCode:   ev.ExitCode,
			Output:     outSafe,
		}

		s.mu.Lock()
		matched := false
		for i := len(s.turnSteps) - 1; i >= 0; i-- {
			if (ev.ID != "" && s.turnSteps[i].ID == ev.ID) || (ev.ID == "" && s.turnSteps[i].Label == ev.Text && s.turnSteps[i].Status == "running") {
				s.turnSteps[i].Status = status
				s.turnSteps[i].DurationMs = ev.DurationMs
				s.turnSteps[i].ExitCode = ev.ExitCode
				if outSafe != "" {
					s.turnSteps[i].Output = outSafe
				}
				step = s.turnSteps[i]
				matched = true
				break
			}
		}
		if !matched {
			s.turnSteps = append(s.turnSteps, step)
		}
		s.mu.Unlock()

		s.broadcast(ServerMessage{
			Event: "tool_end",
			Data:  step,
		})

	case parser.EventAssistantChunk:
		safe := s.redactor.Redact(ev.Text)
		s.mu.Lock()
		if !s.turnActive {
			s.turnActive = true
			s.turnStartTime = time.Now()
		}
		s.turnText.WriteString(safe)
		s.mu.Unlock()

		s.broadcast(ServerMessage{
			Event: "text_delta",
			Data: map[string]interface{}{
				"delta": safe,
			},
		})

	case parser.EventApprovalReq:
		if s.runner.IsAutoApprove() {
			_ = s.runner.SendApprovalResponse(true)
			return
		}
		s.mu.Lock()
		sessID := s.curSession.ID
		proj := s.curProject
		s.mu.Unlock()

		timeout := time.Duration(s.cfg.Limits.ApprovalTimeoutMinutes) * time.Minute
		if timeout <= 0 {
			timeout = 10 * time.Minute
		}
		expiresAt := time.Now().Add(timeout).Unix()

		// Create DB approval if not already created by Telegram ApprovalManager
		if existing, err := s.db.GetApproval(ev.ID); err != nil || existing == nil {
			_ = s.db.CreateApproval(ev.ID, sessID, ev.Text, expiresAt)
			s.mu.Lock()
			s.approvalTimers[ev.ID] = time.AfterFunc(timeout, func() {
				s.resolveApproval(ev.ID, false)
			})
			s.mu.Unlock()
		}

		payloadBytes, _ := json.Marshal(map[string]interface{}{
			"approval_id": ev.ID,
			"prompt":      ev.Text,
			"expires_at":  expiresAt,
		})
		inboxEv := db.InboxEventRecord{
			SessionID:   sessID,
			ProjectName: proj,
			Kind:        "approval",
			Title:       fmt.Sprintf("Permission needed · %s", proj),
			Body:        ev.Text,
			PayloadJSON: string(payloadBytes),
			CreatedAt:   time.Now().Unix(),
		}
		inboxID, _ := s.db.CreateInboxEvent(inboxEv)
		inboxEv.ID = inboxID

		s.broadcast(ServerMessage{
			Event: "approval_request",
			Data: map[string]interface{}{
				"id":         ev.ID,
				"session_id": sessID,
				"project":    proj,
				"prompt":     ev.Text,
				"expires_at": expiresAt,
			},
		})
		s.broadcast(ServerMessage{
			Event: "inbox_event",
			Data:  inboxEv,
		})

	case parser.EventPromptReady:
		s.mu.Lock()
		proj := s.curProject
		sess := *s.curSession
		pCfg := s.cfg.Projects[proj]
		accumText := s.turnText.String()
		accumThink := s.turnThinking.String()
		steps := make([]StepRecord, len(s.turnSteps))
		copy(steps, s.turnSteps)
		var durationMs int64
		if !s.turnStartTime.IsZero() {
			durationMs = time.Since(s.turnStartTime).Milliseconds()
		}
		s.turnActive = false
		s.turnText.Reset()
		s.turnThinking.Reset()
		s.turnSteps = nil
		s.mu.Unlock()

		finalText := strings.TrimSpace(ev.Text)
		if finalText == "" {
			finalText = strings.TrimSpace(accumText)
		}
		if finalText == "" {
			finalText = "Done."
		}
		safeFinal := s.redactor.Redact(finalText)

		finalThink := strings.TrimSpace(ev.Thinking)
		if finalThink == "" {
			finalThink = strings.TrimSpace(accumThink)
		}
		safeThink := s.redactor.Redact(finalThink)

		stepsJSON := ""
		if len(steps) > 0 {
			if b, err := json.Marshal(steps); err == nil {
				stepsJSON = string(b)
			}
		}

		inlineImgs := s.extractInlineAttachmentRefs(safeFinal, pCfg.Path)
		attachJSON := ""
		if len(inlineImgs) > 0 {
			if b, err := json.Marshal(inlineImgs); err == nil {
				attachJSON = string(b)
			}
		}

		savedMsg := db.ChatMessageRecord{
			SessionID:       sess.ID,
			ProjectName:     proj,
			Role:            "assistant",
			Content:         safeFinal,
			Thinking:        safeThink,
			StepsJSON:       stepsJSON,
			AttachmentsJSON: attachJSON,
			Model:           sess.Model,
			DurationMs:      durationMs,
			CreatedAt:       time.Now().Unix(),
		}
		msgID, _ := s.db.SaveChatMessage(savedMsg)
		savedMsg.ID = msgID

		preview := safeFinal
		if len(preview) > 140 {
			preview = preview[:140] + "…"
		}
		inboxEv := db.InboxEventRecord{
			SessionID:   sess.ID,
			ProjectName: proj,
			Kind:        "turn_complete",
			Title:       fmt.Sprintf("%s · Agent finished (%.1fs)", proj, float64(durationMs)/1000.0),
			Body:        preview,
			CreatedAt:   time.Now().Unix(),
		}
		inboxID, _ := s.db.CreateInboxEvent(inboxEv)
		inboxEv.ID = inboxID

		s.broadcast(ServerMessage{
			Event: "turn_complete",
			Data:  savedMsg,
		})
		s.broadcast(ServerMessage{
			Event: "inbox_event",
			Data:  inboxEv,
		})

		go s.drainNextQueued()

	case parser.EventError:
		safeErr := s.redactor.Redact(ev.Text)
		s.mu.Lock()
		sess := *s.curSession
		proj := s.curProject
		s.turnActive = false
		s.turnText.Reset()
		s.turnThinking.Reset()
		s.turnSteps = nil
		s.mu.Unlock()

		inboxEv := db.InboxEventRecord{
			SessionID:   sess.ID,
			ProjectName: proj,
			Kind:        "error",
			Title:       fmt.Sprintf("%s · Runner Error", proj),
			Body:        safeErr,
			CreatedAt:   time.Now().Unix(),
		}
		inboxID, _ := s.db.CreateInboxEvent(inboxEv)
		inboxEv.ID = inboxID

		s.broadcast(ServerMessage{
			Event: "error",
			Data:  map[string]string{"message": safeErr},
		})
		s.broadcast(ServerMessage{
			Event: "inbox_event",
			Data:  inboxEv,
		})

		go s.drainNextQueued()
	}
}

func (s *Server) drainNextQueued() {
	if s.queue == nil {
		return
	}
	time.Sleep(250 * time.Millisecond)
	item, err := s.queue.DrainNext()
	if err == nil && item != nil {
		s.mu.Lock()
		sess := *s.curSession
		proj := s.curProject
		s.mu.Unlock()
		s.beginTurnState(sess, proj, item.Text)
		s.broadcastQueue()
	}
}

func (s *Server) broadcastQueue() {
	s.mu.Lock()
	sessID := s.curSession.ID
	s.mu.Unlock()
	items, _ := s.db.ListPendingQueueItems(sessID)
	s.broadcast(ServerMessage{
		Event: "queue_updated",
		Data:  items,
	})
}

func (s *Server) resolveApproval(id string, allowed bool) bool {
	s.mu.Lock()
	if t, ok := s.approvalTimers[id]; ok {
		t.Stop()
		delete(s.approvalTimers, id)
	}
	s.mu.Unlock()

	app, err := s.db.GetApproval(id)
	if err != nil || app == nil || app.Status != "pending" {
		return false
	}

	status := "denied"
	if allowed {
		status = "allowed"
	}
	_ = s.db.ResolveApproval(id, status)
	_ = s.db.LogAudit("mobile_approval_resolved", fmt.Sprintf("id=%s status=%s", id, status))
	_ = s.runner.SendApprovalResponse(allowed)

	s.broadcast(ServerMessage{
		Event: "approval_resolved",
		Data: map[string]interface{}{
			"id":      id,
			"status":  status,
			"allowed": allowed,
		},
	})
	return true
}

func (s *Server) handleResolveApprovalHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ID      string `json:"id"`
		Allowed bool   `json:"allowed"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}
	ok := s.resolveApproval(body.ID, body.Allowed)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"resolved": ok,
		"id":       body.ID,
		"allowed":  body.Allowed,
	})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	sinceID, _ := strconv.ParseInt(r.URL.Query().Get("since_id"), 10, 64)
	events, err := s.db.ListInboxEventsSince(sinceID, 50)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"events":       events,
		"unread_count": s.db.UnreadInboxCount(),
	})
}

func (s *Server) handleMarkInboxRead(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		UpToID int64 `json:"up_to_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	_ = s.db.MarkInboxRead(body.UpToID)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "unread_count": s.db.UnreadInboxCount()})
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	maxBytes := int64(s.cfg.Limits.MaxFileSizeMB) * 1024 * 1024
	if maxBytes <= 0 {
		maxBytes = 20 * 1024 * 1024
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes+1024*1024)
	if err := r.ParseMultipartForm(maxBytes); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
		return
	}

	file, hdr, err := r.FormFile("file")
	if err != nil {
		http.Error(w, `{"error":"missing file field"}`, http.StatusBadRequest)
		return
	}
	defer file.Close()

	s.mu.Lock()
	proj := r.FormValue("project")
	if proj == "" {
		proj = s.curProject
	}
	pCfg, ok := s.cfg.Projects[proj]
	s.mu.Unlock()
	if !ok {
		http.Error(w, `{"error":"unknown project"}`, http.StatusBadRequest)
		return
	}

	isVoice := r.FormValue("is_voice") == "true" || r.FormValue("is_voice") == "1"
	saved, err := media.SaveInboxFile(pCfg.Path, hdr.Filename, io.LimitReader(file, maxBytes))
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}
	saved.IsVoice = isVoice

	cleanBase, _ := filepath.Abs(pCfg.Path)
	rel, _ := filepath.Rel(cleanBase, saved.Path)

	ref := AttachmentRef{
		Name:      saved.Filename,
		RelPath:   filepath.ToSlash(rel),
		Category:  media.DetectFileType(saved.Filename),
		SizeBytes: saved.Size,
		IsVoice:   isVoice,
	}

	if r.FormValue("submit_now") == "true" {
		caption := strings.TrimSpace(r.FormValue("caption"))
		s.submitInstruction(caption, []AttachmentRef{ref})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ref)
}

func (s *Server) handleFilesTree(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	proj := r.URL.Query().Get("project")
	if proj == "" {
		proj = s.curProject
	}
	pCfg, ok := s.cfg.Projects[proj]
	s.mu.Unlock()
	if !ok {
		http.Error(w, `{"error":"unknown project"}`, http.StatusBadRequest)
		return
	}

	relDir := r.URL.Query().Get("dir")
	query := r.URL.Query().Get("q")
	entries, err := ListWorkspaceFiles(pCfg.Path, relDir, query, 150)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"project": proj,
		"dir":     relDir,
		"entries": entries,
	})
}

func (s *Server) handleFileContent(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	proj := r.URL.Query().Get("project")
	if proj == "" {
		proj = s.curProject
	}
	pCfg, ok := s.cfg.Projects[proj]
	s.mu.Unlock()
	if !ok {
		http.Error(w, `{"error":"unknown project"}`, http.StatusBadRequest)
		return
	}

	reqPath := r.URL.Query().Get("path")
	maxBytes := int64(s.cfg.Limits.MaxFileSizeMB) * 1024 * 1024
	if maxBytes <= 0 {
		maxBytes = 25 * 1024 * 1024
	}
	absPath, err := media.ValidateGetPath(pCfg.Path, reqPath, maxBytes)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusForbidden)
		return
	}

	ctype := mime.TypeByExtension(strings.ToLower(filepath.Ext(absPath)))
	if ctype == "" {
		ctype = "text/plain; charset=utf-8"
	}
	w.Header().Set("Content-Type", ctype)
	http.ServeFile(w, r, absPath)
}

func (s *Server) handleArtifacts(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	proj := r.URL.Query().Get("project")
	if proj == "" {
		proj = s.curProject
	}
	convID := r.URL.Query().Get("conv_id")
	if convID == "" && s.curSession != nil {
		convID = s.curSession.ConversationID
	}
	pCfg := s.cfg.Projects[proj]
	s.mu.Unlock()

	docs := CollectArtifacts(pCfg.Path, convID)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"project":   proj,
		"artifacts": docs,
	})
}
