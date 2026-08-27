package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testEnv points the server at a temp data dir with a known token and
// returns a fresh mux. The global state is scoped per test to keep
// tests independent.
func testEnv(t *testing.T) (*http.ServeMux, string) {
	t.Helper()
	dir := t.TempDir()
	dataDir = dir

	// Isolate global state between tests.
	mu.Lock()
	collaboration = CollaborationState{
		Members:     map[string]Member{},
		Invitations: map[string]Invitation{},
		Branches:    map[string]Branch{},
		Folders:     map[string]Folder{},
	}
	mu.Unlock()

	// main() creates these; tests call newMux() directly, so mirror it.
	os.MkdirAll(filepath.Join(dataDir, "sync"), 0755)
	os.MkdirAll(filepath.Join(dataDir, "attachments"), 0755)
	os.MkdirAll(filepath.Join(dataDir, "backup"), 0755)

	apiKeys = map[string]string{"test": sha256hex([]byte("secret-token"))}

	return newMux(), "secret-token"
}

// do performs an authenticated request and returns the response.
func do(t *testing.T, mux *http.ServeMux, method, path, token string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %s: %v (body %q)", rec.Result().Request.URL.Path, err, rec.Body.String())
	}
}

func TestStatusIdentityAndCapabilities(t *testing.T) {
	mux, token := testEnv(t)
	rec := do(t, mux, http.MethodGet, "/status", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var s StatusResponse
	decodeBody(t, rec, &s)
	if s.Service != serviceName {
		t.Errorf("service = %q, want %q", s.Service, serviceName)
	}
	if s.APIVersion != apiVersion {
		t.Errorf("api_version = %d, want %d", s.APIVersion, apiVersion)
	}
	if s.ServerVersion != serverVersion {
		t.Errorf("server_version = %q, want %q", s.ServerVersion, serverVersion)
	}
	found := map[string]bool{}
	for _, c := range s.Capabilities {
		found[c] = true
	}
	for _, want := range []string{"collaboration", "backups", "attachments"} {
		if !found[want] {
			t.Errorf("capability %q missing from %v", want, s.Capabilities)
		}
	}
	// Collections must be arrays, never null.
	if s.Syncs == nil || s.Attachments == nil || s.Recoveries == nil {
		t.Errorf("status collections must be [] not null: %+v", s)
	}
}

func TestAuth(t *testing.T) {
	mux, token := testEnv(t)

	rec := do(t, mux, http.MethodGet, "/status", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token = %d, want 401", rec.Code)
	}
	assertErrorCode(t, rec, "unauthorized")

	rec = do(t, mux, http.MethodGet, "/status", "wrong-token", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token = %d, want 401", rec.Code)
	}

	rec = do(t, mux, http.MethodGet, "/status", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("correct token = %d, want 200", rec.Code)
	}
}

func assertErrorCode(t *testing.T, rec *httptest.ResponseRecorder, wantCode string) {
	t.Helper()
	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body is not JSON: %v (body %q)", err, rec.Body.String())
	}
	if body.Error.Code != wantCode {
		t.Errorf("error.code = %q, want %q (body %q)", body.Error.Code, wantCode, rec.Body.String())
	}
	if body.Error.Message == "" {
		t.Errorf("error.message is empty")
	}
}

