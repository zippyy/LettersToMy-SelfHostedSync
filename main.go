package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	currentCollaborationVersion = 1
	defaultJSONBodyLimit        = 1 << 20
	defaultSyncLimit            = 1 << 30
	defaultAttachmentLimit      = 256 << 20
	defaultBackupLimit          = 1 << 30
	defaultUploadTimeout        = 10 * time.Minute

	// API v1 identity — the Swift client validates these exactly.
	serviceName    = "LettersToMy-SelfHostedSync"
	apiVersion     = 1
	versionDefault = "0.3.0"
)

var capabilities = []string{"collaboration", "backups", "attachments"}

var (
	errTooLarge     = errors.New("request body exceeds configured limit")
	errInvalidJSON  = errors.New("invalid JSON request body")
	errTrailingJSON = errors.New("request body contains more than one JSON value")
	errInvalidState = errors.New("invalid collaboration state")
)

// Timestamps exposed by this API are Unix milliseconds.
type SyncPayload struct {
	Platform  string `json:"platform"`
	Timestamp int64  `json:"timestamp"`
	Size      int64  `json:"size"`
	Kind      string `json:"kind"` // "device-snapshot": raw platform database snapshot
}

type AttachmentMeta struct {
	ID          string `json:"id"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
}

// Backups are opaque encrypted archives. The server deliberately does not
// decrypt them; letter_count is an optional client-supplied metadata hint
// (the client knows the archive's manifest), defaulting to 0 and persisted
// in a sidecar so listings and /status can report it consistently.
type RecoveryMeta struct {
	ID          string `json:"id"`
	Timestamp   int64  `json:"timestamp"`
	Size        int64  `json:"size"`
	LetterCount int    `json:"letter_count"`
}

type StatusResponse struct {
	Service       string           `json:"service"`
	APIVersion    int              `json:"api_version"`
	ServerVersion string           `json:"server_version"`
	Capabilities  []string         `json:"capabilities"`
	Syncs         []SyncPayload    `json:"syncs"`
	Attachments   []AttachmentMeta `json:"attachments"`
	Recoveries    []RecoveryMeta   `json:"recoveries"`
	Branches      int              `json:"branches"`
	Folders       int              `json:"folders"`
	Members       int              `json:"members"`
	Invitations   int              `json:"invitations"`
	Uptime        int64            `json:"uptime_seconds"`
}

type Role string

// These values are the canonical values used by LettersToMyCore.
const (
	RoleOwner       Role = "owner"
	RoleParentAdmin Role = "parentAdmin"
	RoleOrganizer   Role = "organizer"
	RoleContributor Role = "contributor"
	RoleViewer      Role = "viewer"
	RoleRecipient   Role = "recipient"
)

func validRole(role Role) bool {
	switch role {
	case RoleOwner, RoleParentAdmin, RoleOrganizer, RoleContributor, RoleViewer, RoleRecipient:
		return true
	default:
		return false
	}
}

func migrateRole(role Role) (Role, bool) {
	// editor was the role name used by the first server release. It was a
	// broad editing role, so parentAdmin is the compatible destination. This
	// only applies to old persistence files; new API requests reject editor.
	if role == Role("editor") {
		return RoleParentAdmin, true
	}
	return role, false
}

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

type Branch struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	IsSeeded  bool     `json:"is_seeded"`
	MemberIDs []string `json:"member_ids"`
	CreatedAt int64    `json:"created_at"`
}

type Folder struct {
	ID        string   `json:"id"`
	BranchID  string   `json:"branch_id"`
	ParentID  string   `json:"parent_id,omitempty"`
	Name      string   `json:"name"`
	MemberIDs []string `json:"member_ids"`
	CreatedAt int64    `json:"created_at"`
}

type CollaborationState struct {
	Version     int                   `json:"version"`
	Members     map[string]Member     `json:"members"`
	Invitations map[string]Invitation `json:"invitations"`
	Branches    map[string]Branch     `json:"branches"`
	Folders     map[string]Folder     `json:"folders"`
}

type Limits struct {
	JSONBody   int64
	Sync       int64
	Attachment int64
	Backup     int64
}

type Server struct {
	dataDir   string
	apiKeys   map[string]string
	limits    Limits
	version   string
	startedAt time.Time
	now       func() time.Time

	mu            sync.RWMutex
	collaboration CollaborationState

	syncMu       sync.Mutex
	attachmentMu sync.Mutex
	backupMu     sync.Mutex
}

type uploadResult struct {
	ID     string `json:"id"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func emptyCollaborationState() CollaborationState {
	return CollaborationState{
		Version:     currentCollaborationVersion,
		Members:     map[string]Member{},
		Invitations: map[string]Invitation{},
		Branches:    map[string]Branch{},
		Folders:     map[string]Folder{},
	}
}

func defaultLimits() Limits {
	return Limits{
		JSONBody:   defaultJSONBodyLimit,
		Sync:       defaultSyncLimit,
		Attachment: defaultAttachmentLimit,
		Backup:     defaultBackupLimit,
	}
}

func newServer(dataDir string, keys map[string]string, limits Limits, version string) (*Server, error) {
	if len(keys) == 0 {
		return nil, errors.New("no API keys configured")
	}
	defaults := defaultLimits()
	if limits.JSONBody <= 0 {
		limits.JSONBody = defaults.JSONBody
	}
	if limits.Sync <= 0 {
		limits.Sync = defaults.Sync
	}
	if limits.Attachment <= 0 {
		limits.Attachment = defaults.Attachment
	}
	if limits.Backup <= 0 {
		limits.Backup = defaults.Backup
	}
	if version == "" {
		version = "dev"
	}

	dataDir = filepath.Clean(dataDir)
	if err := ensureDataDirs(dataDir); err != nil {
		return nil, err
	}
	state, migrated, err := loadCollaboration(filepath.Join(dataDir, "collaboration.json"))
	if err != nil {
		return nil, err
	}
	s := &Server{
		dataDir:       dataDir,
		apiKeys:       cloneKeys(keys),
		limits:        limits,
		version:       version,
		startedAt:     time.Now(),
		now:           time.Now,
		collaboration: state,
	}
	if migrated {
		if err := s.saveCollaboration(state); err != nil {
			return nil, fmt.Errorf("save migrated collaboration state: %w", err)
		}
	}
	return s, nil
}

func cloneKeys(keys map[string]string) map[string]string {
	cloned := make(map[string]string, len(keys))
	for name, key := range keys {
		cloned[name] = key
	}
	return cloned
}

func main() {
	if err := run(); err != nil {
		log.Printf("[letters2my-sync] fatal: %v", err)
		os.Exit(1)
	}
}

func run() error {
	port := envOr("PORT", "8080")
	dataDir := envOr("DATA_DIR", "/data")
	keysPath := envOr("API_KEYS_FILE", "/etc/letters2my/api_keys.txt")
	version := envOr("VERSION", versionDefault)
	limits, err := limitsFromEnv()
	if err != nil {
		return err
	}
	keys, err := loadAPIKeys(keysPath, envBool("ALLOW_INSECURE_DEFAULTS"))
	if err != nil {
		return err
	}
	s, err := newServer(dataDir, keys, limits, version)
	if err != nil {
		return err
	}

	httpServer := &http.Server{
		Addr:              ":" + port,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Large uploads are bounded by MaxBytesReader. A read timeout would
		// make the limit unusable for slow but legitimate archive uploads.
		WriteTimeout: defaultUploadTimeout,
		IdleTimeout:  2 * time.Minute,
	}

	log.Printf("[letters2my-sync] version=%s data_dir=%s api_keys=%d", version, dataDir, len(keys))
	serverErr := make(chan error, 1)
	go func() { serverErr <- httpServer.ListenAndServe() }()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)

	select {
	case err := <-serverErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case sig := <-signals:
		log.Printf("[letters2my-sync] shutting down on %s", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(ctx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return nil
	}
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envBool(name string) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	return value == "1" || value == "true" || value == "yes"
}

func limitsFromEnv() (Limits, error) {
	limits := defaultLimits()
	values := []struct {
		name   string
		target *int64
	}{
		{"MAX_JSON_BODY_SIZE", &limits.JSONBody},
		{"MAX_SYNC_SIZE", &limits.Sync},
		{"MAX_ATTACHMENT_SIZE", &limits.Attachment},
		{"MAX_BACKUP_SIZE", &limits.Backup},
	}
	for _, value := range values {
		text := strings.TrimSpace(os.Getenv(value.name))
		if text == "" {
			continue
		}
		parsed, err := parseByteCount(text)
		if err != nil {
			return Limits{}, fmt.Errorf("%s: %w", value.name, err)
		}
		*value.target = parsed
	}
	// Backward compatibility with the original single knob: MAX_UPLOAD_BYTES
	// applies to every upload limit unless the granular variable is set.
	if text := strings.TrimSpace(os.Getenv("MAX_UPLOAD_BYTES")); text != "" {
		parsed, err := parseByteCount(text)
		if err != nil {
			return Limits{}, fmt.Errorf("MAX_UPLOAD_BYTES: %w", err)
		}
		if os.Getenv("MAX_SYNC_SIZE") == "" {
			limits.Sync = parsed
		}
		if os.Getenv("MAX_ATTACHMENT_SIZE") == "" {
			limits.Attachment = parsed
		}
		if os.Getenv("MAX_BACKUP_SIZE") == "" {
			limits.Backup = parsed
		}
	}
	return limits, nil
}

// parseByteCount accepts a plain byte count or a K/M/G suffix
// (case-insensitive), preserving the original MAX_UPLOAD_BYTES syntax.
func parseByteCount(text string) (int64, error) {
	upper := strings.ToUpper(strings.TrimSpace(text))
	multiplier := int64(1)
	for _, suffix := range []struct {
		sfx  string
		mult int64
	}{{"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}} {
		if strings.HasSuffix(upper, suffix.sfx) {
			multiplier = suffix.mult
			upper = strings.TrimSuffix(upper, suffix.sfx)
			break
		}
	}
	upper = strings.TrimSpace(upper)
	if upper == "" {
		return 0, errors.New("must be a positive byte count")
	}
	parsed, err := strconv.ParseInt(upper, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, errors.New("must be a positive byte count")
	}
	if parsed > (1<<63-1)/multiplier {
		return 0, errors.New("byte count overflows")
	}
	return parsed * multiplier, nil
}

// loadAPIKeys hashes tokens immediately so the in-memory state never contains
// plaintext credentials. It intentionally has no implicit production default.
func loadAPIKeys(path string, allowInsecureDefaults bool) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if allowInsecureDefaults && errors.Is(err, os.ErrNotExist) {
			log.Printf("WARN: API key file is missing; using explicit development credential")
			return map[string]string{"default": sha256hex([]byte("letters2my"))}, nil
		}
		return nil, fmt.Errorf("read API key file %q: %w", path, err)
	}

	keys := map[string]string{}
	tokens := map[string]string{}
	for lineNumber, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		separator := strings.IndexByte(line, ':')
		if separator <= 0 || separator == len(line)-1 {
			return nil, fmt.Errorf("API key line %d must be name:token", lineNumber+1)
		}
		rawName := line[:separator]
		rawToken := line[separator+1:]
		name := strings.TrimSpace(rawName)
		token := strings.TrimSpace(rawToken)
		if name != rawName || token != rawToken {
			return nil, fmt.Errorf("API key line %d must not contain whitespace around name or token", lineNumber+1)
		}
		if !validIdentifier(name) || strings.ContainsAny(token, " \t\r\n") || token == "" {
			return nil, fmt.Errorf("API key line %d has an invalid name or token", lineNumber+1)
		}
		if _, exists := keys[name]; exists {
			return nil, fmt.Errorf("duplicate API key name %q on line %d", name, lineNumber+1)
		}
		hash := sha256hex([]byte(token))
		if previous, exists := tokens[hash]; exists {
			return nil, fmt.Errorf("duplicate API token for %q and %q", previous, name)
		}
		keys[name] = hash
		tokens[hash] = name
	}
	if len(keys) == 0 {
		if allowInsecureDefaults {
			log.Printf("WARN: API key file has no active keys; using explicit development credential")
			return map[string]string{"default": sha256hex([]byte("letters2my"))}, nil
		}
		return nil, fmt.Errorf("API key file %q contains no active keys", path)
	}
	return keys, nil
}

