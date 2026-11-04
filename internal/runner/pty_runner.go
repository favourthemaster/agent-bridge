package runner

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"agent-bridge/internal/parser"

	"github.com/creack/pty"
)

type PTYRunner struct {
	cmd       *exec.Cmd
	ptmx      *os.File
	parser    *parser.StreamParser
	eventCb   func(e parser.ParsedEvent)
	isBusy    bool
	closed    bool
	cwd       string
	model     string
	convID      string
	autoApprove bool
	restartCh   chan struct{}
	mu          sync.Mutex
}

func NewPTYRunner() *PTYRunner {
	return &PTYRunner{
		parser:    parser.NewStreamParser(),
		restartCh: make(chan struct{}, 1),
	}
}

func (r *PTYRunner) OnEvent(cb func(e parser.ParsedEvent)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.eventCb = cb
}

func (r *PTYRunner) IsBusy() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.isBusy
}

func (r *PTYRunner) SetAutoApprove(enabled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.autoApprove = enabled
}

func (r *PTYRunner) IsAutoApprove() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.autoApprove
}

func (r *PTYRunner) Start(cwd, model, conversationID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.cwd = cwd
	r.model = model
	r.convID = conversationID
	r.closed = false

	return r.spawnProcess()
}

func (r *PTYRunner) spawnProcess() error {
	args := []string{}
	if r.model != "" {
		args = append(args, "--model", r.model)
	}
	if r.convID != "" {
		args = append(args, "--conversation", r.convID)
	}
	if r.autoApprove {
		args = append(args, "--dangerously-skip-permissions")
	}

	cmd := exec.Command("agy", args...)
	if r.cwd != "" {
		cmd.Dir = r.cwd
	}

	// Forward environment variables
	cmd.Env = os.Environ()

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return fmt.Errorf("starting agy in pty: %w", err)
	}

	// Set a reasonable terminal size (120 cols x 40 rows)
	_ = pty.Setsize(ptmx, &pty.Winsize{
		Rows: 40,
		Cols: 120,
	})

	r.cmd = cmd
	r.ptmx = ptmx

	// Start supervisor reader goroutine
	go r.readLoop(ptmx, cmd)

	return nil
}

func (r *PTYRunner) readLoop(ptmx *os.File, cmd *exec.Cmd) {
	buf := make([]byte, 4096)
	for {
		n, err := ptmx.Read(buf)
		if n > 0 {
			chunk := string(buf[:n])
			events := r.parser.FeedPTYChunk(chunk)

			r.mu.Lock()
			cb := r.eventCb
			for _, ev := range events {
				if ev.Type == parser.EventToolStart || ev.Type == parser.EventThinkingChunk {
					r.isBusy = true
				} else if ev.Type == parser.EventPromptReady {
					r.isBusy = false
				}
			}
			r.mu.Unlock()

			if cb != nil {
				for _, ev := range events {
					cb(ev)
				}
			}
		}

		if err != nil {
			if err != io.EOF {
				// PTY closed / process ended
			}
			break
		}
	}

	_ = cmd.Wait()

	r.mu.Lock()
	closed := r.closed
	cb := r.eventCb
	r.mu.Unlock()

	// If not intentionally closed by Stop(), automatically respawn (always running)
	if !closed {
		if cb != nil {
			cb(parser.ParsedEvent{
				Type: parser.EventError,
				Text: "agy runner exited unexpectedly; supervisor respawning...",
			})
		}
		time.Sleep(1 * time.Second)
		r.mu.Lock()
		if !r.closed {
			_ = r.spawnProcess()
		}
		r.mu.Unlock()
	}
}

func (r *PTYRunner) WriteInstruction(text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.ptmx == nil {
		return fmt.Errorf("pty runner is not running")
	}

	r.isBusy = true
	r.parser.ResetTurn()

	// Send text + carriage return to the terminal
	payload := text + "\r"
	_, err := r.ptmx.Write([]byte(payload))
	return err
}

func (r *PTYRunner) SendApprovalResponse(allowed bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.ptmx == nil {
		return fmt.Errorf("pty runner is not running")
	}

	resp := "2\r"
	if allowed {
		resp = "1\r"
	}
	_, err := r.ptmx.Write([]byte(resp))
	return err
}

func (r *PTYRunner) Interrupt() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.ptmx == nil {
		return fmt.Errorf("pty runner is not running")
	}

	// Send Ctrl+C
	_, err := r.ptmx.Write([]byte{0x03})
	r.isBusy = false
	return err
}

func (r *PTYRunner) Stop() error {
	r.mu.Lock()
	r.closed = true
	ptmx := r.ptmx
	cmd := r.cmd
	r.mu.Unlock()

	if ptmx != nil {
		_ = ptmx.Close()
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	return nil
}
