package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"agent-bridge/internal/config"
	"agent-bridge/internal/db"
	"agent-bridge/internal/mobile"
	"agent-bridge/internal/parser"
	"agent-bridge/internal/queue"
	"agent-bridge/internal/runner"
	"agent-bridge/internal/telegram"
)

func main() {
	var configPath string
	var useMock bool
	var runnerType string
	var mobileAddr string

	defaultConfig := "config.yaml"
	if home, err := os.UserHomeDir(); err == nil {
		stdPath := filepath.Join(home, ".config", "agent-bridge", "config.yaml")
		if _, err := os.Stat(stdPath); err == nil {
			defaultConfig = stdPath
		}
	}

	flag.StringVar(&configPath, "config", defaultConfig, "Path to YAML configuration file")
	flag.BoolVar(&useMock, "mock", false, "Use scriptable fake runner instead of real agy (for testing/demos)")
	flag.StringVar(&runnerType, "runner", "cli", "Runner mode: 'cli' (headless print/json) or 'pty' (interactive pseudo-terminal)")
	flag.StringVar(&mobileAddr, "mobile-addr", "", "Enable mobile HTTP/WebSocket server at address (e.g. :8765)")
	flag.Parse()

	if mobileAddr != "" {
		_ = os.Setenv("AGENT_BRIDGE_LISTEN_ADDR", mobileAddr)
	}

	log.Printf("[agent-bridge] Starting agent-bridge daemon...")
	log.Printf("[agent-bridge] Loading config from %s", configPath)

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("[agent-bridge] Error loading config: %v", err)
	}

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

	// Initialize Base Agent Runner
	var baseRunner runner.Runner
	if useMock {
		log.Printf("[agent-bridge] Initializing FakeRunner (mock mode)")
		fake := runner.NewFakeRunner()
		fake.OnInstruction = func(text string, r *runner.FakeRunner) {
			lower := strings.ToLower(text)
			r.EmitEvent(parser.ParsedEvent{
				Type: parser.EventThinkingChunk,
				Text: "Inspecting workspace structure, recent commits, and relevant packages before formulating execution steps.\n",
			})
			time.Sleep(180 * time.Millisecond)

			// Explore files group
			r.EmitEvent(parser.ParsedEvent{Type: parser.EventToolStart, ID: "exp-1", Text: "read README.md"})
			time.Sleep(120 * time.Millisecond)
			r.EmitEvent(parser.ParsedEvent{Type: parser.EventToolEnd, ID: "exp-1", Text: "read README.md", Ok: true, DurationMs: 120})

			r.EmitEvent(parser.ParsedEvent{Type: parser.EventToolStart, ID: "exp-2", Text: "read internal/config/config.go"})
			time.Sleep(100 * time.Millisecond)
			r.EmitEvent(parser.ParsedEvent{Type: parser.EventToolEnd, ID: "exp-2", Text: "read internal/config/config.go", Ok: true, DurationMs: 100})

			// Subagent step
			r.EmitEvent(parser.ParsedEvent{Type: parser.EventToolStart, ID: "sub-1", Text: "agent: Architecture & Test Verifier"})
			time.Sleep(240 * time.Millisecond)
			r.EmitEvent(parser.ParsedEvent{Type: parser.EventToolEnd, ID: "sub-1", Text: "agent: Architecture & Test Verifier", Ok: true, DurationMs: 240, StderrTail: "Verified 11 packages, 0 race conditions detected."})

			// Interactive approval flow if requested and auto-approve is off
			if (strings.Contains(lower, "approve") || strings.Contains(lower, "clean") || strings.Contains(lower, "rm ") || strings.Contains(lower, "deploy")) && !r.IsAutoApprove() {
				appID := fmt.Sprintf("app_%d", time.Now().UnixNano())
				prevCount, _ := r.ApprovalSnapshot()
				r.EmitEvent(parser.ParsedEvent{
					Type:    parser.EventApprovalReq,
					ID:      appID,
					Text:    "rm -rf ./build/tmp && go test -race ./...",
					Options: []string{"Allow", "Deny"},
				})
				// Wait up to 60s for user approval decision
				allowed := false
				deadline := time.Now().Add(60 * time.Second)
				for time.Now().Before(deadline) {
					if !r.IsBusy() {
						return
					}
					if curCount, lastVal := r.ApprovalSnapshot(); curCount > prevCount {
						allowed = lastVal
						break
					}
					time.Sleep(80 * time.Millisecond)
				}
				if !allowed {
					r.EmitEvent(parser.ParsedEvent{
						Type: parser.EventAssistantChunk,
						Text: "Execution skipped because permission for `rm -rf ./build/tmp` was denied.",
					})
					r.EmitEvent(parser.ParsedEvent{Type: parser.EventPromptReady})
					r.SetBusy(false)
					return
				}
			}

			r.EmitEvent(parser.ParsedEvent{Type: parser.EventToolStart, ID: "cmd-1", Text: "run: go test -race ./..."})
			time.Sleep(220 * time.Millisecond)
			r.EmitEvent(parser.ParsedEvent{Type: parser.EventToolEnd, ID: "cmd-1", Text: "run: go test -race ./...", Ok: true, DurationMs: 220, StderrTail: "ok  agent-bridge/internal/mobile  0.042s\nok  agent-bridge/internal/telegram  0.011s"})

			resp := fmt.Sprintf("### Execution Summary\n\nCompleted analysis for: **%s**\n\n| Module | Status | Latency |\n| :--- | :--- | :--- |\n| `internal/mobile` | Passed | `42ms` |\n| `internal/runner` | Passed | `19ms` |\n| `internal/db` | WAL Ready | `4ms` |\n\n```go\n// Verified mobile bridge health check\nGET /api/v1/health -> 200 OK\n```\n\nAll workspace checks are green and ready.", strings.TrimSpace(text))
			chunks := strings.SplitAfter(resp, " ")
			for _, ch := range chunks {
				r.EmitEvent(parser.ParsedEvent{Type: parser.EventAssistantChunk, Text: ch})
				time.Sleep(18 * time.Millisecond)
			}
			r.EmitEvent(parser.ParsedEvent{Type: parser.EventPromptReady, Text: resp})
			r.SetBusy(false)
		}
		baseRunner = fake
	} else if runnerType == "pty" {
		log.Printf("[agent-bridge] Initializing PTYRunner for agy")
		baseRunner = runner.NewPTYRunner()
	} else {
		log.Printf("[agent-bridge] Initializing CLIRunner for agy (reliable print/json mode)")
		baseRunner = runner.NewCLIRunner()
	}

	// Wrap with FanoutRunner so both Telegram and Mobile Server receive live events simultaneously
	agentRunner := runner.NewFanoutRunner(baseRunner)
	defer agentRunner.Stop()

	// Initialize Queue Manager
	queueMgr := queue.NewManager(database, agentRunner, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		sig := <-sigCh
		log.Printf("[agent-bridge] Received signal %s, shutting down gracefully...", sig)
		cancel()
	}()

	var wg sync.WaitGroup

	// Start Mobile Server if enabled
	if cfg.Mobile.Enabled {
		mobileSrv, err := mobile.NewServer(cfg, database, agentRunner, queueMgr)
		if err != nil {
			log.Fatalf("[agent-bridge] Error initializing mobile server: %v", err)
		}
		if !cfg.HasTelegram() {
			mobileSrv.InitOutboxWatcher()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := mobileSrv.Start(ctx); err != nil {
				log.Printf("[agent-bridge] Mobile server error: %v", err)
				cancel()
			}
		}()
	}

	// Start Telegram Bot if configured
	if cfg.HasTelegram() {
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

		bridgeBot, err := telegram.NewBridgeBot(cfg, database, agentRunner, queueAdapter)
		if err != nil {
			log.Fatalf("[agent-bridge] Error creating Telegram bot: %v", err)
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			log.Printf("[agent-bridge] Bridge is now listening for Telegram messages (long-polling)...")
			if err := bridgeBot.Start(ctx); err != nil && err != context.Canceled {
				log.Printf("[agent-bridge] Bot runtime error: %v", err)
				cancel()
			}
		}()
	}

	wg.Wait()
	log.Printf("[agent-bridge] Shutdown complete. Goodbye.")
}