func ensureDataDirs(dataDir string) error {
	if info, err := os.Stat(dataDir); err == nil && !info.IsDir() {
		return fmt.Errorf("data directory %q is not a directory", dataDir)
	}
	for _, name := range []string{"sync", "attachments", "backup"} {
		if err := os.MkdirAll(filepath.Join(dataDir, name), 0750); err != nil {
			return fmt.Errorf("create data directory %q: %w", name, err)
		}
	}
	return nil
}

// ─────────────────────────────────────────────
// Collaboration persistence
// ─────────────────────────────────────────────

func loadCollaboration(path string) (CollaborationState, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return emptyCollaborationState(), false, nil
	}
	if err != nil {
		return CollaborationState{}, false, fmt.Errorf("read collaboration state: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return CollaborationState{}, false, fmt.Errorf("%w: collaboration.json is empty", errInvalidState)
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return CollaborationState{}, false, fmt.Errorf("%w: collaboration.json must contain an object", errInvalidState)
	}
	var rawDocument map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawDocument); err != nil || rawDocument == nil {
		return CollaborationState{}, false, fmt.Errorf("%w: collaboration.json must contain an object", errInvalidState)
	}
	for _, field := range []string{"members", "invitations", "branches", "folders", "version"} {
		if value, present := rawDocument[field]; present && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return CollaborationState{}, false, fmt.Errorf("%w: collaboration.json field %q cannot be null", errInvalidState, field)
		}
	}

	state := CollaborationState{}
	if err := json.Unmarshal(data, &state); err != nil {
		return CollaborationState{}, false, fmt.Errorf("%w: decode collaboration.json: %v", errInvalidState, err)
	}
	if state.Version > currentCollaborationVersion {
		return CollaborationState{}, false, fmt.Errorf("%w: unsupported collaboration schema version %d", errInvalidState, state.Version)
	}
	if state.Version < 0 {
		return CollaborationState{}, false, fmt.Errorf("%w: invalid collaboration schema version %d", errInvalidState, state.Version)
	}
	if state.Members == nil {
		state.Members = map[string]Member{}
	}
	if state.Invitations == nil {
		state.Invitations = map[string]Invitation{}
	}
	if state.Branches == nil {
		state.Branches = map[string]Branch{}
	}
	if state.Folders == nil {
		state.Folders = map[string]Folder{}
	}

	migrated := state.Version != currentCollaborationVersion
	if migrateLegacyRoles(&state) {
		migrated = true
	}
	if normalizeState(&state) {
		migrated = true
	}
	if err := validateState(state); err != nil {
		return CollaborationState{}, false, fmt.Errorf("%w: %v", errInvalidState, err)
	}
	if state.Version != currentCollaborationVersion {
		state.Version = currentCollaborationVersion
		migrated = true
	}
	return state, migrated, nil
}

func migrateLegacyRoles(state *CollaborationState) bool {
	changed := false
	for id, member := range state.Members {
		if role, migrated := migrateRole(member.Role); migrated {
			member.Role = role
			state.Members[id] = member
			changed = true
		}
	}
	for code, invite := range state.Invitations {
		if role, migrated := migrateRole(invite.Role); migrated {
			invite.Role = role
			state.Invitations[code] = invite
			changed = true
		}
	}
	return changed
}

func normalizeState(state *CollaborationState) bool {
	changed := false
	state.Version = currentCollaborationVersion
	for id, member := range state.Members {
		if member.ID == "" {
			member.ID = id
			state.Members[id] = member
			changed = true
		}
	}
	for id, branch := range state.Branches {
		if branch.ID == "" {
			branch.ID = id
			changed = true
		}
		if branch.MemberIDs == nil {
			branch.MemberIDs = []string{}
			changed = true
		}
		unique := uniqueStrings(branch.MemberIDs)
		if len(unique) != len(branch.MemberIDs) {
			branch.MemberIDs = unique
			changed = true
		}
		state.Branches[id] = branch
	}
	for id, folder := range state.Folders {
		if folder.ID == "" {
			folder.ID = id
			changed = true
		}
		if folder.MemberIDs == nil {
			folder.MemberIDs = []string{}
			changed = true
		}
		unique := uniqueStrings(folder.MemberIDs)
		if len(unique) != len(folder.MemberIDs) {
			folder.MemberIDs = unique
			changed = true
		}
		state.Folders[id] = folder
	}
	for code, invite := range state.Invitations {
		if invite.BranchIDs == nil {
			invite.BranchIDs = []string{}
			changed = true
		}
		if invite.FolderIDs == nil {
			invite.FolderIDs = []string{}
			changed = true
		}
		state.Invitations[code] = invite
	}
	// Old handlers could leave stale member references. Prune only those
	// references; never silently replace or delete the member itself.
	for id, branch := range state.Branches {
		filtered := filterExistingMembers(branch.MemberIDs, state.Members)
		if len(filtered) != len(branch.MemberIDs) {
			branch.MemberIDs = filtered
			state.Branches[id] = branch
			changed = true
		}
	}
	for id, folder := range state.Folders {
		filtered := filterExistingMembers(folder.MemberIDs, state.Members)
		if len(filtered) != len(folder.MemberIDs) {
			folder.MemberIDs = filtered
			state.Folders[id] = folder
			changed = true
		}
	}
	return changed
}

func filterExistingMembers(ids []string, members map[string]Member) []string {
	filtered := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := members[id]; ok {
			filtered = append(filtered, id)
		}
	}
	return filtered
}

