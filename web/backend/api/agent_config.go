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

type agentConfigFileInfo struct {
	Name       string    `json:"name"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modified_at"`
}

type agentConfigFileContent struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

func (h *Handler) registerAgentConfigRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/agent/config/files", h.handleListAgentConfigFiles)
	mux.HandleFunc("POST /api/agent/config/files", h.handleCreateAgentConfigFile)
	mux.HandleFunc("GET /api/agent/config/files/{name}", h.handleGetAgentConfigFile)
	mux.HandleFunc("PUT /api/agent/config/files/{name}", h.handleSaveAgentConfigFile)
	mux.HandleFunc("DELETE /api/agent/config/files/{name}", h.handleDeleteAgentConfigFile)
}

func (h *Handler) agentConfigWorkspace() (string, error) {
	cfg, err := config.LoadConfig(h.configPath)
	if err != nil {
		return "", fmt.Errorf("failed to load config: %w", err)
	}
	return cfg.WorkspacePath(), nil
}

func validateAgentConfigFileName(name string) error {
	if name == "" {
		return fmt.Errorf("file name is required")
	}
	if !strings.HasSuffix(name, ".md") {
		return fmt.Errorf("only .md files are allowed")
	}
	if strings.Contains(name, "/") || strings.Contains(name, "\\") || strings.Contains(name, "..") {
		return fmt.Errorf("invalid file name")
	}
	return nil
}

func (h *Handler) handleListAgentConfigFiles(w http.ResponseWriter, r *http.Request) {
	workspace, err := h.agentConfigWorkspace()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	entries, err := os.ReadDir(workspace)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to read workspace: %v", err), http.StatusInternalServerError)
		return
	}

	files := []agentConfigFileInfo{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files = append(files, agentConfigFileInfo{
			Name:       entry.Name(),
			Size:       info.Size(),
			ModifiedAt: info.ModTime(),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(files)
}

func (h *Handler) handleGetAgentConfigFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := validateAgentConfigFileName(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	workspace, err := h.agentConfigWorkspace()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	path := filepath.Join(workspace, name)
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "file not found", http.StatusNotFound)
			return
		}
		http.Error(w, fmt.Sprintf("failed to read file: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(agentConfigFileContent{
		Name:    name,
		Content: string(content),
	})
}

func (h *Handler) handleSaveAgentConfigFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := validateAgentConfigFileName(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	workspace, err := h.agentConfigWorkspace()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var body agentConfigFileContent
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}

	path := filepath.Join(workspace, name)
	if err := os.WriteFile(path, []byte(body.Content), 0o644); err != nil {
		http.Error(w, fmt.Sprintf("failed to write file: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (h *Handler) handleCreateAgentConfigFile(w http.ResponseWriter, r *http.Request) {
	workspace, err := h.agentConfigWorkspace()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var body agentConfigFileContent
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}

	if err := validateAgentConfigFileName(body.Name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	path := filepath.Join(workspace, body.Name)
	if _, err := os.Stat(path); err == nil {
		http.Error(w, "file already exists", http.StatusConflict)
		return
	}

	if err := os.WriteFile(path, []byte(body.Content), 0o644); err != nil {
		http.Error(w, fmt.Sprintf("failed to create file: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"name": body.Name})
}

func (h *Handler) handleDeleteAgentConfigFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := validateAgentConfigFileName(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	workspace, err := h.agentConfigWorkspace()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	path := filepath.Join(workspace, name)
	if err := os.Remove(path); err != nil {
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
