package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"agent-bridge/internal/config"
	"agent-bridge/internal/db"
	"agent-bridge/internal/queue"
	"agent-bridge/internal/runner"
	"agent-bridge/internal/telegram"
)

func main() {
	var configPath string
	var useMock bool

	defaultConfig := "config.yaml"
	if home, err := os.UserHomeDir(); err == nil {
		stdPath := filepath.Join(home, ".config", "agent-bridge", "config.yaml")
		if _, err := os.Stat(stdPath); err == nil {
			defaultConfig = stdPath
		}
	}

	flag.StringVar(&configPath, "config", defaultConfig, "Path to YAML configuration file")
	flag.BoolVar(&useMock, "mock", false, "Use in-memory fake runner instead of real agy PTY (for testing/demos)")
	flag.Parse()

	log.Printf("[agent-bridge] Starting agent-bridge daemon...")
	log.Printf("[agent-bridge] Loading config from %s", configPath)

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("[agent-bridge] Error loading config: %v", err)
	}

	log.Printf("[agent-bridge] Config loaded. Allowed Telegram User ID: %d", cfg.Telegram.AllowedUserID)
	log.Printf("[agent-bridge] Configured projects: %d", len(cfg.Projects))
	for name, p := range cfg.Projects {
		log.Printf("  • %s -> %s (model: %s)", name, p.Path, p.DefaultModel)
	}

	// Initialize SQLite Database
	database, err := db.Open(cfg.Storage.DBPath)
	if err != nil {
		log.Fatalf("[agent-bridge] Error initializing database at %s: %v", cfg.Storage.DBPath, err)
	}
	defer database.Close()
	log.Printf("[agent-bridge] Database initialized at %s", cfg.Storage.DBPath)

	// Initialize Agent Runner
	var agentRunner runner.Runner
	if useMock {
		log.Printf("[agent-bridge] Initializing FakeRunner (mock mode)")
		fake := runner.NewFakeRunner()
		fake.OnInstruction = func(text string, r *runner.FakeRunner) {
			r.EmitTurnSuccess("Mock reasoning", fmt.Sprintf("Echo: %s", text))
		}
		agentRunner = fake
	} else {
		log.Printf("[agent-bridge] Initializing PTYRunner for agy")
		agentRunner = runner.NewPTYRunner()
	}
	defer agentRunner.Stop()

	// Initialize Queue Manager
	queueMgr := queue.NewManager(database, agentRunner, "")

	// Adapt queue to telegram interface
	queueAdapter := &telegram.QueueAdapter{
		SubmitFn: func(userID int64, text, filesJSON string) (telegram.EnqueueResult, error) {
			res, err := queueMgr.Submit(userID, text, filesJSON)
			return telegram.EnqueueResult{
				Queued:     res.Queued,
				AheadCount: res.AheadCount,
				MessageID:  res.MessageID,
			}, err
		},
		DrainNextFn: func() (*db.QueueItem, error) {
			return queueMgr.DrainNext()
		},
		PendingCountFn: func() (int, error) {
			return queueMgr.PendingCount()
		},
		ClearFn: func() (int, error) {
			return queueMgr.Clear()
		},
		SetSessionIDFn: func(id string) {
			queueMgr.SetSessionID(id)
		},
	}

	// Initialize Telegram Bot
	bridgeBot, err := telegram.NewBridgeBot(cfg, database, agentRunner, queueAdapter)
	if err != nil {
		log.Fatalf("[agent-bridge] Error creating Telegram bot: %v", err)
	}

	// Context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	go func() {
		sig := <-sigCh
		log.Printf("[agent-bridge] Received signal %s, shutting down gracefully...", sig)
		cancel()
	}()

	log.Printf("[agent-bridge] Bridge is now listening for Telegram messages (long-polling)...")
	if err := bridgeBot.Start(ctx); err != nil && err != context.Canceled {
		log.Fatalf("[agent-bridge] Bot runtime error: %v", err)
	}

	log.Printf("[agent-bridge] Shutdown complete. Goodbye.")
}