func validateState(state CollaborationState) error {
	for id, member := range state.Members {
		if !validIdentifier(id) || member.ID != id {
			return fmt.Errorf("member %q has inconsistent id", id)
		}
		if !validName(member.Name) || !validRole(member.Role) || member.Since <= 0 {
			return fmt.Errorf("member %q is invalid", id)
		}
	}
	for id, branch := range state.Branches {
		if !validIdentifier(id) || branch.ID != id || !validName(branch.Name) || !validBranchKind(branch.Kind) || branch.CreatedAt <= 0 {
			return fmt.Errorf("branch %q is invalid", id)
		}
		if err := validateMemberIDs(branch.MemberIDs, state.Members); err != nil {
			return fmt.Errorf("branch %q: %w", id, err)
		}
	}
	for id, folder := range state.Folders {
		if !validIdentifier(id) || folder.ID != id || !validName(folder.Name) || folder.CreatedAt <= 0 {
			return fmt.Errorf("folder %q is invalid", id)
		}
		if _, ok := state.Branches[folder.BranchID]; !ok {
			return fmt.Errorf("folder %q references missing branch %q", id, folder.BranchID)
		}
		if folder.ParentID != "" {
			parent, ok := state.Folders[folder.ParentID]
			if !ok || parent.BranchID != folder.BranchID || parent.ID == folder.ID {
				return fmt.Errorf("folder %q has invalid parent", id)
			}
		}
		if err := validateMemberIDs(folder.MemberIDs, state.Members); err != nil {
			return fmt.Errorf("folder %q: %w", id, err)
		}
		if hasFolderCycle(state.Folders, id) {
			return fmt.Errorf("folder %q is part of a cycle", id)
		}
	}
	for code, invite := range state.Invitations {
		if !validIdentifier(code) || invite.Code != code || !validIdentifier(invite.CreatedBy) || !validRole(invite.Role) || invite.Expires <= 0 {
			return fmt.Errorf("invitation %q is invalid", code)
		}
		if err := validateScopeIDs(invite.BranchIDs, invite.FolderIDs, state); err != nil {
			return fmt.Errorf("invitation %q: %w", code, err)
		}
	}
	return nil
}

func validateScopeIDs(branchIDs, folderIDs []string, state CollaborationState) error {
	branches := map[string]bool{}
	for _, id := range branchIDs {
		if !validIdentifier(id) {
			return fmt.Errorf("invalid branch id %q", id)
		}
		if _, ok := state.Branches[id]; !ok {
			return fmt.Errorf("missing branch %q", id)
		}
		if branches[id] {
			return fmt.Errorf("duplicate branch id %q", id)
		}
		branches[id] = true
	}
	seenFolders := map[string]bool{}
	for _, id := range folderIDs {
		if !validIdentifier(id) {
			return fmt.Errorf("invalid folder id %q", id)
		}
		folder, ok := state.Folders[id]
		if !ok {
			return fmt.Errorf("missing folder %q", id)
		}
		if len(branchIDs) > 0 && !branches[folder.BranchID] {
			return fmt.Errorf("folder %q is outside invited branches", id)
		}
		if seenFolders[id] {
			return fmt.Errorf("duplicate folder id %q", id)
		}
		seenFolders[id] = true
	}
	return nil
}

func (s *Server) saveCollaboration(state CollaborationState) error {
	state.Version = currentCollaborationVersion
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode collaboration state: %w", err)
	}
	if err := atomicWriteFile(filepath.Join(s.dataDir, "collaboration.json"), data, 0600); err != nil {
		log.Printf("[collaboration] save failed: %v", err)
		return err
	}
	return nil
}

func cloneState(state CollaborationState) CollaborationState {
	cloned := CollaborationState{
		Version:     state.Version,
		Members:     make(map[string]Member, len(state.Members)),
		Invitations: make(map[string]Invitation, len(state.Invitations)),
		Branches:    make(map[string]Branch, len(state.Branches)),
		Folders:     make(map[string]Folder, len(state.Folders)),
	}
	for id, member := range state.Members {
		cloned.Members[id] = member
	}
	for code, invite := range state.Invitations {
		invite.BranchIDs = cloneStrings(invite.BranchIDs)
		invite.FolderIDs = cloneStrings(invite.FolderIDs)
		cloned.Invitations[code] = invite
	}
	for id, branch := range state.Branches {
		branch.MemberIDs = cloneStrings(branch.MemberIDs)
		cloned.Branches[id] = branch
	}
	for id, folder := range state.Folders {
		folder.MemberIDs = cloneStrings(folder.MemberIDs)
		cloned.Folders[id] = folder
	}
	return cloned
}

func (s *Server) commitCollaborationLocked(next CollaborationState) error {
	next.Version = currentCollaborationVersion
	if err := validateState(next); err != nil {
		return err
	}
	if err := s.saveCollaboration(next); err != nil {
		return err
	}
	s.collaboration = next
	return nil
}

// ─────────────────────────────────────────────
// Routing and HTTP helpers
// ─────────────────────────────────────────────

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/status", s.auth(s.handleStatus))
	mux.HandleFunc("/sync/push/", s.auth(s.handleSyncPush))
	mux.HandleFunc("/sync/pull/", s.auth(s.handleSyncPull))
	mux.HandleFunc("/sync/list", s.auth(s.handleSyncList))
	mux.HandleFunc("/attachment/upload", s.auth(s.handleAttachmentUpload))
	mux.HandleFunc("/attachment/list", s.auth(s.handleAttachmentList))
	mux.HandleFunc("/attachment/download/", s.auth(s.handleAttachmentDownload))
	mux.HandleFunc("/attachment/", s.auth(s.handleAttachmentDelete))
	mux.HandleFunc("/backup/push", s.auth(s.handleBackupPush))
	mux.HandleFunc("/backup/pull/", s.auth(s.handleBackupPull))
	mux.HandleFunc("/backup/list", s.auth(s.handleBackupList))
	mux.HandleFunc("/backup/", s.auth(s.handleBackupDelete))
	mux.HandleFunc("/members", s.auth(s.handleMembers))
	mux.HandleFunc("/invite", s.auth(s.handleCreateInvite))
	mux.HandleFunc("/invite/", s.auth(s.handleInviteAction))
	mux.HandleFunc("/branches", s.auth(s.handleBranches))
	mux.HandleFunc("/branches/", s.auth(s.handleBranchByID))
	mux.HandleFunc("/folders", s.auth(s.handleFolders))
	mux.HandleFunc("/folders/", s.auth(s.handleFolderByID))
	return mux
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		header := r.Header.Get("Authorization")
		valid := false
		if strings.HasPrefix(header, prefix) {
			token := strings.TrimPrefix(header, prefix)
			if token != "" && !strings.ContainsAny(token, " \t\r\n") {
				hashed := sha256hex([]byte(token))
				for _, expected := range s.apiKeys {
					if subtle.ConstantTimeCompare([]byte(hashed), []byte(expected)) == 1 {
						valid = true
						break
					}
				}
			}
		}
		if !valid {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "unauthorized", "a valid Bearer token is required")
			return
		}
		next(w, r)
	}
}

func (s *Server) decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	if r.Body == nil {
		return errInvalidJSON
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, s.limits.JSONBody))
	if err := decoder.Decode(target); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return errTooLarge
		}
		return fmt.Errorf("%w: %v", errInvalidJSON, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return errTooLarge
		}
		return errTrailingJSON
	}
	return nil
}

func handleJSONDecodeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds the configured limit")
	case errors.Is(err, errTrailingJSON):
		writeError(w, http.StatusBadRequest, "invalid_json", "request body must contain exactly one JSON value")
	default:
		writeError(w, http.StatusBadRequest, "invalid_json", "request body is not valid JSON")
	}
}

func writeJSON(w http.ResponseWriter, value any) error {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	return json.NewEncoder(w).Encode(value)
}

func writeJSONStatus(w http.ResponseWriter, status int, value any) error {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	code = normalizeErrorCode(code)
	var body apiError
	body.Error.Code = code
	body.Error.Message = message
	if err := writeJSONStatus(w, status, body); err != nil {
		log.Printf("[http] encode error response failed: %v", err)
	}
}

// normalizeErrorCode maps internal diagnostics onto the stable public API v1
// vocabulary that the Swift client understands. The server logs and can
// return detailed codes; the wire keeps the client-compatible set so typed
// client errors (unauthorized, notFound, conflict, invitationExpired,
// payloadTooLarge, invalidRequest, serverError) keep working.
func normalizeErrorCode(code string) string {
	switch code {
	case "invalid_id", "invalid_scope", "invalid_member", "invalid_role",
		"invalid_branch", "invalid_folder", "invalid_platform", "invalid_creator", "invalid_json":
		return "invalid_request"
	case "member_exists", "already_exists", "owner_required":
		return "conflict"
	case "creator_not_found":
		return "not_found"
	default:
		return code
	}
}

func methodNotAllowed(w http.ResponseWriter, allowed ...string) {
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed for this endpoint")
}

func isTooLargeContentLength(r *http.Request, limit int64) bool {
	return r.ContentLength > limit
}

// ─────────────────────────────────────────────
// Status and storage
// ─────────────────────────────────────────────

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if err := s.checkStorage(); err != nil {
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "server storage is unavailable")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) checkStorage() error {
	for _, name := range []string{"sync", "attachments", "backup"} {
		path := filepath.Join(s.dataDir, name)
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", path)
		}
		if _, err := os.ReadDir(path); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if err := s.checkStorage(); err != nil {
		log.Printf("[status] storage check failed: %v", err)
		writeError(w, http.StatusInternalServerError, "storage_unavailable", "server storage is unavailable")
		return
	}
	syncs, err := s.listSyncs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "storage_unavailable", "unable to list sync snapshots")
		return
	}
	attachments, err := s.listAttachments()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "storage_unavailable", "unable to list attachments")
		return
	}
	backups, err := s.listBackups()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "storage_unavailable", "unable to list backups")
		return
	}
	s.mu.RLock()
	branches := len(s.collaboration.Branches)
	folders := len(s.collaboration.Folders)
	members := len(s.collaboration.Members)
	invitations := len(s.collaboration.Invitations)
	s.mu.RUnlock()
	response := StatusResponse{
		Service:       serviceName,
		APIVersion:    apiVersion,
		ServerVersion: s.version,
		Capabilities:  capabilities,
		Syncs:         syncs,
		Attachments:   attachments,
		Recoveries:    backups,
		Branches:      branches,
		Folders:       folders,
		Members:       members,
		Invitations:   invitations,
		Uptime:        int64(time.Since(s.startedAt) / time.Second),
	}
	if err := writeJSON(w, response); err != nil {
		log.Printf("[status] response failed: %v", err)
	}
}

