package mobile

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type ArtifactDoc struct {
	ID        string `json:"id"`
	Tab       string `json:"tab"` // overview, walkthrough, plan, task, outbox
	Title     string `json:"title"`
	Filename  string `json:"filename"`
	RelPath   string `json:"rel_path,omitempty"`
	Category  string `json:"category"` // markdown, image, code, binary
	Content   string `json:"content,omitempty"`
	SizeBytes int64  `json:"size_bytes"`
	UpdatedAt int64  `json:"updated_at"`
}

type FileEntry struct {
	Name      string `json:"name"`
	RelPath   string `json:"rel_path"`
	IsDir     bool   `json:"is_dir"`
	SizeBytes int64  `json:"size_bytes"`
	UpdatedAt int64  `json:"updated_at"`
}

func CollectArtifacts(projectDir, convID string) []ArtifactDoc {
	var docs []ArtifactDoc
	seen := make(map[string]bool)

	addFile := func(absPath, relPath, forcedTab, forcedTitle string) {
		if seen[absPath] {
			return
		}
		fi, err := os.Stat(absPath)
		if err != nil || fi.IsDir() || fi.Size() == 0 {
			return
		}
		seen[absPath] = true

		ext := strings.ToLower(filepath.Ext(absPath))
		cat := "binary"
		switch ext {
		case ".md", ".txt":
			cat = "markdown"
		case ".png", ".jpg", ".jpeg", ".webp", ".gif":
			cat = "image"
		case ".go", ".dart", ".ts", ".tsx", ".js", ".py", ".yaml", ".yml", ".json", ".sh":
			cat = "code"
		}

		base := filepath.Base(absPath)
		lower := strings.ToLower(base)
		tab := forcedTab
		if tab == "" {
			switch {
			case strings.Contains(lower, "walkthrough"):
				tab = "walkthrough"
			case strings.Contains(lower, "plan") || strings.Contains(lower, "architecture") || strings.Contains(lower, "decision"):
				tab = "plan"
			case strings.Contains(lower, "task") || strings.Contains(lower, "todo"):
				tab = "task"
			case strings.Contains(lower, "readme") || strings.Contains(lower, "overview"):
				tab = "overview"
			default:
				tab = "outbox"
			}
		}

		title := forcedTitle
		if title == "" {
			title = base
		}

		content := ""
		if (cat == "markdown" || cat == "code") && fi.Size() <= 128*1024 {
			if b, err := os.ReadFile(absPath); err == nil {
				content = string(b)
			}
		}

		docs = append(docs, ArtifactDoc{
			ID:        tab + ":" + base,
			Tab:       tab,
			Title:     title,
			Filename:  base,
			RelPath:   relPath,
			Category:  cat,
			Content:   content,
			SizeBytes: fi.Size(),
			UpdatedAt: fi.ModTime().Unix(),
		})
	}

	// 1. Antigravity brain directory for active conversation
	if convID != "" {
		if home, err := os.UserHomeDir(); err == nil {
			brainDir := filepath.Join(home, ".gemini", "antigravity-cli", "brain", convID)
			if entries, err := os.ReadDir(brainDir); err == nil {
				for _, e := range entries {
					if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
						addFile(filepath.Join(brainDir, e.Name()), "", "", "")
					}
				}
			}
		}
	}

	// 2. Project .bridge/artifacts and .bridge/sent (outbox deliverables)
	if projectDir != "" {
		artDir := filepath.Join(projectDir, ".bridge", "artifacts")
		if entries, err := os.ReadDir(artDir); err == nil {
			for _, e := range entries {
				if !e.IsDir() {
					rel := filepath.ToSlash(filepath.Join(".bridge", "artifacts", e.Name()))
					addFile(filepath.Join(artDir, e.Name()), rel, "", "")
				}
			}
		}

		sentDir := filepath.Join(projectDir, ".bridge", "sent")
		if entries, err := os.ReadDir(sentDir); err == nil {
			for _, e := range entries {
				if !e.IsDir() {
					rel := filepath.ToSlash(filepath.Join(".bridge", "sent", e.Name()))
					addFile(filepath.Join(sentDir, e.Name()), rel, "outbox", "")
				}
			}
		}

		// 3. Standard workspace docs (README, docs/architecture.md, docs/decisions.md)
		readmePath := filepath.Join(projectDir, "README.md")
		addFile(readmePath, "README.md", "overview", "Project Overview")

		docsDir := filepath.Join(projectDir, "docs")
		if entries, err := os.ReadDir(docsDir); err == nil {
			for _, e := range entries {
				if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
					rel := filepath.ToSlash(filepath.Join("docs", e.Name()))
					addFile(filepath.Join(docsDir, e.Name()), rel, "", "")
				}
			}
		}
	}

	sort.Slice(docs, func(i, j int) bool {
		return docs[i].UpdatedAt > docs[j].UpdatedAt
	})
	return docs
}

var skippedDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	".dart_tool":   true,
	"build":        true,
	".gradle":      true,
	".idea":        true,
}

func ListWorkspaceFiles(projectDir, relDir, query string, maxItems int) ([]FileEntry, error) {
	if maxItems <= 0 {
		maxItems = 120
	}
	cleanBase, err := filepath.Abs(projectDir)
	if err != nil {
		return nil, err
	}

	// Search mode for @file mentions
	if strings.TrimSpace(query) != "" {
		q := strings.ToLower(strings.TrimSpace(query))
		var matches []FileEntry
		_ = filepath.WalkDir(cleanBase, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if skippedDirs[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			if len(matches) >= maxItems {
				return filepath.SkipAll
			}
			rel, relErr := filepath.Rel(cleanBase, path)
			if relErr != nil || strings.HasPrefix(rel, "..") {
				return nil
			}
			relSlash := filepath.ToSlash(rel)
			if strings.HasPrefix(filepath.Base(relSlash), ".env") {
				return nil
			}
			if strings.Contains(strings.ToLower(relSlash), q) {
				fi, _ := d.Info()
				var sz, mod int64
				if fi != nil {
					sz = fi.Size()
					mod = fi.ModTime().Unix()
				}
				matches = append(matches, FileEntry{
					Name:      d.Name(),
					RelPath:   relSlash,
					IsDir:     false,
					SizeBytes: sz,
					UpdatedAt: mod,
				})
			}
			return nil
		})
		return matches, nil
	}

	target := cleanBase
	if strings.TrimSpace(relDir) != "" && relDir != "." && relDir != "/" {
		target = filepath.Join(cleanBase, relDir)
	}
	cleanTarget, err := filepath.Abs(target)
	if err != nil {
		return nil, err
	}
	relCheck, err := filepath.Rel(cleanBase, cleanTarget)
	if err != nil || strings.HasPrefix(relCheck, "..") {
		return nil, os.ErrPermission
	}

	entries, err := os.ReadDir(cleanTarget)
	if err != nil {
		return nil, err
	}

	var list []FileEntry
	for _, e := range entries {
		name := e.Name()
		if skippedDirs[name] || strings.HasPrefix(name, ".env") {
			continue
		}
		absItem := filepath.Join(cleanTarget, name)
		relItem, _ := filepath.Rel(cleanBase, absItem)
		fi, _ := e.Info()
		var sz, mod int64
		if fi != nil {
			sz = fi.Size()
			mod = fi.ModTime().Unix()
		}
		list = append(list, FileEntry{
			Name:      name,
			RelPath:   filepath.ToSlash(relItem),
			IsDir:     e.IsDir(),
			SizeBytes: sz,
			UpdatedAt: mod,
		})
		if len(list) >= maxItems {
			break
		}
	}

	sort.Slice(list, func(i, j int) bool {
		if list[i].IsDir != list[j].IsDir {
			return list[i].IsDir
		}
		return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name)
	})
	return list, nil
}