func TestInvitationLifecycle(t *testing.T) {
	mux, token := testEnv(t)

	// Create invitation
	create := `{"created_by":"owner-1","role":"organizer","branch_ids":["b1"],"folder_ids":[]}`
	rec := do(t, mux, http.MethodPost, "/invite", token, strings.NewReader(create))
	if rec.Code != http.StatusOK {
		t.Fatalf("create invite = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var inv Invitation
	decodeBody(t, rec, &inv)
	if inv.Code == "" {
		t.Fatal("invite code is empty")
	}
	if inv.Role != RoleOrganizer {
		t.Errorf("role = %q, want organizer", inv.Role)
	}
	if inv.CreatedBy != "owner-1" {
		t.Errorf("created_by = %q, want owner-1", inv.CreatedBy)
	}
	if inv.BranchIDs == nil || inv.FolderIDs == nil {
		t.Errorf("branch_ids/folder_ids must be [] not null: %+v", inv)
	}

	// Lookup
	rec = do(t, mux, http.MethodGet, "/invite/"+inv.Code, token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("lookup invite = %d, want 200", rec.Code)
	}
	var got Invitation
	decodeBody(t, rec, &got)
	if got.Code != inv.Code || got.Role != inv.Role {
		t.Errorf("lookup mismatch: %+v vs %+v", got, inv)
	}

	// Accept as a new member
	accept := `{"member_id":"acceptor-1","member_name":"Aunt June"}`
	rec = do(t, mux, http.MethodPost, "/invite/"+inv.Code, token, strings.NewReader(accept))
	if rec.Code != http.StatusOK {
		t.Fatalf("accept invite = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var acceptResp map[string]any
	decodeBody(t, rec, &acceptResp)
	if acceptResp["status"] != "accepted" {
		t.Errorf("accept status = %v, want accepted", acceptResp["status"])
	}
	if acceptResp["role"] != "organizer" {
		t.Errorf("accept role = %v, want organizer", acceptResp["role"])
	}

	// Invitation must no longer be pending.
	rec = do(t, mux, http.MethodGet, "/invite/"+inv.Code, token, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("accepted invite lookup = %d, want 404", rec.Code)
	}

	// Member created with the invited role.
	rec = do(t, mux, http.MethodGet, "/members", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list members = %d", rec.Code)
	}
	var members []Member
	decodeBody(t, rec, &members)
	found := false
	for _, m := range members {
		if m.ID == "acceptor-1" {
			found = true
			if m.Role != RoleOrganizer {
				t.Errorf("member role = %q, want organizer", m.Role)
			}
			if m.Since == 0 {
				t.Errorf("member since unset")
			}
		}
	}
	if !found {
		t.Errorf("accepted member not in member list: %+v", members)
	}
}

func TestInviteRejectsDuplicateMember(t *testing.T) {
	mux, token := testEnv(t)
	do(t, mux, http.MethodPost, "/invite", token, strings.NewReader(`{"created_by":"o","role":"viewer"}`))
	// Create a member first.
	do(t, mux, http.MethodPut, "/members", token, strings.NewReader(`{"id":"m1","name":"M","role":"viewer"}`))
	// Create a second invite and try to accept with the same member id.
	rec := do(t, mux, http.MethodPost, "/invite", token, strings.NewReader(`{"created_by":"o","role":"viewer"}`))
	var inv Invitation
	decodeBody(t, rec, &inv)
	rec = do(t, mux, http.MethodPost, "/invite/"+inv.Code, token, strings.NewReader(`{"member_id":"m1","member_name":"M"}`))
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate accept = %d, want 409", rec.Code)
	}
	assertErrorCode(t, rec, "conflict")
}

func TestInviteExpiration(t *testing.T) {
	mux, token := testEnv(t)
	// Insert an already-expired invitation directly.
	mu.Lock()
	collaboration.Invitations["EXPIRED1"] = Invitation{
		Code: "EXPIRED1", CreatedBy: "o", Role: RoleViewer,
		Expires: time.Now().Add(-time.Hour).UnixMilli(),
	}
	mu.Unlock()

	rec := do(t, mux, http.MethodGet, "/invite/EXPIRED1", token, nil)
	if rec.Code != http.StatusGone {
		t.Fatalf("expired lookup = %d, want 410", rec.Code)
	}
	assertErrorCode(t, rec, "expired")

	rec = do(t, mux, http.MethodPost, "/invite/EXPIRED1", token, strings.NewReader(`{"member_id":"m","member_name":"M"}`))
	if rec.Code != http.StatusGone {
		t.Fatalf("expired accept = %d, want 410", rec.Code)
	}
}

func TestInviteRevoke(t *testing.T) {
	mux, token := testEnv(t)
	rec := do(t, mux, http.MethodPost, "/invite", token, strings.NewReader(`{"created_by":"o","role":"viewer"}`))
	var inv Invitation
	decodeBody(t, rec, &inv)

	rec = do(t, mux, http.MethodDelete, "/invite/"+inv.Code, token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d, want 200", rec.Code)
	}
	rec = do(t, mux, http.MethodGet, "/invite/"+inv.Code, token, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("revoked lookup = %d, want 404", rec.Code)
	}
}

func TestInviteRejectsUnknownRole(t *testing.T) {
	mux, token := testEnv(t)
	rec := do(t, mux, http.MethodPost, "/invite", token, strings.NewReader(`{"created_by":"o","role":"superuser","branch_ids":[]}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown role = %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, "invalid_request")
}

func TestInviteRoleDefaultsToViewer(t *testing.T) {
	mux, token := testEnv(t)
	rec := do(t, mux, http.MethodPost, "/invite", token, strings.NewReader(`{"created_by":"o"}`))
	var inv Invitation
	decodeBody(t, rec, &inv)
	if inv.Role != RoleViewer {
		t.Errorf("default role = %q, want viewer (least privilege)", inv.Role)
	}
}

func TestMemberRoleValidation(t *testing.T) {
	mux, token := testEnv(t)
	rec := do(t, mux, http.MethodPut, "/members", token, strings.NewReader(`{"id":"m1","name":"M","role":"admin"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown member role = %d, want 400", rec.Code)
	}
	assertErrorCode(t, rec, "invalid_request")
}

func TestMemberRemoveCleansScope(t *testing.T) {
	mux, token := testEnv(t)
	// Member
	do(t, mux, http.MethodPut, "/members", token, strings.NewReader(`{"id":"m1","name":"M","role":"viewer"}`))
	// Branch shared with m1
	do(t, mux, http.MethodPost, "/branches", token, strings.NewReader(`{"id":"b1","name":"Side","kind":"paternal","member_ids":["m1"]}`))
	// Folder shared with m1
	do(t, mux, http.MethodPost, "/folders", token, strings.NewReader(`{"id":"f1","branch_id":"b1","name":"Box","member_ids":["m1"]}`))

	rec := do(t, mux, http.MethodDelete, "/members?id=m1", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("remove member = %d, want 200", rec.Code)
	}

	rec = do(t, mux, http.MethodGet, "/branches/b1", token, nil)
	var b Branch
	decodeBody(t, rec, &b)
	if len(b.MemberIDs) != 0 {
		t.Errorf("branch scope not cleaned: %v", b.MemberIDs)
	}
	rec = do(t, mux, http.MethodGet, "/folders/f1", token, nil)
	var f Folder
	decodeBody(t, rec, &f)
	if len(f.MemberIDs) != 0 {
		t.Errorf("folder scope not cleaned: %v", f.MemberIDs)
	}

	// Deleting a missing member is 404, not a silent success.
	rec = do(t, mux, http.MethodDelete, "/members?id=nope", token, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("delete missing member = %d, want 404", rec.Code)
	}
}

func TestBranchLifecycle(t *testing.T) {
	mux, token := testEnv(t)

	// Create
	rec := do(t, mux, http.MethodPost, "/branches", token, strings.NewReader(`{"id":"b1","name":"Maternal","kind":"maternal"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("create branch = %d (body %q)", rec.Code, rec.Body.String())
	}
	var b Branch
	decodeBody(t, rec, &b)
	if b.IsSeeded || b.CreatedAt == 0 || b.MemberIDs == nil || len(b.MemberIDs) != 0 {
		t.Errorf("branch defaults wrong: %+v", b)
	}

	// Duplicate create → 409
	rec = do(t, mux, http.MethodPost, "/branches", token, strings.NewReader(`{"id":"b1","name":"Maternal"}`))
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate branch = %d, want 409", rec.Code)
	}

	// List
	rec = do(t, mux, http.MethodGet, "/branches", token, nil)
	var branches []Branch
	decodeBody(t, rec, &branches)
	if len(branches) != 1 {
		t.Fatalf("branch list len = %d, want 1", len(branches))
	}

	// Get
	rec = do(t, mux, http.MethodGet, "/branches/b1", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get branch = %d", rec.Code)
	}

	// Update
	rec = do(t, mux, http.MethodPut, "/branches/b1", token, strings.NewReader(`{"name":"Maternal side","kind":"maternal","member_ids":["m1"]}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("update branch = %d (body %q)", rec.Code, rec.Body.String())
	}
	decodeBody(t, rec, &b)
	if b.Name != "Maternal side" || len(b.MemberIDs) != 1 || b.ID != "b1" {
		t.Errorf("update result wrong: %+v", b)
	}

	// Update missing branch → 404 (no false success)
	rec = do(t, mux, http.MethodPut, "/branches/missing", token, strings.NewReader(`{"name":"x"}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("update missing branch = %d, want 404", rec.Code)
	}
	assertErrorCode(t, rec, "not_found")

	// Delete cascades folders
	do(t, mux, http.MethodPost, "/folders", token, strings.NewReader(`{"id":"f1","branch_id":"b1","name":"Box"}`))
	rec = do(t, mux, http.MethodDelete, "/branches/b1", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete branch = %d", rec.Code)
	}
	rec = do(t, mux, http.MethodGet, "/folders/f1", token, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("folder after branch delete = %d, want 404 (cascade)", rec.Code)
	}
}

func TestFolderLifecycle(t *testing.T) {
	mux, token := testEnv(t)
	do(t, mux, http.MethodPost, "/branches", token, strings.NewReader(`{"id":"b1","name":"Paternal"}`))

	// Create
	rec := do(t, mux, http.MethodPost, "/folders", token, strings.NewReader(`{"id":"f1","branch_id":"b1","name":"Grandpa letters"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("create folder = %d (body %q)", rec.Code, rec.Body.String())
	}
	var f Folder
	decodeBody(t, rec, &f)
	if f.MemberIDs == nil {
		t.Errorf("member_ids must be [] not null: %+v", f)
	}

	// Folder in missing branch → 422
	rec = do(t, mux, http.MethodPost, "/folders", token, strings.NewReader(`{"id":"f2","branch_id":"nope","name":"X"}`))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("folder in missing branch = %d, want 422", rec.Code)
	}

	// Filter by branch
	do(t, mux, http.MethodPost, "/folders", token, strings.NewReader(`{"id":"f3","branch_id":"b1","name":"Another","parent_id":"f1"}`))
	rec = do(t, mux, http.MethodGet, "/folders?branch_id=b1", token, nil)
	var folders []Folder
	decodeBody(t, rec, &folders)
	if len(folders) != 2 {
		t.Fatalf("filtered folder len = %d, want 2", len(folders))
	}

	// Update
	rec = do(t, mux, http.MethodPut, "/folders/f1", token, strings.NewReader(`{"branch_id":"b1","name":"Renamed"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("update folder = %d", rec.Code)
	}
	decodeBody(t, rec, &f)
	if f.Name != "Renamed" || f.ID != "f1" {
		t.Errorf("update result wrong: %+v", f)
	}

	// Update missing → 404
	rec = do(t, mux, http.MethodPut, "/folders/missing", token, strings.NewReader(`{"branch_id":"b1","name":"x"}`))
	if rec.Code != http.StatusNotFound {
		t.Errorf("update missing folder = %d, want 404", rec.Code)
	}

	// Delete
	rec = do(t, mux, http.MethodDelete, "/folders/f1", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete folder = %d", rec.Code)
	}
	rec = do(t, mux, http.MethodGet, "/folders/f1", token, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("deleted folder = %d, want 404", rec.Code)
	}
}

func TestBackupRoundTrip(t *testing.T) {
	mux, token := testEnv(t)
	payload := bytes.Repeat([]byte("encrypted-blob-"), 100)

	// Push
	rec := do(t, mux, http.MethodPut, "/backup/push?id=backup-1", token, bytes.NewReader(payload))
	if rec.Code != http.StatusOK {
		t.Fatalf("backup push = %d (body %q)", rec.Code, rec.Body.String())
	}
	var meta RecoveryMeta
	decodeBody(t, rec, &meta)
	if meta.ID != "backup-1" || meta.Size != int64(len(payload)) || meta.Timestamp == 0 {
		t.Errorf("push meta wrong: %+v", meta)
	}

	// List
	rec = do(t, mux, http.MethodGet, "/backup/list", token, nil)
	var backups []RecoveryMeta
	decodeBody(t, rec, &backups)
	if len(backups) != 1 || backups[0].ID != "backup-1" {
		t.Fatalf("backup list wrong: %+v", backups)
	}

	// Pull — byte-identical
	rec = do(t, mux, http.MethodGet, "/backup/pull/backup-1", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("backup pull = %d", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatal("backup bytes do not match after round trip")
	}

	// Missing pull → 404
	rec = do(t, mux, http.MethodGet, "/backup/pull/missing", token, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing backup = %d, want 404", rec.Code)
	}

	// Delete
	rec = do(t, mux, http.MethodDelete, "/backup/backup-1", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("backup delete = %d", rec.Code)
	}
	rec = do(t, mux, http.MethodGet, "/backup/list", token, nil)
	decodeBody(t, rec, &backups)
	if len(backups) != 0 {
		t.Errorf("backup list after delete = %d, want 0", len(backups))
	}
}

func TestAttachmentRoundTrip(t *testing.T) {
	mux, token := testEnv(t)
	payload := bytes.Repeat([]byte{0xAB, 0xCD}, 500)

	rec := do(t, mux, http.MethodPut, "/attachment/upload?id=att-1", token, bytes.NewReader(payload))
	if rec.Code != http.StatusOK {
		t.Fatalf("attachment upload = %d (body %q)", rec.Code, rec.Body.String())
	}

	rec = do(t, mux, http.MethodGet, "/attachment/list", token, nil)
	var atts []AttachmentMeta
	decodeBody(t, rec, &atts)
	if len(atts) != 1 || atts[0].ID != "att-1" {
		t.Fatalf("attachment list wrong: %+v", atts)
	}

	rec = do(t, mux, http.MethodGet, "/attachment/download/att-1", token, nil)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatalf("attachment download mismatch (code %d)", rec.Code)
	}

	rec = do(t, mux, http.MethodDelete, "/attachment/att-1", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("attachment delete = %d", rec.Code)
	}
	rec = do(t, mux, http.MethodGet, "/attachment/download/att-1", token, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("deleted attachment = %d, want 404", rec.Code)
	}
}

func TestIDValidationBlocksTraversal(t *testing.T) {
	mux, token := testEnv(t)
	for _, bad := range []string{
		"../../etc/passwd",
		"..%2F..%2Fetc",
		"a/b",
		"",
		"a b",
		"backup;rm",
	} {
		target := "/backup/push?id=" + url.QueryEscape(bad)
		rec := do(t, mux, http.MethodPut, target, token, strings.NewReader("x"))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("backup id %q = %d, want 400", bad, rec.Code)
		}
	}
	// Same for attachment upload ids.
	rec := do(t, mux, http.MethodPut, "/attachment/upload?id="+url.QueryEscape("../x"), token, strings.NewReader("x"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("attachment traversal id = %d, want 400", rec.Code)
	}
	// Valid UUIDs and invite codes pass validation (404 because missing).
	rec = do(t, mux, http.MethodGet, "/branches/7B8E0A2C-3D4F-5A6B-7C8D-9E0F1A2B3C4D", token, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("uuid branch = %d, want 404 (validated, then missing)", rec.Code)
	}
}

func TestSyncSnapshotStorage(t *testing.T) {
	mux, token := testEnv(t)
	blob := []byte("not-a-real-database")

	rec := do(t, mux, http.MethodPut, "/sync/push/ios", token, bytes.NewReader(blob))
	if rec.Code != http.StatusOK {
		t.Fatalf("sync push = %d", rec.Code)
	}

	rec = do(t, mux, http.MethodGet, "/sync/list", token, nil)
	var syncs []SyncPayload
	decodeBody(t, rec, &syncs)
	if len(syncs) != 1 || syncs[0].Platform != "ios" || syncs[0].Kind != "device-snapshot" {
		t.Fatalf("sync list wrong: %+v", syncs)
	}

	rec = do(t, mux, http.MethodGet, "/sync/pull/ios", token, nil)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), blob) {
		t.Fatalf("sync pull mismatch (code %d)", rec.Code)
	}

	rec = do(t, mux, http.MethodGet, "/sync/pull/android", token, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing platform = %d, want 404", rec.Code)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	mux, token := testEnv(t)
	rec := do(t, mux, http.MethodDelete, "/status", token, nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE /status = %d, want 405", rec.Code)
	}
	assertErrorCode(t, rec, "method_not_allowed")
}

func TestPersistenceAcrossRestart(t *testing.T) {
	// Simulates container restart: save collaboration state to disk,
	// then reload it into a fresh CollaborationState.
	dir := t.TempDir()
	dataDir = dir
	defer func() { dataDir = "" }()

	mu.Lock()
	collaboration = CollaborationState{
		Members:     map[string]Member{},
		Invitations: map[string]Invitation{},
		Branches:    map[string]Branch{},
		Folders:     map[string]Folder{},
	}
	collaboration.Branches["b1"] = Branch{ID: "b1", Name: "Maternal", Kind: "maternal", MemberIDs: []string{}, CreatedAt: time.Now().UnixMilli()}
	collaboration.Members["m1"] = Member{ID: "m1", Name: "M", Role: RoleViewer, Since: time.Now().UnixMilli()}
	saveCollaboration()
	mu.Unlock()

	// Reload into a clean state.
	mu.Lock()
	collaboration = CollaborationState{
		Members:     map[string]Member{},
		Invitations: map[string]Invitation{},
		Branches:    map[string]Branch{},
		Folders:     map[string]Folder{},
	}
	mu.Unlock()
	loadCollaboration()

	mu.RLock()
	defer mu.RUnlock()
	if len(collaboration.Branches) != 1 || collaboration.Branches["b1"].Name != "Maternal" {
		t.Errorf("branch not restored after restart: %+v", collaboration.Branches)
	}
	if len(collaboration.Members) != 1 || collaboration.Members["m1"].Role != RoleViewer {
		t.Errorf("member not restored after restart: %+v", collaboration.Members)
	}
	// The data file on disk is a valid JSON document.
	data, err := os.ReadFile(filepath.Join(dir, "collaboration.json"))
	if err != nil {
		t.Fatalf("collaboration.json missing: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("collaboration.json invalid JSON: %v", err)
	}
}

func TestMain(m *testing.M) {
	code := m.Run()
	os.Exit(code)
}

var _ = fmt.Sprintf // keep fmt import if unused in some build modes