func (s *Server) listSyncs() ([]SyncPayload, error) {
	entries, err := listRegularFiles(filepath.Join(s.dataDir, "sync"))
	if err != nil {
		return nil, err
	}
	result := make([]SyncPayload, 0)
	for _, entry := range entries {
		if !strings.HasSuffix(entry.name, "-letters.db") {
			continue
		}
		platform := strings.TrimSuffix(entry.name, "-letters.db")
		if !validPlatform(platform) {
			continue
		}
		result = append(result, SyncPayload{Platform: platform, Timestamp: entry.modTime.UnixMilli(), Size: entry.size, Kind: "device-snapshot"})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Platform < result[j].Platform })
	return result, nil
}

func (s *Server) listAttachments() ([]AttachmentMeta, error) {
	entries, err := listRegularFiles(filepath.Join(s.dataDir, "attachments"))
	if err != nil {
		return nil, err
	}
	result := make([]AttachmentMeta, 0, len(entries))
	for _, entry := range entries {
		if validIdentifier(entry.name) {
			result = append(result, AttachmentMeta{ID: entry.name, ContentType: contentTypeFor(entry.name), Size: entry.size})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

// contentTypeFor returns a best-effort MIME type for an attachment id based
// on its file extension, defaulting to octet-stream. The Swift client only
// requires a present string field; the value is informational.
func contentTypeFor(name string) string {
	lower := strings.ToLower(name)
	for suffix, mime := range map[string]string{
		".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png",
		".gif": "image/gif", ".heic": "image/heic", ".mp4": "video/mp4",
		".mov": "video/quicktime", ".m4a": "audio/mp4", ".mp3": "audio/mpeg",
		".pdf": "application/pdf", ".txt": "text/plain",
	} {
		if strings.HasSuffix(lower, suffix) {
			return mime
		}
	}
	return "application/octet-stream"
}

func (s *Server) listBackups() ([]RecoveryMeta, error) {
	entries, err := listRegularFiles(filepath.Join(s.dataDir, "backup"))
	if err != nil {
		return nil, err
	}
	result := make([]RecoveryMeta, 0, len(entries))
	for _, entry := range entries {
		if !strings.HasSuffix(entry.name, ".letterstomy") {
			continue
		}
		id := strings.TrimSuffix(entry.name, ".letterstomy")
		if validIdentifier(id) {
			result = append(result, RecoveryMeta{
				ID:          id,
				Timestamp:   entry.modTime.UnixMilli(),
				Size:        entry.size,
				LetterCount: s.readBackupMetaLocked(id),
			})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Timestamp == result[j].Timestamp {
			return result[i].ID < result[j].ID
		}
		return result[i].Timestamp > result[j].Timestamp
	})
	return result, nil
}

// writeBackupMetaLocked persists a small sidecar (backup/<id>.letterstomy.meta)
// holding the client-supplied letter_count hint. Callers hold s.backupMu.
func (s *Server) writeBackupMetaLocked(id string, timestamp int64, letterCount int) error {
	data, err := json.Marshal(struct {
		Timestamp   int64 `json:"timestamp"`
		LetterCount int   `json:"letter_count"`
	}{Timestamp: timestamp, LetterCount: letterCount})
	if err != nil {
		return err
	}
	return atomicWriteFile(filepath.Join(s.dataDir, "backup", id+".letterstomy.meta"), data, 0600)
}

// readBackupMetaLocked reads the sidecar; the archive file itself is the
// source of truth for timestamp/size, so a missing or corrupt sidecar
// degrades to zero letters rather than failing the list.
func (s *Server) readBackupMetaLocked(id string) int {
	data, err := os.ReadFile(filepath.Join(s.dataDir, "backup", id+".letterstomy.meta"))
	if err != nil {
		return 0
	}
	var meta struct {
		Timestamp   int64 `json:"timestamp"`
		LetterCount int   `json:"letter_count"`
	}
	if json.Unmarshal(data, &meta) != nil || meta.LetterCount < 0 {
		return 0
	}
	return meta.LetterCount
}

type regularFile struct {
	name    string
	size    int64
	modTime time.Time
}

func listRegularFiles(dir string) ([]regularFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	result := make([]regularFile, 0, len(entries))
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		result = append(result, regularFile{name: entry.Name(), size: info.Size(), modTime: info.ModTime()})
	}
	return result, nil
}

// ─────────────────────────────────────────────
// Snapshot, attachment, and backup uploads
// ─────────────────────────────────────────────

func (s *Server) handleSyncPush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		methodNotAllowed(w, http.MethodPut)
		return
	}
	platform, ok := pathIdentifier(r.URL.Path, "/sync/push/")
	if !ok || !validPlatform(platform) {
		writeError(w, http.StatusBadRequest, "invalid_platform", "platform must be one of ios, android, or web")
		return
	}
	if isTooLargeContentLength(r, s.limits.Sync) {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "sync snapshot exceeds the configured limit")
		return
	}
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	result, err := atomicCopy(filepath.Join(s.dataDir, "sync", platform+"-letters.db"), r.Body, s.limits.Sync, 0600)
	if err != nil {
		if errors.Is(err, errTooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "sync snapshot exceeds the configured limit")
		} else {
			log.Printf("[sync] push %s failed: %v", platform, err)
			writeError(w, http.StatusInternalServerError, "storage_failure", "unable to store sync snapshot")
		}
		return
	}
	result.ID = platform
	log.Printf("[sync] push %s size=%d sha256=%s", platform, result.Size, result.SHA256)
	if err := writeJSON(w, result); err != nil {
		log.Printf("[sync] response failed: %v", err)
	}
}

func (s *Server) handleSyncPull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	platform, ok := pathIdentifier(r.URL.Path, "/sync/pull/")
	if !ok || !validPlatform(platform) {
		writeError(w, http.StatusBadRequest, "invalid_platform", "platform must be one of ios, android, or web")
		return
	}
	s.serveStored(w, r, filepath.Join(s.dataDir, "sync", platform+"-letters.db"), "sync snapshot not found")
}

func (s *Server) handleSyncList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	result, err := s.listSyncs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "storage_unavailable", "unable to list sync snapshots")
		return
	}
	if err := writeJSON(w, result); err != nil {
		log.Printf("[sync] list response failed: %v", err)
	}
}

func (s *Server) handleAttachmentUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		methodNotAllowed(w, http.MethodPut)
		return
	}
	id := r.URL.Query().Get("id")
	if !validIdentifier(id) {
		writeError(w, http.StatusBadRequest, "invalid_id", "a safe attachment id is required")
		return
	}
	if isTooLargeContentLength(r, s.limits.Attachment) {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "attachment exceeds the configured limit")
		return
	}
	s.attachmentMu.Lock()
	defer s.attachmentMu.Unlock()
	result, err := atomicCopy(filepath.Join(s.dataDir, "attachments", id), r.Body, s.limits.Attachment, 0600)
	if err != nil {
		if errors.Is(err, errTooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "attachment exceeds the configured limit")
		} else {
			log.Printf("[attachment] upload %s failed: %v", id, err)
			writeError(w, http.StatusInternalServerError, "storage_failure", "unable to store attachment")
		}
		return
	}
	result.ID = id
	log.Printf("[attachment] upload %s size=%d sha256=%s", id, result.Size, result.SHA256)
	if err := writeJSON(w, result); err != nil {
		log.Printf("[attachment] response failed: %v", err)
	}
}

func (s *Server) handleAttachmentDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	id, ok := pathIdentifier(r.URL.Path, "/attachment/download/")
	if !ok || !validIdentifier(id) {
		writeError(w, http.StatusBadRequest, "invalid_id", "a safe attachment id is required")
		return
	}
	s.serveStored(w, r, filepath.Join(s.dataDir, "attachments", id), "attachment not found")
}

func (s *Server) handleAttachmentList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	result, err := s.listAttachments()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "storage_unavailable", "unable to list attachments")
		return
	}
	if err := writeJSON(w, result); err != nil {
		log.Printf("[attachment] list response failed: %v", err)
	}
}

