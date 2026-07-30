package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ─────────────────────────────────────────────
// Data models
// ─────────────────────────────────────────────

type SyncPayload struct {
	Platform  string `json:"platform"`  // "ios" or "android"
	Timestamp int64  `json:"timestamp"` // unix millis
	Size      int64  `json:"size"`
}

type AttachmentMeta struct {
	ID          string `json:"id"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
}

type BackupMeta struct {
	ID          string `json:"id"`
	Timestamp   int64  `json:"timestamp"`
	Size        int64  `json:"size"`
	LetterCount int    `json:"letter_count"`
}

type StatusResponse struct {
	Syncs       []SyncPayload    `json:"syncs"`
	Attachments []AttachmentMeta `json:"attachments"`
	Backups     []BackupMeta     `json:"backups"`
}

// ─────────────────────────────────────────────
// Server state
// ─────────────────────────────────────────────

var (
	dataDir  string
	apiKeys  map[string]string
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	dataDir = os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "/data"
	}
	loadAPIKeys()

	os.MkdirAll(filepath.Join(dataDir, "sync"), 0755)
	os.MkdirAll(filepath.Join(dataDir, "attachments"), 0755)
	os.MkdirAll(filepath.Join(dataDir, "backup"), 0755)

	mux := http.NewServeMux()
	mux.HandleFunc("/status", auth(handleStatus))
	mux.HandleFunc("/sync/push/", auth(handleSyncPush))
	mux.HandleFunc("/sync/pull/", auth(handleSyncPull))
	mux.HandleFunc("/sync/list", auth(handleSyncList))
	mux.HandleFunc("/attachment/upload", auth(handleAttachmentUpload))
	mux.HandleFunc("/attachment/download/", auth(handleAttachmentDownload))
	mux.HandleFunc("/backup/push", auth(handleBackupPush))
	mux.HandleFunc("/backup/pull/", auth(handleBackupPull))
	mux.HandleFunc("/backup/list", auth(handleBackupList))

	log.Printf("[letters2my-sync] listening on :%s …", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

// ─────────────────────────────────────────────
// Auth middleware
// ─────────────────────────────────────────────

func loadAPIKeys() {
	apiKeys = map[string]string{}
	data, err := os.ReadFile("/etc/letters2my/api_keys.txt")
	if err != nil {
		log.Printf("WARN: no api_keys.txt — using default password: letters2my")
		apiKeys["default"] = sha256hex([]byte("letters2my"))
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		name := parts[0]
		token := parts[1]
		apiKeys[name] = sha256hex([]byte(token))
	}
}

func auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		hashed := sha256hex([]byte(key))
		valid := false
		for _, v := range apiKeys {
			if subtle.ConstantTimeCompare([]byte(hashed), []byte(v)) == 1 {
				valid = true
				break
			}
		}
		if !valid {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// ─────────────────────────────────────────────
// Status
// ─────────────────────────────────────────────

func handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	resp := StatusResponse{
		Syncs:       listSyncs(),
		Attachments: listAttachments(),
		Backups:     listBackups(),
	}
	writeJSON(w, resp)
}

// ─────────────────────────────────────────────
// Sync — push / pull database snapshots
// ─────────────────────────────────────────────

func handleSyncPush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "PUT only", http.StatusMethodNotAllowed)
		return
	}
	platform := strings.TrimPrefix(r.URL.Path, "/sync/push/")
	if platform == "" || platform == "/" {
		http.Error(w, "platform required: /sync/push/ios", http.StatusBadRequest)
		return
	}
	path := filepath.Join(dataDir, "sync", fmt.Sprintf("%s-letters.db", platform))
	f, err := os.Create(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	n, _ := io.Copy(f, r.Body)
	log.Printf("[sync] push %s → %d bytes", platform, n)
	w.WriteHeader(http.StatusOK)
}

func handleSyncPull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	platform := strings.TrimPrefix(r.URL.Path, "/sync/pull/")
	if platform == "" || platform == "/" {
		http.Error(w, "platform required", http.StatusBadRequest)
		return
	}
	path := filepath.Join(dataDir, "sync", fmt.Sprintf("%s-letters.db", platform))
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	io.Copy(w, f)
}

func handleSyncList(w http.ResponseWriter, r *http.Request) {
	syncs := listSyncs()
	writeJSON(w, syncs)
}

func listSyncs() []SyncPayload {
	var out []SyncPayload
	files, _ := filepath.Glob(filepath.Join(dataDir, "sync", "*-letters.db"))
	for _, f := range files {
		name := filepath.Base(f)
		platform := strings.TrimSuffix(name, "-letters.db")
		info, err := os.Stat(f)
		if err != nil {
			continue
		}
		out = append(out, SyncPayload{
			Platform:  platform,
			Timestamp: info.ModTime().UnixMilli(),
			Size:      info.Size(),
		})
	}
	return out
}

// ─────────────────────────────────────────────
// Attachments
// ─────────────────────────────────────────────

func handleAttachmentUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "PUT only", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	path := filepath.Join(dataDir, "attachments", id)
	f, err := os.Create(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	n, _ := io.Copy(f, r.Body)
	log.Printf("[attachment] upload %s → %d bytes", id, n)
	w.WriteHeader(http.StatusOK)
}

func handleAttachmentDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/attachment/download/")
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	path := filepath.Join(dataDir, "attachments", id)
	http.ServeFile(w, r, path)
}

func listAttachments() []AttachmentMeta {
	var out []AttachmentMeta
	files, _ := filepath.Glob(filepath.Join(dataDir, "attachments", "*"))
	for _, f := range files {
		info, _ := os.Stat(f)
		out = append(out, AttachmentMeta{
			ID:   filepath.Base(f),
			Size: info.Size(),
		})
	}
	return out
}

// ─────────────────────────────────────────────
// Backup
// ─────────────────────────────────────────────

func handleBackupPush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "PUT only", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		id = fmt.Sprintf("backup-%d", time.Now().Unix())
	}
	path := filepath.Join(dataDir, "backup", fmt.Sprintf("%s.letterstomy", id))
	f, err := os.Create(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	n, _ := io.Copy(f, r.Body)
	log.Printf("[backup] push %s → %d bytes", id, n)
	w.WriteHeader(http.StatusOK)
}

func handleBackupPull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/backup/pull/")
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	path := filepath.Join(dataDir, "backup", fmt.Sprintf("%s.letterstomy", id))
	http.ServeFile(w, r, path)
}

func handleBackupList(w http.ResponseWriter, r *http.Request) {
	backups := listBackups()
	writeJSON(w, backups)
}

func listBackups() []BackupMeta {
	var out []BackupMeta
	files, _ := filepath.Glob(filepath.Join(dataDir, "backup", "*.letterstomy"))
	for _, f := range files {
		name := filepath.Base(f)
		id := strings.TrimSuffix(name, ".letterstomy")
		info, _ := os.Stat(f)
		out = append(out, BackupMeta{
			ID:        id,
			Timestamp: info.ModTime().UnixMilli(),
			Size:      info.Size(),
		})
	}
	return out
}

// ─────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func sha256hex(data []byte) string {
	h := sha256.Sum256(data)
	return fmt.Sprintf("%x", h)
}