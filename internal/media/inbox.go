package media

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var sanitizeRegex = regexp.MustCompile(`[^a-zA-Z0-9_\.\-]`)

type SavedFile struct {
	Path     string
	Filename string
	MimeType string
	Size     int64
	IsVoice  bool
}

// SaveInboxFile writes an incoming file to <projectDir>/.bridge/inbox/<YYYY-MM-DD>/<filename>.
func SaveInboxFile(projectDir, rawFilename string, r io.Reader) (*SavedFile, error) {
	cleanName := filepath.Base(rawFilename)
	cleanName = sanitizeRegex.ReplaceAllString(cleanName, "_")
	if cleanName == "" || cleanName == "." {
		cleanName = fmt.Sprintf("file_%d", time.Now().UnixNano())
	}

	dateDir := time.Now().Format("2006-01-02")
	targetDir := filepath.Join(projectDir, ".bridge", "inbox", dateDir)
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return nil, fmt.Errorf("creating inbox directory: %w", err)
	}

	targetPath := filepath.Join(targetDir, cleanName)
	f, err := os.Create(targetPath)
	if err != nil {
		return nil, fmt.Errorf("creating file %q: %w", targetPath, err)
	}
	defer f.Close()

	n, err := io.Copy(f, r)
	if err != nil {
		return nil, fmt.Errorf("writing file %q: %w", targetPath, err)
	}

	mimeType := DetectFileType(cleanName)

	return &SavedFile{
		Path:     targetPath,
		Filename: cleanName,
		MimeType: mimeType,
		Size:     n,
	}, nil
}

// AlbumBatcher collects files arriving within a 2-second window from Telegram media albums.
type AlbumBatcher struct {
	debounce time.Duration
	onReady  func(caption string, files []SavedFile)
	timer    *time.Timer
	caption  string
	files    []SavedFile
	mu       sync.Mutex
}

func NewAlbumBatcher(debounce time.Duration, onReady func(caption string, files []SavedFile)) *AlbumBatcher {
	if debounce <= 0 {
		debounce = 2 * time.Second
	}
	return &AlbumBatcher{
		debounce: debounce,
		onReady:  onReady,
		files:    make([]SavedFile, 0),
	}
}

func (b *AlbumBatcher) Add(file SavedFile, caption string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.files = append(b.files, file)
	if caption != "" && b.caption == "" {
		b.caption = caption
	}

	if b.timer != nil {
		b.timer.Stop()
	}

	b.timer = time.AfterFunc(b.debounce, func() {
		b.mu.Lock()
		files := b.files
		capText := b.caption
		b.files = nil
		b.caption = ""
		b.mu.Unlock()

		if b.onReady != nil && len(files) > 0 {
			b.onReady(capText, files)
		}
	})
}

// BuildPromptForFiles creates a clean instruction for agy referencing received files.
func BuildPromptForFiles(caption string, files []SavedFile) string {
	var sb strings.Builder

	// If this is a single voice message from the user, treat it as direct conversational input
	if len(files) == 1 && (files[0].IsVoice || strings.HasPrefix(files[0].Filename, "voice_note")) {
		if strings.TrimSpace(caption) != "" {
			sb.WriteString(caption + "\n\n")
		}
		sb.WriteString("The user sent a voice message. Listen to the audio file and respond directly to what the user said as a normal conversational turn (or execute whatever instruction the user spoke). Do not describe the file format or ask what to do with the file; respond directly to the spoken message.\n\n")
		sb.WriteString(fmt.Sprintf("Audio file:\n- Path: %s\n", files[0].Path))
		return strings.TrimRight(sb.String(), "\n")
	}

	if strings.TrimSpace(caption) != "" {
		sb.WriteString(caption + "\n\n")
	} else {
		sb.WriteString("Describe what this is and ask what I want done with it.\n\n")
	}

	sb.WriteString("Attached files:\n")
	for _, f := range files {
		sb.WriteString(fmt.Sprintf("- Path: %s (Type: %s, Size: %d bytes)\n", f.Path, f.MimeType, f.Size))
	}

	return strings.TrimRight(sb.String(), "\n")
}

// PruneInbox removes files and directories from <projectDir>/.bridge/inbox older than retentionDays.
func PruneInbox(projectDir string, retentionDays int) (int, error) {
	if retentionDays <= 0 {
		retentionDays = 7
	}
	cutoff := time.Now().AddDate(0, 0, -retentionDays)
	inboxDir := filepath.Join(projectDir, ".bridge", "inbox")
	if _, err := os.Stat(inboxDir); os.IsNotExist(err) {
		return 0, nil
	}

	prunedCount := 0
	entries, err := os.ReadDir(inboxDir)
	if err != nil {
		return 0, err
	}

	for _, entry := range entries {
		entryPath := filepath.Join(inboxDir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}

		// Try parsing date folder name "YYYY-MM-DD"
		if entry.IsDir() {
			if folderDate, err := time.Parse("2006-01-02", entry.Name()); err == nil {
				if folderDate.Before(cutoff) {
					if os.RemoveAll(entryPath) == nil {
						prunedCount++
					}
					continue
				}
			}
		}

		// Fallback to ModTime check
		if info.ModTime().Before(cutoff) {
			if entry.IsDir() {
				if os.RemoveAll(entryPath) == nil {
					prunedCount++
				}
			} else {
				if os.Remove(entryPath) == nil {
					prunedCount++
				}
			}
		}
	}

	return prunedCount, nil
}