func (s *Server) handleAttachmentDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		methodNotAllowed(w, http.MethodDelete)
		return
	}
	id, ok := pathIdentifier(r.URL.Path, "/attachment/")
	if !ok || !validIdentifier(id) {
		writeError(w, http.StatusBadRequest, "invalid_id", "a safe attachment id is required")
		return
	}
	s.attachmentMu.Lock()
	defer s.attachmentMu.Unlock()
	if err := os.Remove(filepath.Join(s.dataDir, "attachments", id)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "not_found", "attachment not found")
			return
		}
		log.Printf("[attachment] delete %s failed: %v", id, err)
		writeError(w, http.StatusInternalServerError, "storage_failure", "unable to delete attachment")
		return
	}
	log.Printf("[attachment] deleted id=%s", id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleBackupPush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		methodNotAllowed(w, http.MethodPut)
		return
	}
	id := r.URL.Query().Get("id")
	letterCount := 0
	if raw := r.URL.Query().Get("letter_count"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "letter_count must be a non-negative integer")
			return
		}
		letterCount = parsed
	}
	s.backupMu.Lock()
	defer s.backupMu.Unlock()
	if id == "" {
		var err error
		id, err = s.newBackupIDLocked()
		if err != nil {
			log.Printf("[backup] generate id failed: %v", err)
			writeError(w, http.StatusInternalServerError, "id_generation_failure", "unable to generate backup id")
			return
		}
	} else if !validIdentifier(id) {
		writeError(w, http.StatusBadRequest, "invalid_id", "a safe backup id is required")
		return
	}
	if isTooLargeContentLength(r, s.limits.Backup) {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "backup exceeds the configured limit")
		return
	}
	result, err := atomicCopy(filepath.Join(s.dataDir, "backup", id+".letterstomy"), r.Body, s.limits.Backup, 0600)
	if err != nil {
		if errors.Is(err, errTooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "backup exceeds the configured limit")
		} else {
			log.Printf("[backup] push %s failed: %v", id, err)
			writeError(w, http.StatusInternalServerError, "storage_failure", "unable to store backup")
		}
		return
	}
	result.ID = id
	timestamp := s.now().UnixMilli()
	if err := s.writeBackupMetaLocked(id, timestamp, letterCount); err != nil {
		log.Printf("[backup] meta %s failed: %v", id, err)
		writeError(w, http.StatusInternalServerError, "storage_failure", "unable to store backup metadata")
		return
	}
	log.Printf("[backup] push %s size=%d sha256=%s letters=%d", id, result.Size, result.SHA256, letterCount)
	if err := writeJSON(w, struct {
		ID          string `json:"id"`
		Timestamp   int64  `json:"timestamp"`
		Size        int64  `json:"size"`
		LetterCount int    `json:"letter_count"`
		SHA256      string `json:"sha256"`
	}{ID: id, Timestamp: timestamp, Size: result.Size, LetterCount: letterCount, SHA256: result.SHA256}); err != nil {
		log.Printf("[backup] response failed: %v", err)
	}
}

func (s *Server) newBackupIDLocked() (string, error) {
	for attempt := 0; attempt < 10; attempt++ {
		randomPart := make([]byte, 8)
		if _, err := rand.Read(randomPart); err != nil {
			return "", err
		}
		id := fmt.Sprintf("backup-%d-%s", s.now().UnixMilli(), hex.EncodeToString(randomPart))
		_, err := os.Lstat(filepath.Join(s.dataDir, "backup", id+".letterstomy"))
		if errors.Is(err, os.ErrNotExist) {
			return id, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", errors.New("could not find an unused backup id")
}

func (s *Server) handleBackupPull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	id, ok := pathIdentifier(r.URL.Path, "/backup/pull/")
	if !ok || !validIdentifier(id) {
		writeError(w, http.StatusBadRequest, "invalid_id", "a safe backup id is required")
		return
	}
	s.serveStored(w, r, filepath.Join(s.dataDir, "backup", id+".letterstomy"), "backup not found")
}

func (s *Server) handleBackupList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	result, err := s.listBackups()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "storage_unavailable", "unable to list backups")
		return
	}
	if err := writeJSON(w, result); err != nil {
		log.Printf("[backup] list response failed: %v", err)
	}
}

func (s *Server) handleBackupDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		methodNotAllowed(w, http.MethodDelete)
		return
	}
	id, ok := pathIdentifier(r.URL.Path, "/backup/")
	if !ok || !validIdentifier(id) {
		writeError(w, http.StatusBadRequest, "invalid_id", "a safe backup id is required")
		return
	}
	s.backupMu.Lock()
	defer s.backupMu.Unlock()
	if err := os.Remove(filepath.Join(s.dataDir, "backup", id+".letterstomy")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "not_found", "backup not found")
			return
		}
		log.Printf("[backup] delete %s failed: %v", id, err)
		writeError(w, http.StatusInternalServerError, "storage_failure", "unable to delete backup")
		return
	}
	// Best-effort cleanup of the metadata sidecar.
	_ = os.Remove(filepath.Join(s.dataDir, "backup", id+".letterstomy.meta"))
	log.Printf("[backup] deleted id=%s", id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) serveStored(w http.ResponseWriter, r *http.Request, path, notFoundMessage string) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "not_found", notFoundMessage)
		return
	}
	if err != nil {
		log.Printf("[storage] stat %s failed: %v", filepath.Base(path), err)
		writeError(w, http.StatusInternalServerError, "storage_failure", "unable to access stored content")
		return
	}
	if !info.Mode().IsRegular() {
		writeError(w, http.StatusNotFound, "not_found", notFoundMessage)
		return
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "not_found", notFoundMessage)
			return
		}
		writeError(w, http.StatusInternalServerError, "storage_failure", "unable to access stored content")
		return
	}
	defer file.Close()
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, filepath.Base(path), info.ModTime(), file)
}

// ─────────────────────────────────────────────
// Members and invitations
// ─────────────────────────────────────────────

func (s *Server) handleMembers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.RLock()
		members := make([]Member, 0, len(s.collaboration.Members))
		for _, member := range s.collaboration.Members {
			members = append(members, member)
		}
		s.mu.RUnlock()
		sort.Slice(members, func(i, j int) bool {
			if members[i].Since == members[j].Since {
				return members[i].ID < members[j].ID
			}
			return members[i].Since < members[j].Since
		})
		if err := writeJSON(w, members); err != nil {
			log.Printf("[members] list response failed: %v", err)
		}

	case http.MethodPut:
		var member Member
		if err := s.decodeJSON(w, r, &member); err != nil {
			handleJSONDecodeError(w, err)
			return
		}
		if !validIdentifier(member.ID) || !validName(member.Name) {
			writeError(w, http.StatusBadRequest, "invalid_member", "id and name must be valid")
			return
		}
		if !validRole(member.Role) {
			writeError(w, http.StatusBadRequest, "invalid_role", "role must be owner, parentAdmin, organizer, contributor, viewer, or recipient")
			return
		}
		s.mu.Lock()
		next := cloneState(s.collaboration)
		previous, exists := next.Members[member.ID]
		if exists {
			member.Since = previous.Since
		} else {
			member.Since = s.now().UnixMilli()
		}
		next.Members[member.ID] = member
		if err := s.commitCollaborationLocked(next); err != nil {
			s.mu.Unlock()
			writeError(w, http.StatusInternalServerError, "storage_failure", "unable to save member")
			return
		}
		s.mu.Unlock()
		log.Printf("[members] upsert id=%s role=%s", member.ID, member.Role)
		if err := writeJSON(w, member); err != nil {
			log.Printf("[members] response failed: %v", err)
		}

	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if !validIdentifier(id) {
			writeError(w, http.StatusBadRequest, "invalid_id", "a valid member id is required")
			return
		}
		s.mu.Lock()
		next := cloneState(s.collaboration)
		member, exists := next.Members[id]
		if !exists {
			s.mu.Unlock()
			writeError(w, http.StatusNotFound, "not_found", "member does not exist")
			return
		}
		if member.Role == RoleOwner && countOwners(next.Members) == 1 {
			s.mu.Unlock()
			writeError(w, http.StatusConflict, "owner_required", "the last owner cannot be removed")
			return
		}
		delete(next.Members, id)
		for branchID, branch := range next.Branches {
			branch.MemberIDs = removeString(branch.MemberIDs, id)
			next.Branches[branchID] = branch
		}
		for folderID, folder := range next.Folders {
			folder.MemberIDs = removeString(folder.MemberIDs, id)
			next.Folders[folderID] = folder
		}
		for code, invite := range next.Invitations {
			if invite.CreatedBy == id {
				delete(next.Invitations, code)
			}
		}
		if err := s.commitCollaborationLocked(next); err != nil {
			s.mu.Unlock()
			writeError(w, http.StatusInternalServerError, "storage_failure", "unable to save member removal")
			return
		}
		s.mu.Unlock()
		log.Printf("[members] removed id=%s", id)
		w.WriteHeader(http.StatusNoContent)

	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
	}
}

