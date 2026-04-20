package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

type agentMemoryFileInfo struct {
	Name       string    `json:"name"`
	Path       string    `json:"path"` // relative to memory dir, e.g. "MEMORY.md" or "202603/20260316.md"
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modified_at"`
}

type agentMemoryFileContent struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (h *Handler) registerAgentMemoryRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/agent/memory/size", h.handleMemorySize)
	mux.HandleFunc("GET /api/agent/memory/files", h.handleListMemoryFiles)
	mux.HandleFunc("POST /api/agent/memory/files", h.handleCreateMemoryFile)
	mux.HandleFunc("GET /api/agent/memory/files/{path...}", h.handleGetMemoryFile)
	mux.HandleFunc("PUT /api/agent/memory/files/{path...}", h.handleSaveMemoryFile)
	mux.HandleFunc("DELETE /api/agent/memory/files/{path...}", h.handleDeleteMemoryFile)
}

func (h *Handler) memoryDir() (string, error) {
	cfg, err := config.LoadConfig(h.configPath)
	if err != nil {
		return "", fmt.Errorf("failed to load config: %w", err)
	}
	return filepath.Join(cfg.WorkspacePath(), "memory"), nil
}

// validateMemoryPath ensures the path is safe: only .md files, max one level deep, no traversal.
func validateMemoryPath(path string) error {
	if path == "" {
		return fmt.Errorf("path is required")
	}
	parts := strings.Split(path, "/")
	switch len(parts) {
	case 1:
		// root-level file: must be .md
		if !strings.HasSuffix(parts[0], ".md") {
			return fmt.Errorf("only .md files are allowed")
		}
	case 2:
		// one level deep: dir/file.md
		if !strings.HasSuffix(parts[1], ".md") {
			return fmt.Errorf("only .md files are allowed")
		}
	default:
		return fmt.Errorf("path too deep: only root or one subdirectory allowed")
	}
	for _, part := range parts {
		if part == ".." || part == "." || part == "" {
			return fmt.Errorf("invalid path component: %q", part)
		}
	}
	return nil
}

func (h *Handler) handleListMemoryFiles(w http.ResponseWriter, r *http.Request) {
	memDir, err := h.memoryDir()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := os.MkdirAll(memDir, 0o755); err != nil {
		http.Error(w, fmt.Sprintf("failed to create memory dir: %v", err), http.StatusInternalServerError)
		return
	}

	entries, err := os.ReadDir(memDir)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to read memory dir: %v", err), http.StatusInternalServerError)
		return
	}

	var files []agentMemoryFileInfo
	for _, entry := range entries {
		if entry.IsDir() {
			// Recurse one level (YYYYMM subdirs)
			subDir := filepath.Join(memDir, entry.Name())
			subEntries, err := os.ReadDir(subDir)
			if err != nil {
				continue
			}
			for _, sub := range subEntries {
				if sub.IsDir() || !strings.HasSuffix(sub.Name(), ".md") {
					continue
				}
				info, err := sub.Info()
				if err != nil {
					continue
				}
				files = append(files, agentMemoryFileInfo{
					Name:       sub.Name(),
					Path:       entry.Name() + "/" + sub.Name(),
					Size:       info.Size(),
					ModifiedAt: info.ModTime(),
				})
			}
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files = append(files, agentMemoryFileInfo{
			Name:       entry.Name(),
			Path:       entry.Name(),
			Size:       info.Size(),
			ModifiedAt: info.ModTime(),
		})
	}
	if files == nil {
		files = []agentMemoryFileInfo{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(files)
}

func (h *Handler) handleGetMemoryFile(w http.ResponseWriter, r *http.Request) {
	relPath := r.PathValue("path")
	if err := validateMemoryPath(relPath); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	memDir, err := h.memoryDir()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	fullPath := filepath.Join(memDir, filepath.FromSlash(relPath))
	content, err := os.ReadFile(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "file not found", http.StatusNotFound)
			return
		}
		http.Error(w, fmt.Sprintf("failed to read file: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(agentMemoryFileContent{
		Path:    relPath,
		Content: string(content),
	})
}

func (h *Handler) handleSaveMemoryFile(w http.ResponseWriter, r *http.Request) {
	relPath := r.PathValue("path")
	if err := validateMemoryPath(relPath); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	memDir, err := h.memoryDir()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var body agentMemoryFileContent
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}

	fullPath := filepath.Join(memDir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		http.Error(w, fmt.Sprintf("failed to create directory: %v", err), http.StatusInternalServerError)
		return
	}
	if err := os.WriteFile(fullPath, []byte(body.Content), 0o644); err != nil {
		http.Error(w, fmt.Sprintf("failed to write file: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (h *Handler) handleCreateMemoryFile(w http.ResponseWriter, r *http.Request) {
	memDir, err := h.memoryDir()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var body agentMemoryFileContent
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}

	if err := validateMemoryPath(body.Path); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	fullPath := filepath.Join(memDir, filepath.FromSlash(body.Path))
	if _, err := os.Stat(fullPath); err == nil {
		http.Error(w, "file already exists", http.StatusConflict)
		return
	}

	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		http.Error(w, fmt.Sprintf("failed to create directory: %v", err), http.StatusInternalServerError)
		return
	}
	if err := os.WriteFile(fullPath, []byte(body.Content), 0o644); err != nil {
		http.Error(w, fmt.Sprintf("failed to create file: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"path": body.Path})
}

type agentMemorySizeInfo struct {
	GeneralBytes  int64 `json:"general_bytes"`
	SnapshotBytes int64 `json:"snapshot_bytes"`
	TotalBytes    int64 `json:"total_bytes"`
}

func (h *Handler) handleMemorySize(w http.ResponseWriter, r *http.Request) {
	memDir, err := h.memoryDir()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	snapshotsDir := filepath.Join(memDir, "snapshots")

	var generalBytes int64
	filepath.WalkDir(memDir, func(path string, d os.DirEntry, err error) error { //nolint:errcheck
		if err != nil {
			return nil
		}
		// Skip the snapshots subdirectory entirely
		if d.IsDir() && path == snapshotsDir {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			if info, err := d.Info(); err == nil {
				generalBytes += info.Size()
			}
		}
		return nil
	})

	var snapshotBytes int64
	filepath.WalkDir(snapshotsDir, func(path string, d os.DirEntry, err error) error { //nolint:errcheck
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			if info, err := d.Info(); err == nil {
				snapshotBytes += info.Size()
			}
		}
		return nil
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(agentMemorySizeInfo{
		GeneralBytes:  generalBytes,
		SnapshotBytes: snapshotBytes,
		TotalBytes:    generalBytes + snapshotBytes,
	})
}

func (h *Handler) handleDeleteMemoryFile(w http.ResponseWriter, r *http.Request) {
	relPath := r.PathValue("path")
	if err := validateMemoryPath(relPath); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	memDir, err := h.memoryDir()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	fullPath := filepath.Join(memDir, filepath.FromSlash(relPath))
	if err := os.Remove(fullPath); err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "file not found", http.StatusNotFound)
			return
		}
		http.Error(w, fmt.Sprintf("failed to delete file: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
