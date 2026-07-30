package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ─────────────────────────────────────────────
// Data models
// ─────────────────────────────────────────────

type SyncPayload struct {
	Platform  string `json:"platform"`
	Timestamp int64  `json:"timestamp"`
	Size      int64  `json:"size"`
}

type AttachmentMeta struct {
	ID          string `json:"id"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
}

type RecoveryMeta struct {
	ID          string `json:"id"`
	Timestamp   int64  `json:"timestamp"`
	Size        int64  `json:"size"`
	LetterCount int    `json:"letter_count"`
}

type StatusResponse struct {
	Syncs       []SyncPayload    `json:"syncs"`
	Attachments []AttachmentMeta `json:"attachments"`
	Recoveries  []RecoveryMeta   `json:"recoveries"`
}

// ── Collaboration ──

type Role string

const (
	RoleOwner       Role = "owner"
	RoleEditor      Role = "editor"
	RoleContributor Role = "contributor"
	RoleViewer      Role = "viewer"
)

type Member struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Role  Role   `json:"role"`
	Since int64  `json:"since"`
}

type Invitation struct {
	Code      string `json:"code"`
	CreatedBy string `json:"created_by"`
	Role      Role   `json:"role"`
	Expires   int64  `json:"expires"`
}

type CollaborationState struct {
	Members     map[string]Member     `json:"members"`
	Invitations map[string]Invitation `json:"invitations"`
}

// ─────────────────────────────────────────────
// Server state
// ─────────────────────────────────────────────

var (
	dataDir  string
	apiKeys  map[string]string

	mu             sync.RWMutex
	collaboration  = CollaborationState{
		Members:     map[string]Member{},
		Invitations: map[string]Invitation{},
	}
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
	loadCollaboration()

	os.MkdirAll(filepath.Join(dataDir, "sync"), 0755)
	os.MkdirAll(filepath.Join(dataDir, "attachments"), 0755)
	os.MkdirAll(filepath.Join(dataDir, "backup"), 0755)

	mux := http.NewServeMux()

	// Lifecycle
	mux.HandleFunc("/status", auth(handleStatus))

	// Sync
	mux.HandleFunc("/sync/push/", auth(handleSyncPush))
	mux.HandleFunc("/sync/pull/", auth(handleSyncPull))
	mux.HandleFunc("/sync/list", auth(handleSyncList))

	// Attachments
	mux.HandleFunc("/attachment/upload", auth(handleAttachmentUpload))
	mux.HandleFunc("/attachment/download/", auth(handleAttachmentDownload))

	// Backup
	mux.HandleFunc("/backup/push", auth(handleBackupPush))
	mux.HandleFunc("/backup/pull/", auth(handleBackupPull))
	mux.HandleFunc("/backup/list", auth(handleBackupList))

	// Collaboration — cross-platform invitations and roles
	mux.HandleFunc("/members", auth(handleMembers))
	mux.HandleFunc("/invite", auth(handleCreateInvite))
	mux.HandleFunc("/invite/", auth(handleInviteAction))

	log.Printf("[letters2my-sync] listening on :%s …", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

// ─────────────────────────────────────────────
// Auth
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

func loadCollaboration() {
	mu.Lock()
	defer mu.Unlock()
	path := filepath.Join(dataDir, "collaboration.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	json.Unmarshal(data, &collaboration)
}

func saveCollaboration() {
	data, _ := json.MarshalIndent(collaboration, "", "  ")
	os.WriteFile(filepath.Join(dataDir, "collaboration.json"), data, 0644)
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
		Recoveries:  listBackups(),
	}
	writeJSON(w, resp)
}

// ─────────────────────────────────────────────
// Sync
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
	log.Printf("[sync] push %s → %d byte", platform, n)
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
	writeJSON(w, listSyncs())
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
	writeJSON(w, listBackups())
}

func listBackups() []RecoveryMeta {
	var out []RecoveryMeta
	files, _ := filepath.Glob(filepath.Join(dataDir, "backup", "*.letterstomy"))
	for _, f := range files {
		name := filepath.Base(f)
		id := strings.TrimSuffix(name, ".letterstomy")
		info, _ := os.Stat(f)
		out = append(out, RecoveryMeta{
			ID:        id,
			Timestamp: info.ModTime().UnixMilli(),
			Size:      info.Size(),
		})
	}
	return out
}

// ─────────────────────────────────────────────
// Collaboration — invitations and roles
// ─────────────────────────────────────────────

func handleMembers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		mu.RLock()
		defer mu.RUnlock()
		members := make([]Member, 0, len(collaboration.Members))
		for _, m := range collaboration.Members {
			members = append(members, m)
		}
		writeJSON(w, members)

	case http.MethodPut:
		var member Member
		if err := json.NewDecoder(r.Body).Decode(&member); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		if member.ID == "" || member.Name == "" || member.Role == "" {
			http.Error(w, "id, name, and role required", http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if member.Since == 0 {
			member.Since = time.Now().UnixMilli()
		}
		collaboration.Members[member.ID] = member
		saveCollaboration()
		log.Printf("[members] added/updated %s (%s)", member.Name, member.Role)
		w.WriteHeader(http.StatusOK)

	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if id == "" {
			http.Error(w, "id required", http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if _, ok := collaboration.Members[id]; ok {
			delete(collaboration.Members, id)
			saveCollaboration()
			log.Printf("[members] removed %s", id)
		}
		w.WriteHeader(http.StatusOK)

	default:
		http.Error(w, "GET/PUT/DELETE", http.StatusMethodNotAllowed)
	}
}

func handleCreateInvite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		CreatedBy string `json:"created_by"`
		Role      string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if req.CreatedBy == "" {
		http.Error(w, "created_by required", http.StatusBadRequest)
		return
	}

	inviteRole := RoleEditor
	switch req.Role {
	case "owner", "editor", "contributor", "viewer":
		inviteRole = Role(req.Role)
	}

	code := genInviteCode()

	mu.Lock()
	defer mu.Unlock()

	// Remove expired invitations
	now := time.Now().UnixMilli()
	for k, v := range collaboration.Invitations {
		if v.Expires > 0 && now > v.Expires {
			delete(collaboration.Invitations, k)
		}
	}

	inv := Invitation{
		Code:      code,
		CreatedBy: req.CreatedBy,
		Role:      inviteRole,
		Expires:   time.Now().Add(7 * 24 * time.Hour).UnixMilli(),
	}
	collaboration.Invitations[code] = inv
	saveCollaboration()

	log.Printf("[invite] created %s for %s (role: %s)", code, req.CreatedBy, inviteRole)
	writeJSON(w, map[string]any{
		"code":    code,
		"role":    inviteRole,
		"expires": inv.Expires,
	})
}

func handleInviteAction(w http.ResponseWriter, r *http.Request) {
	code := strings.TrimPrefix(r.URL.Path, "/invite/")
	if code == "" || code == "/" {
		http.Error(w, "invite code required in path: /invite/ABC123", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		// Look up an invitation (doesn't consume it)
		mu.RLock()
		defer mu.RUnlock()
		inv, ok := collaboration.Invitations[code]
		if !ok {
			http.Error(w, "not found or expired", http.StatusNotFound)
			return
		}
		if inv.Expires > 0 && time.Now().UnixMilli() > inv.Expires {
			http.Error(w, "invitation expired", http.StatusGone)
			return
		}
		writeJSON(w, inv)

	case http.MethodPost:
		// Accept an invitation
		var req struct {
			MemberID   string `json:"member_id"`
			MemberName string `json:"member_name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		inv, ok := collaboration.Invitations[code]
		if !ok {
			http.Error(w, "not found or expired", http.StatusNotFound)
			return
		}
		if inv.Expires > 0 && time.Now().UnixMilli() > inv.Expires {
			delete(collaboration.Invitations, code)
			saveCollaboration()
			http.Error(w, "invitation expired", http.StatusGone)
			return
		}

		collaboration.Members[req.MemberID] = Member{
			ID:    req.MemberID,
			Name:  req.MemberName,
			Role:  inv.Role,
			Since: time.Now().UnixMilli(),
		}
		delete(collaboration.Invitations, code)
		saveCollaboration()
		log.Printf("[invite] accepted %s as %s (member: %s)", code, inv.Role, req.MemberName)
		writeJSON(w, map[string]any{
			"member_id": req.MemberID,
			"role":      inv.Role,
			"status":    "accepted",
		})

	case http.MethodDelete:
		// Revoke an invitation
		mu.Lock()
		defer mu.Unlock()
		_, ok := collaboration.Invitations[code]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		delete(collaboration.Invitations, code)
		saveCollaboration()
		log.Printf("[invite] revoked %s", code)
		w.WriteHeader(http.StatusOK)

	default:
		http.Error(w, "GET/POST/DELETE", http.StatusMethodNotAllowed)
	}
}

// ─────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────

func genInviteCode() string {
	b := make([]byte, 6)
	rand.Read(b)
	return strings.ToUpper(hex.EncodeToString(b))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func sha256hex(data []byte) string {
	h := sha256.Sum256(data)
	return fmt.Sprintf("%x", h)
}