func (s *Server) handleCreateInvite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var request struct {
		CreatedBy string   `json:"created_by"`
		Role      Role     `json:"role"`
		BranchIDs []string `json:"branch_ids"`
		FolderIDs []string `json:"folder_ids"`
	}
	if err := s.decodeJSON(w, r, &request); err != nil {
		handleJSONDecodeError(w, err)
		return
	}
	if !validIdentifier(request.CreatedBy) {
		writeError(w, http.StatusBadRequest, "invalid_creator", "created_by must be a valid member id")
		return
	}
	// API v1 committed behavior: the creator does not need to exist as a
	// member yet (the client's capability probe creates invites for
	// not-yet-accepted members), and an omitted role defaults to viewer
	// (least privilege).
	inviteRole := request.Role
	if inviteRole == "" {
		inviteRole = RoleViewer
	}
	if !validRole(inviteRole) {
		writeError(w, http.StatusBadRequest, "invalid_role", "role must be owner, parentAdmin, organizer, contributor, viewer, or recipient")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneState(s.collaboration)
	if hasDuplicates(request.BranchIDs) || hasDuplicates(request.FolderIDs) {
		writeError(w, http.StatusBadRequest, "invalid_scope", "scope IDs must be unique")
		return
	}
	if request.BranchIDs == nil {
		request.BranchIDs = []string{}
	}
	if request.FolderIDs == nil {
		request.FolderIDs = []string{}
	}
	if err := validateScopeIDs(request.BranchIDs, request.FolderIDs, next); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_scope", err.Error())
		return
	}
	for code, invite := range next.Invitations {
		if invite.Expires > 0 && s.now().UnixMilli() > invite.Expires {
			delete(next.Invitations, code)
		}
	}
	code, err := newInviteCodeLocked(next.Invitations)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "id_generation_failure", "unable to generate invitation code")
		return
	}
	invite := Invitation{
		Code:      code,
		CreatedBy: request.CreatedBy,
		Role:      request.Role,
		BranchIDs: request.BranchIDs,
		FolderIDs: request.FolderIDs,
		Expires:   s.now().Add(7 * 24 * time.Hour).UnixMilli(),
	}
	next.Invitations[code] = invite
	if err := s.commitCollaborationLocked(next); err != nil {
		writeError(w, http.StatusInternalServerError, "storage_failure", "unable to save invitation")
		return
	}
	log.Printf("[invite] created code=%s creator=%s role=%s", code, request.CreatedBy, request.Role)
	if err := writeJSONStatus(w, http.StatusCreated, invite); err != nil {
		log.Printf("[invite] response failed: %v", err)
	}
}

func newInviteCodeLocked(existing map[string]Invitation) (string, error) {
	for attempt := 0; attempt < 10; attempt++ {
		codeBytes := make([]byte, 6)
		if _, err := rand.Read(codeBytes); err != nil {
			return "", err
		}
		code := strings.ToUpper(hex.EncodeToString(codeBytes))
		if _, exists := existing[code]; !exists {
			return code, nil
		}
	}
	return "", errors.New("could not find an unused invitation code")
}

func (s *Server) handleInviteAction(w http.ResponseWriter, r *http.Request) {
	code, ok := pathIdentifier(r.URL.Path, "/invite/")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_id", "invitation code is required")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.mu.RLock()
		invite, exists := s.collaboration.Invitations[code]
		s.mu.RUnlock()
		if !exists {
			writeError(w, http.StatusNotFound, "not_found", "invitation does not exist")
			return
		}
		if invite.Expires > 0 && s.now().UnixMilli() > invite.Expires {
			if err := s.expireInvitation(code); err != nil {
				writeError(w, http.StatusInternalServerError, "storage_failure", "unable to remove expired invitation")
				return
			}
			writeError(w, http.StatusGone, "expired", "invitation has expired")
			return
		}
		if err := writeJSON(w, invite); err != nil {
			log.Printf("[invite] lookup response failed: %v", err)
		}

	case http.MethodPost:
		var request struct {
			MemberID   string `json:"member_id"`
			MemberName string `json:"member_name"`
		}
		if err := s.decodeJSON(w, r, &request); err != nil {
			handleJSONDecodeError(w, err)
			return
		}
		if !validIdentifier(request.MemberID) || !validName(request.MemberName) {
			writeError(w, http.StatusBadRequest, "invalid_member", "member_id and member_name are required")
			return
		}
		s.mu.Lock()
		next := cloneState(s.collaboration)
		invite, exists := next.Invitations[code]
		if !exists {
			s.mu.Unlock()
			writeError(w, http.StatusNotFound, "not_found", "invitation does not exist")
			return
		}
		if invite.Expires > 0 && s.now().UnixMilli() > invite.Expires {
			s.mu.Unlock()
			if err := s.expireInvitation(code); err != nil {
				writeError(w, http.StatusInternalServerError, "storage_failure", "unable to remove expired invitation")
				return
			}
			writeError(w, http.StatusGone, "expired", "invitation has expired")
			return
		}
		if _, exists := next.Members[request.MemberID]; exists {
			s.mu.Unlock()
			writeError(w, http.StatusConflict, "member_exists", "member already exists")
			return
		}
		if err := validateScopeIDs(invite.BranchIDs, invite.FolderIDs, next); err != nil {
			s.mu.Unlock()
			writeError(w, http.StatusConflict, "invalid_scope", "invitation references deleted data")
			return
		}
		member := Member{ID: request.MemberID, Name: request.MemberName, Role: invite.Role, Since: s.now().UnixMilli()}
		next.Members[member.ID] = member
		for _, branchID := range invite.BranchIDs {
			branch := next.Branches[branchID]
			branch.MemberIDs = appendIfMissing(branch.MemberIDs, member.ID)
			next.Branches[branchID] = branch
		}
		for _, folderID := range invite.FolderIDs {
			folder := next.Folders[folderID]
			folder.MemberIDs = appendIfMissing(folder.MemberIDs, member.ID)
			next.Folders[folderID] = folder
		}
		delete(next.Invitations, code)
		if err := s.commitCollaborationLocked(next); err != nil {
			s.mu.Unlock()
			writeError(w, http.StatusInternalServerError, "storage_failure", "unable to save invitation acceptance")
			return
		}
		s.mu.Unlock()
		log.Printf("[invite] accepted code=%s member=%s role=%s", code, member.ID, member.Role)
		if err := writeJSONStatus(w, http.StatusOK, map[string]any{
			"member_id":  member.ID,
			"role":       member.Role,
			"branch_ids": invite.BranchIDs,
			"folder_ids": invite.FolderIDs,
			"status":     "accepted",
		}); err != nil {
			log.Printf("[invite] acceptance response failed: %v", err)
		}

	case http.MethodDelete:
		s.mu.Lock()
		next := cloneState(s.collaboration)
		if _, exists := next.Invitations[code]; !exists {
			s.mu.Unlock()
			writeError(w, http.StatusNotFound, "not_found", "invitation does not exist")
			return
		}
		delete(next.Invitations, code)
		if err := s.commitCollaborationLocked(next); err != nil {
			s.mu.Unlock()
			writeError(w, http.StatusInternalServerError, "storage_failure", "unable to revoke invitation")
			return
		}
		s.mu.Unlock()
		log.Printf("[invite] revoked code=%s", code)
		w.WriteHeader(http.StatusNoContent)

	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPost, http.MethodDelete)
	}
}

func (s *Server) expireInvitation(code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneState(s.collaboration)
	delete(next.Invitations, code)
	return s.commitCollaborationLocked(next)
}

// ─────────────────────────────────────────────
// Branches and folders
// ─────────────────────────────────────────────

var branchKinds = map[string]bool{
	"parents":      true,
	"maternal":     true,
	"paternal":     true,
	"chosenFamily": true,
	"custom":       true,
}

func validBranchKind(kind string) bool { return branchKinds[kind] }

type branchInput struct {
	ID        *string   `json:"id"`
	Name      *string   `json:"name"`
	Kind      *string   `json:"kind"`
	IsSeeded  *bool     `json:"is_seeded"`
	MemberIDs *[]string `json:"member_ids"`
	CreatedAt *int64    `json:"created_at"`
}

func (input branchInput) hasMutableField() bool {
	return input.Name != nil || input.Kind != nil || input.IsSeeded != nil || input.MemberIDs != nil
}

func (s *Server) handleBranches(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.RLock()
		branches := make([]Branch, 0, len(s.collaboration.Branches))
		for _, branch := range s.collaboration.Branches {
			branches = append(branches, branch)
		}
		s.mu.RUnlock()
		sort.Slice(branches, func(i, j int) bool {
			if branches[i].CreatedAt == branches[j].CreatedAt {
				return branches[i].ID < branches[j].ID
			}
			return branches[i].CreatedAt < branches[j].CreatedAt
		})
		if err := writeJSON(w, branches); err != nil {
			log.Printf("[branches] list response failed: %v", err)
		}

	case http.MethodPost:
		var input branchInput
		if err := s.decodeJSON(w, r, &input); err != nil {
			handleJSONDecodeError(w, err)
			return
		}
		branch, err := s.branchFromInput(input)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_branch", err.Error())
			return
		}
		s.mu.Lock()
		next := cloneState(s.collaboration)
		if _, exists := next.Branches[branch.ID]; exists {
			s.mu.Unlock()
			writeError(w, http.StatusConflict, "already_exists", "branch already exists")
			return
		}
		if err := validateMemberIDs(branch.MemberIDs, next.Members); err != nil {
			s.mu.Unlock()
			writeError(w, http.StatusBadRequest, "invalid_branch", err.Error())
			return
		}
		next.Branches[branch.ID] = branch
		if err := s.commitCollaborationLocked(next); err != nil {
			s.mu.Unlock()
			writeError(w, http.StatusInternalServerError, "storage_failure", "unable to save branch")
			return
		}
		s.mu.Unlock()
		log.Printf("[branches] created id=%s", branch.ID)
		if err := writeJSONStatus(w, http.StatusCreated, branch); err != nil {
			log.Printf("[branches] response failed: %v", err)
		}

	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if !validIdentifier(id) {
			writeError(w, http.StatusBadRequest, "invalid_id", "a valid branch id is required")
			return
		}
		s.deleteBranch(w, id)

	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPost, http.MethodDelete)
	}
}

