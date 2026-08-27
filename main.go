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
	"regexp"
	"strings"
	"sync"
	"time"
)

// ─────────────────────────────────────────────
// Data models
// ─────────────────────────────────────────────

// Service identity and capability advertisement. The Swift client refuses
// to treat an arbitrary HTTP 200 as proof of compatibility; it validates
// `service`, `api_version`, and `capabilities` against its own expectations.
const (
	serviceName    = "LettersToMy-SelfHostedSync"
	apiVersion     = 1
	serverVersion  = "0.3.0"
)

var capabilities = []string{"collaboration", "backups", "attachments"}

type SyncPayload struct {
	Platform  string `json:"platform"`
	Timestamp int64  `json:"timestamp"`
	Size      int64  `json:"size"`
	Kind      string `json:"kind"` // "device-snapshot": raw platform database snapshot, not a logical sync record
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
	Service      string           `json:"service"`
	APIVersion   int              `json:"api_version"`
	ServerVersion string          `json:"server_version"`
	Capabilities []string         `json:"capabilities"`
	Syncs        []SyncPayload    `json:"syncs"`
	Attachments  []AttachmentMeta `json:"attachments"`
	Recoveries   []RecoveryMeta   `json:"recoveries"`
	Branches     int              `json:"branches"`
	Folders      int              `json:"folders"`
	Members      int              `json:"members"`
	Invitations  int              `json:"invitations"`
}

// ── Collaboration ──

// Role is the canonical wire representation of the client's
// CollaborationRole raw values. The client sends exactly these strings;
// anything else is rejected with 400 (never silently remapped, and never
// defaulted to a more privileged role).
type Role string

const (
	RoleOwner       Role = "owner"
	RoleParentAdmin Role = "parentAdmin"
	RoleOrganizer   Role = "organizer"
	RoleContributor Role = "contributor"
	RoleViewer      Role = "viewer"
	RoleRecipient   Role = "recipient"
)

var validRoles = map[Role]bool{
	RoleOwner: true, RoleParentAdmin: true, RoleOrganizer: true,
	RoleContributor: true, RoleViewer: true, RoleRecipient: true,
}

func validRole(r Role) bool { return validRoles[r] }

type Member struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Role  Role   `json:"role"`
	Since int64  `json:"since"`
}

type Invitation struct {
	Code      string   `json:"code"`
	CreatedBy string   `json:"created_by"`
	Role      Role     `json:"role"`
	BranchIDs []string `json:"branch_ids"`
	FolderIDs []string `json:"folder_ids"`
	Expires   int64    `json:"expires"`
}

// ── Family structure ──

type Branch struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Kind      string   `json:"kind"` // parents, maternal, paternal, chosenFamily, custom
	IsSeeded  bool     `json:"is_seeded"`
	MemberIDs []string `json:"member_ids"` // who has access to this branch
	CreatedAt int64    `json:"created_at"`
}

