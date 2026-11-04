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