func (s *Server) branchFromInput(input branchInput) (Branch, error) {
	if input.ID == nil || !validIdentifier(valueOrEmpty(input.ID)) {
		return Branch{}, fmt.Errorf("id is required")
	}
	if input.Name == nil || !validName(valueOrEmpty(input.Name)) {
		return Branch{}, fmt.Errorf("name is required")
	}
	kind := "custom"
	if input.Kind != nil {
		kind = *input.Kind
	}
	if !validBranchKind(kind) {
		return Branch{}, fmt.Errorf("kind must be parents, maternal, paternal, chosenFamily, or custom")
	}
	members := []string{}
	if input.MemberIDs != nil {
		if hasDuplicates(*input.MemberIDs) {
			return Branch{}, fmt.Errorf("member IDs must be unique")
		}
		members = cloneStrings(*input.MemberIDs)
	}
	for _, id := range members {
		if !validIdentifier(id) {
			return Branch{}, fmt.Errorf("invalid member id %q", id)
		}
	}
	createdAt := s.now().UnixMilli()
	if input.CreatedAt != nil && *input.CreatedAt > 0 {
		createdAt = *input.CreatedAt
	}
	seeded := false
	if input.IsSeeded != nil {
		seeded = *input.IsSeeded
	}
	return Branch{ID: *input.ID, Name: *input.Name, Kind: kind, IsSeeded: seeded, MemberIDs: members, CreatedAt: createdAt}, nil
}

func (s *Server) handleBranchByID(w http.ResponseWriter, r *http.Request) {
	id, ok := pathIdentifier(r.URL.Path, "/branches/")
	if !ok || !validIdentifier(id) {
		writeError(w, http.StatusBadRequest, "invalid_id", "a valid branch id is required")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.mu.RLock()
		branch, exists := s.collaboration.Branches[id]
		s.mu.RUnlock()
		if !exists {
			writeError(w, http.StatusNotFound, "not_found", "branch does not exist")
			return
		}
		if err := writeJSON(w, branch); err != nil {
			log.Printf("[branches] response failed: %v", err)
		}

	case http.MethodPut:
		var input branchInput
		if err := s.decodeJSON(w, r, &input); err != nil {
			handleJSONDecodeError(w, err)
			return
		}
		if !input.hasMutableField() {
			writeError(w, http.StatusBadRequest, "invalid_branch", "at least one mutable branch field is required")
			return
		}
		s.mu.Lock()
		next := cloneState(s.collaboration)
		branch, exists := next.Branches[id]
		if !exists {
			s.mu.Unlock()
			writeError(w, http.StatusNotFound, "not_found", "branch does not exist")
			return
		}
		if input.Name != nil {
			if !validName(*input.Name) {
				s.mu.Unlock()
				writeError(w, http.StatusBadRequest, "invalid_branch", "name is invalid")
				return
			}
			branch.Name = *input.Name
		}
		if input.Kind != nil {
			if !validBranchKind(*input.Kind) {
				s.mu.Unlock()
				writeError(w, http.StatusBadRequest, "invalid_branch", "kind is invalid")
				return
			}
			branch.Kind = *input.Kind
		}
		if input.IsSeeded != nil {
			branch.IsSeeded = *input.IsSeeded
		}
		if input.MemberIDs != nil {
			if hasDuplicates(*input.MemberIDs) {
				s.mu.Unlock()
				writeError(w, http.StatusBadRequest, "invalid_branch", "member IDs must be unique")
				return
			}
			branch.MemberIDs = cloneStrings(*input.MemberIDs)
			if err := validateMemberIDs(branch.MemberIDs, next.Members); err != nil {
				s.mu.Unlock()
				writeError(w, http.StatusBadRequest, "invalid_branch", err.Error())
				return
			}
		}
		branch.ID = id
		next.Branches[id] = branch
		if err := s.commitCollaborationLocked(next); err != nil {
			s.mu.Unlock()
			writeError(w, http.StatusInternalServerError, "storage_failure", "unable to save branch")
			return
		}
		s.mu.Unlock()
		if err := writeJSON(w, branch); err != nil {
			log.Printf("[branches] response failed: %v", err)
		}

	case http.MethodDelete:
		s.deleteBranch(w, id)

	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
	}
}

func (s *Server) deleteBranch(w http.ResponseWriter, id string) {
	s.mu.Lock()
	next := cloneState(s.collaboration)
	if _, exists := next.Branches[id]; !exists {
		s.mu.Unlock()
		writeError(w, http.StatusNotFound, "not_found", "branch does not exist")
		return
	}
	deletedFolders := map[string]bool{}
	for folderID, folder := range next.Folders {
		if folder.BranchID == id {
			deletedFolders[folderID] = true
			delete(next.Folders, folderID)
		}
	}
	delete(next.Branches, id)
	removeInvitationsForScope(&next, id, deletedFolders)
	if err := s.commitCollaborationLocked(next); err != nil {
		s.mu.Unlock()
		writeError(w, http.StatusInternalServerError, "storage_failure", "unable to save branch removal")
		return
	}
	s.mu.Unlock()
	log.Printf("[branches] deleted id=%s", id)
	w.WriteHeader(http.StatusNoContent)
}

type folderInput struct {
	ID        *string         `json:"id"`
	BranchID  *string         `json:"branch_id"`
	ParentID  json.RawMessage `json:"parent_id"`
	Name      *string         `json:"name"`
	MemberIDs *[]string       `json:"member_ids"`
	CreatedAt *int64          `json:"created_at"`
}

func (input folderInput) hasMutableField() bool {
	return input.BranchID != nil || input.ParentID != nil || input.Name != nil || input.MemberIDs != nil
}

func (s *Server) handleFolders(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		branchID := r.URL.Query().Get("branch_id")
		if branchID != "" && !validIdentifier(branchID) {
			writeError(w, http.StatusBadRequest, "invalid_id", "branch_id is invalid")
			return
		}
		s.mu.RLock()
		folders := make([]Folder, 0)
		for _, folder := range s.collaboration.Folders {
			if branchID == "" || folder.BranchID == branchID {
				folders = append(folders, folder)
			}
		}
		s.mu.RUnlock()
		sort.Slice(folders, func(i, j int) bool {
			if folders[i].CreatedAt == folders[j].CreatedAt {
				return folders[i].ID < folders[j].ID
			}
			return folders[i].CreatedAt < folders[j].CreatedAt
		})
		if err := writeJSON(w, folders); err != nil {
			log.Printf("[folders] list response failed: %v", err)
		}

	case http.MethodPost:
		var input folderInput
		if err := s.decodeJSON(w, r, &input); err != nil {
			handleJSONDecodeError(w, err)
			return
		}
		folder, err := s.folderFromInput(input)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_folder", err.Error())
			return
		}
		s.mu.Lock()
		next := cloneState(s.collaboration)
		if _, exists := next.Folders[folder.ID]; exists {
			s.mu.Unlock()
			writeError(w, http.StatusConflict, "already_exists", "folder already exists")
			return
		}
		if err := validateFolder(next, folder); err != nil {
			s.mu.Unlock()
			writeError(w, http.StatusBadRequest, "invalid_folder", err.Error())
			return
		}
		if err := validateMemberIDs(folder.MemberIDs, next.Members); err != nil {
			s.mu.Unlock()
			writeError(w, http.StatusBadRequest, "invalid_folder", err.Error())
			return
		}
		next.Folders[folder.ID] = folder
		if err := s.commitCollaborationLocked(next); err != nil {
			s.mu.Unlock()
			writeError(w, http.StatusInternalServerError, "storage_failure", "unable to save folder")
			return
		}
		s.mu.Unlock()
		log.Printf("[folders] created id=%s branch=%s", folder.ID, folder.BranchID)
		if err := writeJSONStatus(w, http.StatusCreated, folder); err != nil {
			log.Printf("[folders] response failed: %v", err)
		}

	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if !validIdentifier(id) {
			writeError(w, http.StatusBadRequest, "invalid_id", "a valid folder id is required")
			return
		}
		s.deleteFolder(w, id)

	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPost, http.MethodDelete)
	}
}

func (s *Server) folderFromInput(input folderInput) (Folder, error) {
	if input.ID == nil || !validIdentifier(valueOrEmpty(input.ID)) {
		return Folder{}, fmt.Errorf("id is required")
	}
	if input.BranchID == nil || !validIdentifier(valueOrEmpty(input.BranchID)) {
		return Folder{}, fmt.Errorf("branch_id is required")
	}
	if input.Name == nil || !validName(valueOrEmpty(input.Name)) {
		return Folder{}, fmt.Errorf("name is required")
	}
	parent, present, err := parseParentID(input.ParentID)
	if err != nil {
		return Folder{}, err
	}
	if !present {
		parent = ""
	}
	members := []string{}
	if input.MemberIDs != nil {
		if hasDuplicates(*input.MemberIDs) {
			return Folder{}, fmt.Errorf("member IDs must be unique")
		}
		members = cloneStrings(*input.MemberIDs)
	}
	for _, id := range members {
		if !validIdentifier(id) {
			return Folder{}, fmt.Errorf("invalid member id %q", id)
		}
	}
	createdAt := s.now().UnixMilli()
	if input.CreatedAt != nil && *input.CreatedAt > 0 {
		createdAt = *input.CreatedAt
	}
	return Folder{ID: *input.ID, BranchID: *input.BranchID, ParentID: parent, Name: *input.Name, MemberIDs: members, CreatedAt: createdAt}, nil
}