type Folder struct {
	ID        string   `json:"id"`
	BranchID  string   `json:"branch_id"`
	ParentID  string   `json:"parent_id,omitempty"`
	Name      string   `json:"name"`
	MemberIDs []string `json:"member_ids"` // scope override; always an array, never null
	CreatedAt int64    `json:"created_at"`
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
	dataDir   string
	apiKeys   map[string]string
	maxUpload int64 = 512 << 20 // 512 MiB, configurable via MAX_UPLOAD_BYTES

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
	if v := os.Getenv("MAX_UPLOAD_BYTES"); v != "" {
		if n, err := parseBytes(v); err == nil && n > 0 {
			maxUpload = n
		}
	}
	loadAPIKeys()
	loadCollaboration()

	os.MkdirAll(filepath.Join(dataDir, "sync"), 0755)
	os.MkdirAll(filepath.Join(dataDir, "attachments"), 0755)
	os.MkdirAll(filepath.Join(dataDir, "backup"), 0755)

	mux := newMux()

	log.Printf("[%s] listening on :%s (api v%d, data %s)", serviceName, port, apiVersion, dataDir)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

func newMux() *http.ServeMux {
	mux := http.NewServeMux()

	// Lifecycle
	mux.HandleFunc("/status", auth(handleStatus))

	// Sync — device snapshots (raw platform database files)
	mux.HandleFunc("/sync/push/", auth(handleSyncPush))
	mux.HandleFunc("/sync/pull/", auth(handleSyncPull))
	mux.HandleFunc("/sync/list", auth(handleSyncList))

	// Attachments
	mux.HandleFunc("/attachment/upload", auth(handleAttachmentUpload))
	mux.HandleFunc("/attachment/list", auth(handleAttachmentList))
	mux.HandleFunc("/attachment/download/", auth(handleAttachmentDownload))
	mux.HandleFunc("/attachment/", auth(handleAttachmentDelete))

	// Backup
	mux.HandleFunc("/backup/push", auth(handleBackupPush))
	mux.HandleFunc("/backup/pull/", auth(handleBackupPull))
	mux.HandleFunc("/backup/list", auth(handleBackupList))
	mux.HandleFunc("/backup/", auth(handleBackupDelete))

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

	return mux
}

// parseBytes accepts plain integers or a suffix (K/M/G, case-insensitive).
func parseBytes(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	mult := int64(1)
	for _, suffix := range []struct {
		sfx  string
		mult int64
	}{{"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}} {
		if strings.HasSuffix(s, suffix.sfx) {
			mult = suffix.mult
			s = strings.TrimSuffix(s, suffix.sfx)
			break
		}
	}
	var n int64
	if _, err := fmt.Sscan(s, &n); err != nil {
		return 0, err
	}
	return n * mult, nil
}

// ─────────────────────────────────────────────
// Auth
// ─────────────────────────────────────────────

func loadAPIKeys() {
	apiKeys = map[string]string{}
	path := os.Getenv("API_KEYS_FILE")
	if path == "" {
		path = "/etc/letters2my/api_keys.txt"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("WARN: no api_keys.txt — using default token: letters2my (dev only)")
		apiKeys["default"] = sha256hex([]byte("letters2my"))
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		token := strings.TrimSpace(parts[1])
		if name == "" || token == "" {
			continue
		}
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
			writeError(w, http.StatusUnauthorized, "unauthorized", "The API token is missing or invalid.")
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
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET only")
		return
	}
	mu.RLock()
	defer mu.RUnlock()
	resp := StatusResponse{
		Service:       serviceName,
		APIVersion:    apiVersion,
		ServerVersion: serverVersion,
		Capabilities:  capabilities,
		Syncs:         listSyncs(),
		Attachments:   listAttachments(),
		Recoveries:    listBackups(),
		Branches:      len(collaboration.Branches),
		Folders:       len(collaboration.Folders),
		Members:       len(collaboration.Members),
		Invitations:   len(collaboration.Invitations),
	}
	writeJSON(w, resp)
}

// ─────────────────────────────────────────────
// Sync — raw device snapshots
// ─────────────────────────────────────────────

// The /sync endpoints store and retrieve whole platform database files
// (Core Data SQLite on iOS, Room SQLite on Android). They are DEVICE
// SNAPSHOTS / server-side backup artifacts, NOT a logical cross-platform
// synchronization protocol: a raw Android database cannot be dropped into
// an iOS Core Data store. The Swift client exposes these only as snapshot
// storage, never as live "sync".

func handleSyncPush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "PUT only")
		return
	}
	platform := strings.TrimPrefix(r.URL.Path, "/sync/push/")
	if !validID(platform) {
		writeError(w, http.StatusBadRequest, "invalid_request", "platform must match [A-Za-z0-9._-]{1,64}")
		return
	}
	path := filepath.Join(dataDir, "sync", fmt.Sprintf("%s-letters.db", platform))
	f, err := os.Create(path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	defer f.Close()
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	n, err := io.Copy(f, r.Body)
	if err != nil {
		os.Remove(path)
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "upload exceeds the configured limit")
		return
	}
	log.Printf("[sync] push %s → %d bytes", platform, n)
	w.WriteHeader(http.StatusOK)
}

func handleSyncPull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET only")
		return
	}
	platform := strings.TrimPrefix(r.URL.Path, "/sync/pull/")
	if !validID(platform) {
		writeError(w, http.StatusBadRequest, "invalid_request", "platform required")
		return
	}
	path := filepath.Join(dataDir, "sync", fmt.Sprintf("%s-letters.db", platform))
	f, err := os.Open(path)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no snapshot for platform "+platform)
		return
	}
	defer f.Close()
	io.Copy(w, f)
}

