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
	Branches    int              `json:"branches"`
	Folders     int              `json:"folders"`
	Members     int              `json:"members"`
	Invitations int              `json:"invitations"`
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
	BranchIDs []string `json:"branch_ids,omitempty"`
	FolderIDs []string `json:"folder_ids,omitempty"`
	Expires   int64  `json:"expires"`
}

// ── Family structure ──

type Branch struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`      // parents, maternal, paternal, chosenFamily, custom
	IsSeeded  bool     `json:"is_seeded"`
	MemberIDs []string `json:"member_ids"` // who has access to this branch
	CreatedAt int64    `json:"created_at"`
}

type Folder struct {
	ID         string   `json:"id"`
	BranchID   string   `json:"branch_id"`
	ParentID   string   `json:"parent_id,omitempty"`
	Name       string   `json:"name"`
	MemberIDs  []string `json:"member_ids,omitempty"` // scope override
	CreatedAt  int64    `json:"created_at"`
}

type CollaborationState struct {
	Members     map[string]Member     `json:"members"`
	Invitations map[string]Invitation `json:"invitations"`
	Branches    map[string]Branch     `json:"branches"`
	Folders     map[string]Folder     `json:"folders"`
}

// ─────────────────────────────────────────────
// Server state
// ─────────────────────────────────────────────

var (
	dataDir string
	apiKeys map[string]string

	mu            sync.RWMutex
	collaboration = CollaborationState{
		Members:     map[string]Member{},
		Invitations: map[string]Invitation{},
		Branches:    map[string]Branch{},
		Folders:     map[string]Folder{},
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

	// Collaboration — members
	mux.HandleFunc("/members", auth(handleMembers))

	// Collaboration — invitations
	mux.HandleFunc("/invite", auth(handleCreateInvite))
	mux.HandleFunc("/invite/", auth(handleInviteAction))

	// Collaboration — family branches
	mux.HandleFunc("/branches", auth(handleBranches))
	mux.HandleFunc("/branches/", auth(handleBranchByID))

	// Collaboration — folders within branches
	mux.HandleFunc("/folders", auth(handleFolders))
	mux.HandleFunc("/folders/", auth(handleFolderByID))

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
	mu.RLock()
	defer mu.RUnlock()
	resp := StatusResponse{
		Syncs:       listSyncs(),
		Attachments: listAttachments(),
		Recoveries:  listBackups(),
		Branches:    len(collaboration.Branches),
		Folders:     len(collaboration.Folders),
		Members:     len(collaboration.Members),
		Invitations: len(collaboration.Invitations),
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
// Collaboration — members
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

// ─────────────────────────────────────────────
// Collaboration — invitations
// ─────────────────────────────────────────────

func handleCreateInvite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		CreatedBy string   `json:"created_by"`
		Role      string   `json:"role"`
		BranchIDs []string `json:"branch_ids"`
		FolderIDs []string `json:"folder_ids"`
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
		BranchIDs: req.BranchIDs,
		FolderIDs: req.FolderIDs,
		Expires:   time.Now().Add(7 * 24 * time.Hour).UnixMilli(),
	}
	collaboration.Invitations[code] = inv
	saveCollaboration()

	log.Printf("[invite] created %s by %s (role: %s, branches: %v)", code, req.CreatedBy, inviteRole, req.BranchIDs)
	writeJSON(w, map[string]any{
		"code":       code,
		"role":       inviteRole,
		"branch_ids": req.BranchIDs,
		"folder_ids": req.FolderIDs,
		"expires":    inv.Expires,
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

		// Create the member
		memberID := req.MemberID
		member := Member{
			ID:    memberID,
			Name:  req.MemberName,
			Role:  inv.Role,
			Since: time.Now().UnixMilli(),
		}
		collaboration.Members[memberID] = member

		// Grant access to the invited branches/folders
		for _, branchID := range inv.BranchIDs {
			if b, ok := collaboration.Branches[branchID]; ok {
				b.MemberIDs = appendIfMissing(b.MemberIDs, memberID)
				collaboration.Branches[branchID] = b
			}
		}
		for _, folderID := range inv.FolderIDs {
			if f, ok := collaboration.Folders[folderID]; ok {
				f.MemberIDs = appendIfMissing(f.MemberIDs, memberID)
				collaboration.Folders[folderID] = f
			}
		}

		delete(collaboration.Invitations, code)
		saveCollaboration()
		log.Printf("[invite] accepted %s as %s (member: %s)", code, inv.Role, req.MemberName)
		writeJSON(w, map[string]any{
			"member_id":  memberID,
			"role":       inv.Role,
			"branch_ids": inv.BranchIDs,
			"status":     "accepted",
		})

	case http.MethodDelete:
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
// Collaboration — branches
// ─────────────────────────────────────────────

func handleBranches(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		mu.RLock()
		defer mu.RUnlock()
		branches := make([]Branch, 0, len(collaboration.Branches))
		for _, b := range collaboration.Branches {
			branches = append(branches, b)
		}
		writeJSON(w, branches)

	case http.MethodPost:
		var branch Branch
		if err := json.NewDecoder(r.Body).Decode(&branch); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		if branch.ID == "" || branch.Name == "" {
			http.Error(w, "id and name required", http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if branch.CreatedAt == 0 {
			branch.CreatedAt = time.Now().UnixMilli()
		}
		if branch.Kind == "" {
			branch.Kind = "custom"
		}
		collaboration.Branches[branch.ID] = branch
		saveCollaboration()
		log.Printf("[branches] created %s (%s)", branch.Name, branch.Kind)
		writeJSON(w, branch)

	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if id == "" {
			http.Error(w, "id required", http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		delete(collaboration.Branches, id)
		// Cascade delete folders under this branch
		for fid, f := range collaboration.Folders {
			if f.BranchID == id {
				delete(collaboration.Folders, fid)
			}
		}
		saveCollaboration()
		log.Printf("[branches] deleted %s", id)
		w.WriteHeader(http.StatusOK)

	default:
		http.Error(w, "GET/POST/DELETE", http.StatusMethodNotAllowed)
	}
}

func handleBranchByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/branches/")
	if id == "" || id == "/" {
		http.Error(w, "branch id required", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		mu.RLock()
		defer mu.RUnlock()
		branch, ok := collaboration.Branches[id]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, branch)

	case http.MethodPut:
		// Update: share with members, rename, etc
		var branch Branch
		if err := json.NewDecoder(r.Body).Decode(&branch); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if _, ok := collaboration.Branches[id]; ok {
			branch.ID = id
			collaboration.Branches[id] = branch
			saveCollaboration()
			log.Printf("[branches] updated %s (shared with %d members)", id, len(branch.MemberIDs))
		}
		writeJSON(w, branch)

	case http.MethodDelete:
		mu.Lock()
		defer mu.Unlock()
		delete(collaboration.Branches, id)
		for fid, f := range collaboration.Folders {
			if f.BranchID == id {
				delete(collaboration.Folders, fid)
			}
		}
		saveCollaboration()
		log.Printf("[branches] deleted %s", id)
		w.WriteHeader(http.StatusOK)

	default:
		http.Error(w, "GET/PUT/DELETE", http.StatusMethodNotAllowed)
	}
}

// ─────────────────────────────────────────────
// Collaboration — folders
// ─────────────────────────────────────────────

func handleFolders(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		branchID := r.URL.Query().Get("branch_id")
		mu.RLock()
		defer mu.RUnlock()
		folders := make([]Folder, 0)
		for _, f := range collaboration.Folders {
			if branchID == "" || f.BranchID == branchID {
				folders = append(folders, f)
			}
		}
		writeJSON(w, folders)

	case http.MethodPost:
		var folder Folder
		if err := json.NewDecoder(r.Body).Decode(&folder); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		if folder.ID == "" || folder.Name == "" || folder.BranchID == "" {
			http.Error(w, "id, name, and branch_id required", http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if folder.CreatedAt == 0 {
			folder.CreatedAt = time.Now().UnixMilli()
		}
		collaboration.Folders[folder.ID] = folder
		saveCollaboration()
		log.Printf("[folders] created %s in branch %s", folder.Name, folder.BranchID)
		writeJSON(w, folder)

	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if id == "" {
			http.Error(w, "id required", http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		delete(collaboration.Folders, id)
		saveCollaboration()
		log.Printf("[folders] deleted %s", id)
		w.WriteHeader(http.StatusOK)

	default:
		http.Error(w, "GET/POST/DELETE", http.StatusMethodNotAllowed)
	}
}

func handleFolderByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/folders/")
	if id == "" || id == "/" {
		http.Error(w, "folder id required", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		mu.RLock()
		defer mu.RUnlock()
		folder, ok := collaboration.Folders[id]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, folder)

	case http.MethodPut:
		var folder Folder
		if err := json.NewDecoder(r.Body).Decode(&folder); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if _, ok := collaboration.Folders[id]; ok {
			folder.ID = id
			collaboration.Folders[id] = folder
			saveCollaboration()
		}
		writeJSON(w, folder)

	case http.MethodDelete:
		mu.Lock()
		defer mu.Unlock()
		delete(collaboration.Folders, id)
		saveCollaboration()
		log.Printf("[folders] deleted %s", id)
		w.WriteHeader(http.StatusOK)

	default:
		http.Error(w, "GET/PUT/DELETE", http.StatusMethodNotAllowed)
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

func appendIfMissing(slice []string, item string) []string {
	for _, s := range slice {
		if s == item {
			return slice
		}
	}
	return append(slice, item)
}