func parseParentID(raw json.RawMessage) (string, bool, error) {
	if len(raw) == 0 {
		return "", false, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", true, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || !validIdentifier(value) {
		return "", true, fmt.Errorf("parent_id must be null or a valid folder id")
	}
	return value, true, nil
}

func (s *Server) handleFolderByID(w http.ResponseWriter, r *http.Request) {
	id, ok := pathIdentifier(r.URL.Path, "/folders/")
	if !ok || !validIdentifier(id) {
		writeError(w, http.StatusBadRequest, "invalid_id", "a valid folder id is required")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.mu.RLock()
		folder, exists := s.collaboration.Folders[id]
		s.mu.RUnlock()
		if !exists {
			writeError(w, http.StatusNotFound, "not_found", "folder does not exist")
			return
		}
		if err := writeJSON(w, folder); err != nil {
			log.Printf("[folders] response failed: %v", err)
		}

	case http.MethodPut:
		var input folderInput
		if err := s.decodeJSON(w, r, &input); err != nil {
			handleJSONDecodeError(w, err)
			return
		}
		if !input.hasMutableField() {
			writeError(w, http.StatusBadRequest, "invalid_folder", "at least one mutable folder field is required")
			return
		}
		s.mu.Lock()
		next := cloneState(s.collaboration)
		folder, exists := next.Folders[id]
		if !exists {
			s.mu.Unlock()
			writeError(w, http.StatusNotFound, "not_found", "folder does not exist")
			return
		}
		if input.BranchID != nil {
			if !validIdentifier(*input.BranchID) {
				s.mu.Unlock()
				writeError(w, http.StatusBadRequest, "invalid_folder", "branch_id is invalid")
				return
			}
			folder.BranchID = *input.BranchID
		}
		if input.Name != nil {
			if !validName(*input.Name) {
				s.mu.Unlock()
				writeError(w, http.StatusBadRequest, "invalid_folder", "name is invalid")
				return
			}
			folder.Name = *input.Name
		}
		if input.ParentID != nil {
			parent, _, err := parseParentID(input.ParentID)
			if err != nil {
				s.mu.Unlock()
				writeError(w, http.StatusBadRequest, "invalid_folder", err.Error())
				return
			}
			folder.ParentID = parent
		}
		if input.MemberIDs != nil {
			if hasDuplicates(*input.MemberIDs) {
				s.mu.Unlock()
				writeError(w, http.StatusBadRequest, "invalid_folder", "member IDs must be unique")
				return
			}
			folder.MemberIDs = cloneStrings(*input.MemberIDs)
			if err := validateMemberIDs(folder.MemberIDs, next.Members); err != nil {
				s.mu.Unlock()
				writeError(w, http.StatusBadRequest, "invalid_folder", err.Error())
				return
			}
		}
		folder.ID = id
		if err := validateFolder(next, folder); err != nil {
			s.mu.Unlock()
			writeError(w, http.StatusBadRequest, "invalid_folder", err.Error())
			return
		}
		next.Folders[id] = folder
		if err := s.commitCollaborationLocked(next); err != nil {
			s.mu.Unlock()
			writeError(w, http.StatusInternalServerError, "storage_failure", "unable to save folder")
			return
		}
		s.mu.Unlock()
		if err := writeJSON(w, folder); err != nil {
			log.Printf("[folders] response failed: %v", err)
		}

	case http.MethodDelete:
		s.deleteFolder(w, id)

	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
	}
}

func validateFolder(state CollaborationState, folder Folder) error {
	if !validIdentifier(folder.ID) || !validIdentifier(folder.BranchID) || !validName(folder.Name) {
		return errors.New("folder id, branch_id, and name are required")
	}
	if _, exists := state.Branches[folder.BranchID]; !exists {
		return fmt.Errorf("branch %q does not exist", folder.BranchID)
	}
	if folder.ParentID == "" {
		return nil
	}
	if folder.ParentID == folder.ID {
		return errors.New("folder cannot parent itself")
	}
	seen := map[string]bool{}
	current := folder.ParentID
	for current != "" {
		if current == folder.ID || seen[current] {
			return errors.New("folder hierarchy cannot contain cycles")
		}
		seen[current] = true
		ancestor, exists := state.Folders[current]
		if !exists {
			return fmt.Errorf("parent folder %q does not exist", current)
		}
		if ancestor.BranchID != folder.BranchID {
			return errors.New("parent folder must belong to the same branch")
		}
		current = ancestor.ParentID
	}
	return nil
}

func hasFolderCycle(folders map[string]Folder, id string) bool {
	seen := map[string]bool{}
	current := id
	for current != "" {
		if seen[current] {
			return true
		}
		seen[current] = true
		folder, ok := folders[current]
		if !ok {
			return false
		}
		current = folder.ParentID
	}
	return false
}

func (s *Server) deleteFolder(w http.ResponseWriter, id string) {
	s.mu.Lock()
	next := cloneState(s.collaboration)
	if _, exists := next.Folders[id]; !exists {
		s.mu.Unlock()
		writeError(w, http.StatusNotFound, "not_found", "folder does not exist")
		return
	}
	deleted := map[string]bool{id: true}
	changed := true
	for changed {
		changed = false
		for folderID, folder := range next.Folders {
			if deleted[folder.ParentID] && !deleted[folderID] {
				deleted[folderID] = true
				changed = true
			}
		}
	}
	for folderID := range deleted {
		delete(next.Folders, folderID)
	}
	removeInvitationsForScope(&next, "", deleted)
	if err := s.commitCollaborationLocked(next); err != nil {
		s.mu.Unlock()
		writeError(w, http.StatusInternalServerError, "storage_failure", "unable to save folder removal")
		return
	}
	s.mu.Unlock()
	log.Printf("[folders] deleted id=%s count=%d", id, len(deleted))
	w.WriteHeader(http.StatusNoContent)
}

func removeInvitationsForScope(state *CollaborationState, branchID string, folderIDs map[string]bool) {
	for code, invite := range state.Invitations {
		remove := false
		if branchID != "" {
			for _, id := range invite.BranchIDs {
				if id == branchID {
					remove = true
				}
			}
		}
		if !remove {
			for _, id := range invite.FolderIDs {
				if folderIDs[id] {
					remove = true
				}
			}
		}
		if remove {
			delete(state.Invitations, code)
		}
	}
}

// ─────────────────────────────────────────────
// Filesystem and validation helpers
// ─────────────────────────────────────────────

func atomicCopy(path string, source io.Reader, maxBytes int64, mode os.FileMode) (uploadResult, error) {
	dir := filepath.Dir(path)
	temporary, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return uploadResult{}, err
	}
	temporaryName := temporary.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(temporaryName)
		}
	}()

	hasher := sha256.New()
	limited := io.LimitReader(source, maxBytes+1)
	n, err := io.Copy(io.MultiWriter(temporary, hasher), limited)
	if err != nil {
		_ = temporary.Close()
		return uploadResult{}, err
	}
	if n > maxBytes {
		_ = temporary.Close()
		return uploadResult{}, errTooLarge
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return uploadResult{}, err
	}
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return uploadResult{}, err
	}
	if err := temporary.Close(); err != nil {
		return uploadResult{}, err
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return uploadResult{}, err
	}
	committed = true
	return uploadResult{ID: filepath.Base(path), Size: n, SHA256: hex.EncodeToString(hasher.Sum(nil))}, nil
}

func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(temporaryName)
		}
	}()
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return err
	}
	committed = true
	return nil
}

func pathIdentifier(path, prefix string) (string, bool) {
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	id := strings.TrimPrefix(path, prefix)
	if id == "" || strings.Contains(id, "/") {
		return "", false
	}
	return id, true
}

func validPlatform(platform string) bool {
	switch platform {
	case "ios", "android", "web":
		return true
	default:
		return false
	}
}

func validIdentifier(value string) bool {
	if len(value) == 0 || len(value) > 128 || value == "." || value == ".." {
		return false
	}
	for index, char := range []byte(value) {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || (index > 0 && (char == '-' || char == '_' || char == '.')) {
			continue
		}
		return false
	}
	return true
}

func validName(value string) bool {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || len(trimmed) > 512 || !utf8.ValidString(trimmed) {
		return false
	}
	for _, char := range trimmed {
		if char < 0x20 || char == 0x7f {
			return false
		}
	}
	return true
}

func validateMemberIDs(ids []string, members map[string]Member) error {
	seen := map[string]bool{}
	for _, id := range ids {
		if !validIdentifier(id) {
			return fmt.Errorf("invalid member id %q", id)
		}
		if seen[id] {
			return fmt.Errorf("duplicate member id %q", id)
		}
		if _, exists := members[id]; !exists {
			return fmt.Errorf("member %q does not exist", id)
		}
		seen[id] = true
	}
	return nil
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func uniqueStrings(values []string) []string {
	result := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func cloneStrings(values []string) []string {
	cloned := make([]string, len(values))
	copy(cloned, values)
	return cloned
}

func hasDuplicates(values []string) bool {
	return len(uniqueStrings(values)) != len(values)
}

func appendIfMissing(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func removeString(values []string, target string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != target {
			result = append(result, value)
		}
	}
	return result
}

func countOwners(members map[string]Member) int {
	count := 0
	for _, member := range members {
		if member.Role == RoleOwner {
			count++
		}
	}
	return count
}

func sha256hex(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}
