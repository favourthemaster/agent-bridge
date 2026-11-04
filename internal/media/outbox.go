package media

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

type FileCategory string

const (
	CategoryPhoto    FileCategory = "photo"
	CategoryAudio    FileCategory = "audio"
	CategoryVideo    FileCategory = "video"
	CategoryDocument FileCategory = "document"
)

func DetectFileType(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".webp", ".bmp":
		return string(CategoryPhoto)
	case ".ogg", ".opus", ".mp3", ".wav", ".m4a", ".aac":
		return string(CategoryAudio)
	case ".mp4", ".mov", ".mkv", ".webm":
		return string(CategoryVideo)
	default:
		return string(CategoryDocument)
	}
}

type OutboxWatcher struct {
	projectDir string
	outboxDir  string
	sentDir    string
	watcher    *fsnotify.Watcher
	debounce   time.Duration
	onSend     func(path string, category FileCategory) error
	timers     map[string]*time.Timer
	stopCh     chan struct{}
	mu         sync.Mutex
}

func NewOutboxWatcher(projectDir string, debounce time.Duration, onSend func(path string, category FileCategory) error) (*OutboxWatcher, error) {
	if debounce <= 0 {
		debounce = 2 * time.Second
	}

	outboxDir := filepath.Join(projectDir, ".bridge", "outbox")
	sentDir := filepath.Join(projectDir, ".bridge", "sent")

	if err := os.MkdirAll(outboxDir, 0755); err != nil {
		return nil, fmt.Errorf("creating outbox directory: %w", err)
	}
	if err := os.MkdirAll(sentDir, 0755); err != nil {
		return nil, fmt.Errorf("creating sent directory: %w", err)
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("creating fsnotify watcher: %w", err)
	}

	ow := &OutboxWatcher{
		projectDir: projectDir,
		outboxDir:  outboxDir,
		sentDir:    sentDir,
		watcher:    w,
		debounce:   debounce,
		onSend:     onSend,
		timers:     make(map[string]*time.Timer),
		stopCh:     make(chan struct{}),
	}

	if err := w.Add(outboxDir); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("watching outbox dir: %w", err)
	}

	go ow.watchLoop()

	return ow, nil
}

func (ow *OutboxWatcher) watchLoop() {
	for {
		select {
		case <-ow.stopCh:
			return
		case event, ok := <-ow.watcher.Events:
			if !ok {
				return
			}
			if event.Op&(fsnotify.Create|fsnotify.Write) != 0 {
				ow.scheduleSend(event.Name)
			}
		case err, ok := <-ow.watcher.Errors:
			if !ok {
				return
			}
			_ = err // log or ignore
		}
	}
}

func (ow *OutboxWatcher) scheduleSend(path string) {
	ow.mu.Lock()
	defer ow.mu.Unlock()

	// If timer already exists for this file, reset it
	if t, exists := ow.timers[path]; exists {
		t.Stop()
	}

	ow.timers[path] = time.AfterFunc(ow.debounce, func() {
		ow.processFile(path)
	})
}

func (ow *OutboxWatcher) processFile(path string) {
	ow.mu.Lock()
	delete(ow.timers, path)
	ow.mu.Unlock()

	// Check if file still exists and has stopped growing
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() || fi.Size() == 0 {
		return
	}

	filename := filepath.Base(path)
	category := FileCategory(DetectFileType(filename))

	// Send to Telegram
	if ow.onSend != nil {
		if err := ow.onSend(path, category); err != nil {
			// On error, leave in outbox for retry
			return
		}
	}

	// Move to sent directory
	destPath := filepath.Join(ow.sentDir, filename)
	// If file exists at dest, add timestamp suffix
	if _, err := os.Stat(destPath); err == nil {
		ext := filepath.Ext(filename)
		base := strings.TrimSuffix(filename, ext)
		destPath = filepath.Join(ow.sentDir, fmt.Sprintf("%s_%d%s", base, time.Now().UnixNano(), ext))
	}
	_ = os.Rename(path, destPath)
}

func (ow *OutboxWatcher) Stop() error {
	close(ow.stopCh)
	ow.mu.Lock()
	for _, t := range ow.timers {
		t.Stop()
	}
	ow.timers = nil
	ow.mu.Unlock()
	return ow.watcher.Close()
}