func handleSyncList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET only")
		return
	}
	writeJSON(w, listSyncs())
}

func listSyncs() []SyncPayload {
	out := make([]SyncPayload, 0)
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
			Kind:      "device-snapshot",
		})
	}
	return out
}

// ─────────────────────────────────────────────
// Attachments
// ─────────────────────────────────────────────

func handleAttachmentUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "PUT only")
		return
	}
	id := r.URL.Query().Get("id")
	if !validID(id) {
		writeError(w, http.StatusBadRequest, "invalid_request", "id must match [A-Za-z0-9._-]{1,128}")
		return
	}
	path := filepath.Join(dataDir, "attachments", id)
	f, err := os.Create(path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	defer f.Close()
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	n, err := io.Copy(f, r.Body)
	if err != nil {
		os.Remove(path)
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "upload exceeds the configured limit")
		return
	}
	log.Printf("[attachment] upload %s → %d bytes", id, n)
	w.WriteHeader(http.StatusOK)
}

func handleAttachmentList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET only")
		return
	}
	writeJSON(w, listAttachments())
}

func handleAttachmentDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET only")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/attachment/download/")
	if !validID(id) {
		writeError(w, http.StatusBadRequest, "invalid_request", "id must match [A-Za-z0-9._-]{1,128}")
		return
	}
	path := filepath.Join(dataDir, "attachments", id)
	if _, err := os.Stat(path); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "attachment not found")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, path)
}

func handleAttachmentDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "DELETE only")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/attachment/")
	if !validID(id) {
		writeError(w, http.StatusBadRequest, "invalid_request", "id must match [A-Za-z0-9._-]{1,128}")
		return
	}
	path := filepath.Join(dataDir, "attachments", id)
	if err := os.Remove(path); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "attachment not found")
		return
	}
	log.Printf("[attachment] deleted %s", id)
	w.WriteHeader(http.StatusOK)
}

func listAttachments() []AttachmentMeta {
	out := make([]AttachmentMeta, 0)
	files, _ := filepath.Glob(filepath.Join(dataDir, "attachments", "*"))
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			continue
		}
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

// Backups are opaque encrypted archives from the client. The server never
// inspects the payload, never needs the passphrase, and never sees
// plaintext letter contents.

func handleBackupPush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "PUT only")
		return
	}
	id := r.URL.Query().Get("id")
	if !validID(id) {
		writeError(w, http.StatusBadRequest, "invalid_request", "id must match [A-Za-z0-9._-]{1,128}")
		return
	}
	path := filepath.Join(dataDir, "backup", fmt.Sprintf("%s.letterstomy", id))
	f, err := os.Create(path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	defer f.Close()
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	n, err := io.Copy(f, r.Body)
	if err != nil {
		os.Remove(path)
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "upload exceeds the configured limit")
		return
	}
	log.Printf("[backup] push %s → %d bytes", id, n)
	writeJSON(w, RecoveryMeta{
		ID:        id,
		Timestamp: time.Now().UnixMilli(),
		Size:      n,
	})
}

func handleBackupPull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET only")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/backup/pull/")
	if !validID(id) {
		writeError(w, http.StatusBadRequest, "invalid_request", "id must match [A-Za-z0-9._-]{1,128}")
		return
	}
	path := filepath.Join(dataDir, "backup", fmt.Sprintf("%s.letterstomy", id))
	if _, err := os.Stat(path); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "backup not found")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, path)
}

func handleBackupList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET only")
		return
	}
	writeJSON(w, listBackups())
}

func handleBackupDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "DELETE only")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/backup/")
	if !validID(id) {
		writeError(w, http.StatusBadRequest, "invalid_request", "id must match [A-Za-z0-9._-]{1,128}")
		return
	}
	path := filepath.Join(dataDir, "backup", fmt.Sprintf("%s.letterstomy", id))
	if err := os.Remove(path); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "backup not found")
		return
	}
	log.Printf("[backup] deleted %s", id)
	w.WriteHeader(http.StatusOK)
}

