package media

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestInboxAndAlbumBatcher(t *testing.T) {
	tempProject := t.TempDir()

	fileContent := "sample file payload"
	reader := strings.NewReader(fileContent)

	saved, err := SaveInboxFile(tempProject, "my test photo.png", reader)
	if err != nil {
		t.Fatalf("save inbox file failed: %v", err)
	}

	if !strings.HasSuffix(saved.Filename, ".png") || saved.MimeType != "photo" {
		t.Errorf("expected photo type, got: %+v", saved)
	}

	if _, err := os.Stat(saved.Path); err != nil {
		t.Fatalf("saved file does not exist on disk: %v", err)
	}

	// Test album batcher
	var readyFiles []SavedFile
	var readyCaption string
	var wg sync.WaitGroup
	wg.Add(1)

	batcher := NewAlbumBatcher(50*time.Millisecond, func(caption string, files []SavedFile) {
		readyCaption = caption
		readyFiles = files
		wg.Done()
	})

	batcher.Add(*saved, "Inspect these screenshots")
	batcher.Add(*saved, "")

	wg.Wait()

	if len(readyFiles) != 2 {
		t.Errorf("expected 2 batched files, got %d", len(readyFiles))
	}
	if readyCaption != "Inspect these screenshots" {
		t.Errorf("expected caption preserved, got %q", readyCaption)
	}

	prompt := BuildPromptForFiles(readyCaption, readyFiles)
	if !strings.Contains(prompt, "Inspect these screenshots") || !strings.Contains(prompt, "Attached files:") {
		t.Errorf("unexpected prompt: %s", prompt)
	}
}

func TestValidateGetPath(t *testing.T) {
	tempProject := t.TempDir()

	// Create valid file
	validFile := filepath.Join(tempProject, "readme.txt")
	_ = os.WriteFile(validFile, []byte("hello"), 0644)

	// Create sensitive files
	envFile := filepath.Join(tempProject, ".env")
	_ = os.WriteFile(envFile, []byte("SECRET=123"), 0644)

	pemFile := filepath.Join(tempProject, "key.pem")
	_ = os.WriteFile(pemFile, []byte("CERT"), 0644)

	gitDir := filepath.Join(tempProject, ".git")
	_ = os.MkdirAll(gitDir, 0755)
	gitConfig := filepath.Join(gitDir, "config")
	_ = os.WriteFile(gitConfig, []byte("[core]"), 0644)

	// 1. Valid file succeeds
	resolved, err := ValidateGetPath(tempProject, "readme.txt", 1024*1024)
	if err != nil || resolved != validFile {
		t.Errorf("expected valid file, got: %s, err: %v", resolved, err)
	}

	// 2. Traversal fails
	_, err = ValidateGetPath(tempProject, "../etc/passwd", 1024*1024)
	if err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Errorf("expected path escape error, got: %v", err)
	}

	// 3. .env denied
	_, err = ValidateGetPath(tempProject, ".env", 1024*1024)
	if err == nil || !strings.Contains(err.Error(), "deny-list") {
		t.Errorf("expected deny-list error for .env, got: %v", err)
	}

	// 4. .pem denied
	_, err = ValidateGetPath(tempProject, "key.pem", 1024*1024)
	if err == nil || !strings.Contains(err.Error(), "deny-list") {
		t.Errorf("expected deny-list error for .pem, got: %v", err)
	}

	// 5. .git config denied
	_, err = ValidateGetPath(tempProject, ".git/config", 1024*1024)
	if err == nil || !strings.Contains(err.Error(), ".git folder is protected") {
		t.Errorf("expected .git protected error, got: %v", err)
	}
}

func TestOutboxWatcher(t *testing.T) {
	tempProject := t.TempDir()

	var sentPath string
	var sentCategory FileCategory
	var wg sync.WaitGroup
	wg.Add(1)

	watcher, err := NewOutboxWatcher(tempProject, 50*time.Millisecond, func(path string, cat FileCategory) error {
		sentPath = path
		sentCategory = cat
		wg.Done()
		return nil
	})
	if err != nil {
		t.Fatalf("new outbox watcher failed: %v", err)
	}
	defer watcher.Stop()

	// Write file into outbox
	testFile := filepath.Join(tempProject, ".bridge", "outbox", "report.txt")
	if err := os.WriteFile(testFile, []byte("all tests pass"), 0644); err != nil {
		t.Fatalf("failed to write outbox test file: %v", err)
	}

	wg.Wait()

	if sentPath != testFile || sentCategory != CategoryDocument {
		t.Errorf("expected document sent, got: path=%s cat=%s", sentPath, sentCategory)
	}

	// File should be moved to sent/
	time.Sleep(20 * time.Millisecond)
	if _, err := os.Stat(testFile); !os.IsNotExist(err) {
		t.Errorf("expected outbox file to be moved out of outbox, but it still exists")
	}

	sentFile := filepath.Join(tempProject, ".bridge", "sent", "report.txt")
	if _, err := os.Stat(sentFile); err != nil {
		t.Errorf("expected file to exist in sent directory: %v", err)
	}
}

func TestPruneInbox(t *testing.T) {
	tempProject := t.TempDir()

	// Old folder (10 days old)
	oldDir := filepath.Join(tempProject, ".bridge", "inbox", "2020-01-01")
	_ = os.MkdirAll(oldDir, 0755)
	_ = os.WriteFile(filepath.Join(oldDir, "old.txt"), []byte("old file"), 0644)

	// Recent folder
	todayDir := filepath.Join(tempProject, ".bridge", "inbox", time.Now().Format("2006-01-02"))
	_ = os.MkdirAll(todayDir, 0755)
	_ = os.WriteFile(filepath.Join(todayDir, "new.txt"), []byte("new file"), 0644)

	pruned, err := PruneInbox(tempProject, 7)
	if err != nil {
		t.Fatalf("PruneInbox error: %v", err)
	}

	if pruned < 1 {
		t.Errorf("expected at least 1 pruned entry, got %d", pruned)
	}

	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Errorf("expected oldDir to be deleted, but still exists")
	}

	if _, err := os.Stat(todayDir); err != nil {
		t.Errorf("expected todayDir to remain, got err: %v", err)
	}
}