func listBackups() []RecoveryMeta {
	out := make([]RecoveryMeta, 0)
	files, _ := filepath.Glob(filepath.Join(dataDir, "backup", "*.letterstomy"))
	for _, f := range files {
		name := filepath.Base(f)
		id := strings.TrimSuffix(name, ".letterstomy")
		info, err := os.Stat(f)
		if err != nil {
			continue
		}
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
			writeError(w, http.StatusBadRequest, "invalid_request", "invalid body")
			return
		}
		if !validID(member.ID) || member.Name == "" {
			writeError(w, http.StatusBadRequest, "invalid_request", "id and name required; id must match [A-Za-z0-9._-]{1,128}")
			return
		}
		if !validRole(member.Role) {
			writeError(w, http.StatusBadRequest, "invalid_request", "unknown role "+string(member.Role))
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
		if !validID(id) {
			writeError(w, http.StatusBadRequest, "invalid_request", "id required")
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if _, ok := collaboration.Members[id]; !ok {
			writeError(w, http.StatusNotFound, "not_found", "member not found")
			return
		}
		delete(collaboration.Members, id)
		// Clean up scope references so removed members never linger in
		// branch/folder access lists.
		for bid, b := range collaboration.Branches {
			if newIDs := removeString(b.MemberIDs, id); len(newIDs) != len(b.MemberIDs) {
				b.MemberIDs = newIDs
				collaboration.Branches[bid] = b
			}
		}
		for fid, f := range collaboration.Folders {
			if newIDs := removeString(f.MemberIDs, id); len(newIDs) != len(f.MemberIDs) {
				f.MemberIDs = newIDs
				collaboration.Folders[fid] = f
			}
		}
		saveCollaboration()
		log.Printf("[members] removed %s", id)
		w.WriteHeader(http.StatusOK)

	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET/PUT/DELETE")
	}
}

// ─────────────────────────────────────────────
// Collaboration — invitations
// ─────────────────────────────────────────────

func handleCreateInvite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST only")
		return
	}

	var req struct {
		CreatedBy string   `json:"created_by"`
		Role      string   `json:"role"`
		BranchIDs []string `json:"branch_ids"`
		FolderIDs []string `json:"folder_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid body")
		return
	}
	if !validID(req.CreatedBy) {
		writeError(w, http.StatusBadRequest, "invalid_request", "created_by required")
		return
	}
	inviteRole := RoleViewer // least privilege when no role is specified
	if req.Role != "" {
		if !validRole(Role(req.Role)) {
			writeError(w, http.StatusBadRequest, "invalid_request", "unknown role "+req.Role)
			return
		}
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
	if inv.BranchIDs == nil {
		inv.BranchIDs = []string{}
	}
	if inv.FolderIDs == nil {
		inv.FolderIDs = []string{}
	}
	collaboration.Invitations[code] = inv
	saveCollaboration()

	log.Printf("[invite] created %s by %s (role: %s, branches: %v)", code, req.CreatedBy, inviteRole, req.BranchIDs)
	writeJSON(w, inv)
}

func handleInviteAction(w http.ResponseWriter, r *http.Request) {
	code := strings.TrimPrefix(r.URL.Path, "/invite/")
	if !validID(code) {
		writeError(w, http.StatusBadRequest, "invalid_request", "invite code required in path: /invite/ABC123")
		return
	}

	switch r.Method {
	case http.MethodGet:
		mu.RLock()
		defer mu.RUnlock()
		inv, ok := collaboration.Invitations[code]
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", "invitation not found")
			return
		}
		if inv.Expires > 0 && time.Now().UnixMilli() > inv.Expires {
			writeError(w, http.StatusGone, "expired", "invitation expired")
			return
		}
		writeJSON(w, inv)

	case http.MethodPost:
		var req struct {
			MemberID   string `json:"member_id"`
			MemberName string `json:"member_name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "invalid body")
			return
		}
		if !validID(req.MemberID) || req.MemberName == "" {
			writeError(w, http.StatusBadRequest, "invalid_request", "member_id and member_name required")
			return
		}
		mu.Lock()
		defer mu.Unlock()
		inv, ok := collaboration.Invitations[code]
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", "invitation not found")
			return
		}
		if inv.Expires > 0 && time.Now().UnixMilli() > inv.Expires {
			delete(collaboration.Invitations, code)
			saveCollaboration()
			writeError(w, http.StatusGone, "expired", "invitation expired")
			return
		}
		if _, exists := collaboration.Members[req.MemberID]; exists {
			writeError(w, http.StatusConflict, "conflict", "a member with this id already exists")
			return
		}

		// Create the member
		member := Member{
			ID:    req.MemberID,
			Name:  req.MemberName,
			Role:  inv.Role,
			Since: time.Now().UnixMilli(),
		}
		collaboration.Members[member.ID] = member

		// Grant access to the invited branches/folders
		for _, branchID := range inv.BranchIDs {
			if b, ok := collaboration.Branches[branchID]; ok {
				b.MemberIDs = appendIfMissing(b.MemberIDs, member.ID)
				collaboration.Branches[branchID] = b
			}
		}
		for _, folderID := range inv.FolderIDs {
			if f, ok := collaboration.Folders[folderID]; ok {
				f.MemberIDs = appendIfMissing(f.MemberIDs, member.ID)
				collaboration.Folders[folderID] = f
			}
		}

		delete(collaboration.Invitations, code)
		saveCollaboration()
		log.Printf("[invite] accepted %s as %s (member: %s)", code, inv.Role, req.MemberName)
		writeJSON(w, map[string]any{
			"code":       code,
			"member_id":  member.ID,
			"role":       inv.Role,
			"branch_ids": inv.BranchIDs,
			"folder_ids": inv.FolderIDs,
			"status":     "accepted",
		})

	case http.MethodDelete:
		mu.Lock()
		defer mu.Unlock()
		_, ok := collaboration.Invitations[code]
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", "invitation not found")
			return
		}
		delete(collaboration.Invitations, code)
		saveCollaboration()
		log.Printf("[invite] revoked %s", code)
		w.WriteHeader(http.StatusOK)

	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET/POST/DELETE")
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
			writeError(w, http.StatusBadRequest, "invalid_request", "invalid body")
			return
		}
		if !validID(branch.ID) || branch.Name == "" {
			writeError(w, http.StatusBadRequest, "invalid_request", "id and name required; id must match [A-Za-z0-9._-]{1,128}")
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if _, ok := collaboration.Branches[branch.ID]; ok {
			writeError(w, http.StatusConflict, "conflict", "a branch with this id already exists")
			return
		}
		if branch.CreatedAt == 0 {
			branch.CreatedAt = time.Now().UnixMilli()
		}
		if branch.Kind == "" {
			branch.Kind = "custom"
		}
		if branch.MemberIDs == nil {
			branch.MemberIDs = []string{}
		}
		collaboration.Branches[branch.ID] = branch
		saveCollaboration()
		log.Printf("[branches] created %s (%s)", branch.Name, branch.Kind)
		writeJSON(w, branch)

	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if !validID(id) {
			writeError(w, http.StatusBadRequest, "invalid_request", "id required")
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if _, ok := collaboration.Branches[id]; !ok {
			writeError(w, http.StatusNotFound, "not_found", "branch not found")
			return
		}
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
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET/POST/DELETE")
	}
}

func handleBranchByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/branches/")
	if !validID(id) {
		writeError(w, http.StatusBadRequest, "invalid_request", "branch id required")
		return
	}

	switch r.Method {
	case http.MethodGet:
		mu.RLock()
		defer mu.RUnlock()
		branch, ok := collaboration.Branches[id]
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", "branch not found")
			return
		}
		writeJSON(w, branch)

	case http.MethodPut:
		// Update: share with members, rename, etc
		var branch Branch
		if err := json.NewDecoder(r.Body).Decode(&branch); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "invalid body")
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if _, ok := collaboration.Branches[id]; !ok {
			writeError(w, http.StatusNotFound, "not_found", "branch not found")
			return
		}
		branch.ID = id
		if branch.MemberIDs == nil {
			branch.MemberIDs = []string{}
		}
		collaboration.Branches[id] = branch
		saveCollaboration()
		log.Printf("[branches] updated %s (shared with %d members)", id, len(branch.MemberIDs))
		writeJSON(w, branch)

	case http.MethodDelete:
		mu.Lock()
		defer mu.Unlock()
		if _, ok := collaboration.Branches[id]; !ok {
			writeError(w, http.StatusNotFound, "not_found", "branch not found")
			return
		}
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
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET/PUT/DELETE")
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
			writeError(w, http.StatusBadRequest, "invalid_request", "invalid body")
			return
		}
		if !validID(folder.ID) || folder.Name == "" || !validID(folder.BranchID) {
			writeError(w, http.StatusBadRequest, "invalid_request", "id, name, and branch_id required")
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if _, ok := collaboration.Folders[folder.ID]; ok {
			writeError(w, http.StatusConflict, "conflict", "a folder with this id already exists")
			return
		}
		if _, ok := collaboration.Branches[folder.BranchID]; !ok {
			writeError(w, http.StatusUnprocessableEntity, "invalid_request", "branch_id does not exist")
			return
		}
		if folder.CreatedAt == 0 {
			folder.CreatedAt = time.Now().UnixMilli()
		}
		if folder.MemberIDs == nil {
			folder.MemberIDs = []string{}
		}
		collaboration.Folders[folder.ID] = folder
		saveCollaboration()
		log.Printf("[folders] created %s in branch %s", folder.Name, folder.BranchID)
		writeJSON(w, folder)

	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if !validID(id) {
			writeError(w, http.StatusBadRequest, "invalid_request", "id required")
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if _, ok := collaboration.Folders[id]; !ok {
			writeError(w, http.StatusNotFound, "not_found", "folder not found")
			return
		}
		delete(collaboration.Folders, id)
		saveCollaboration()
		log.Printf("[folders] deleted %s", id)
		w.WriteHeader(http.StatusOK)

	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET/POST/DELETE")
	}
}

func handleFolderByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/folders/")
	if !validID(id) {
		writeError(w, http.StatusBadRequest, "invalid_request", "folder id required")
		return
	}

	switch r.Method {
	case http.MethodGet:
		mu.RLock()
		defer mu.RUnlock()
		folder, ok := collaboration.Folders[id]
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", "folder not found")
			return
		}
		writeJSON(w, folder)

	case http.MethodPut:
		var folder Folder
		if err := json.NewDecoder(r.Body).Decode(&folder); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "invalid body")
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if _, ok := collaboration.Folders[id]; !ok {
			writeError(w, http.StatusNotFound, "not_found", "folder not found")
			return
		}
		folder.ID = id
		if folder.MemberIDs == nil {
			folder.MemberIDs = []string{}
		}
		collaboration.Folders[id] = folder
		saveCollaboration()
		writeJSON(w, folder)

	case http.MethodDelete:
		mu.Lock()
		defer mu.Unlock()
		if _, ok := collaboration.Folders[id]; !ok {
			writeError(w, http.StatusNotFound, "not_found", "folder not found")
			return
		}
		delete(collaboration.Folders, id)
		saveCollaboration()
		log.Printf("[folders] deleted %s", id)
		w.WriteHeader(http.StatusOK)

	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET/PUT/DELETE")
	}
}

// ─────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────

var idPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// validID rejects anything that could escape its storage directory
// (path traversal), collide with reserved names, or exceed reasonable
// identifier length. UUID strings, hex invite codes, and hyphenated
// names all pass.
func validID(s string) bool {
	return s != "" && idPattern.MatchString(s) && s != "." && s != ".."
}

func genInviteCode() string {
	b := make([]byte, 6)
	rand.Read(b)
	return strings.ToUpper(hex.EncodeToString(b))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// writeError emits the canonical structured error body:
//
//	{"error":{"code":"not_found","message":"..."}}
//
// The Swift client maps `code` to typed errors (unauthorized, not_found,
// conflict, gone/expired, payload_too_large, invalid_request, internal).
func writeError(w http.ResponseWriter, status int, code, message string) {
	var body errorBody
	body.Error.Code = code
	body.Error.Message = message
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
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

func removeString(slice []string, item string) []string {
	out := slice[:0]
	for _, s := range slice {
		if s != item {
			out = append(out, s)
		}
	}
	return out
}