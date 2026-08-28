package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "test-secret-token"

func testServer(t *testing.T) *Server {
	t.Helper()
	server, err := newServer(
		t.TempDir(),
		map[string]string{"test": sha256hex([]byte(testToken))},
		Limits{JSONBody: 256, Sync: 32, Attachment: 32, Backup: 32},
		"test",
	)
	if err != nil {
		t.Fatal(err)
	}
	fixedNow := time.UnixMilli(1_700_000_000_000)
	server.now = func() time.Time { return fixedNow }
	return server
}

func request(server *Server, method, path string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Authorization", "Bearer "+testToken)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, req)
	return recorder
}

func requestWithBody(server *Server, method, path string, body io.ReadCloser) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Authorization", "Bearer "+testToken)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, req)
	return recorder
}

func jsonRequest(t *testing.T, server *Server, method, path string, value any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, req)
	return recorder
}

func decodeResponse[T any](t *testing.T, recorder *httptest.ResponseRecorder) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(recorder.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode response %s: %v; body=%q", recorder.Result().Status, err, recorder.Body.String())
	}
	return value
}

func requireStatus(t *testing.T, recorder *httptest.ResponseRecorder, want int) {
	t.Helper()
	if recorder.Code != want {
		t.Fatalf("status=%d want=%d body=%s", recorder.Code, want, recorder.Body.String())
	}
}

func putMember(t *testing.T, server *Server, id string, role Role) {
	t.Helper()
	recorder := jsonRequest(t, server, http.MethodPut, "/members", Member{ID: id, Name: id + " name", Role: role})
	requireStatus(t, recorder, http.StatusOK)
}

func putBranch(t *testing.T, server *Server, branch Branch) *httptest.ResponseRecorder {
	t.Helper()
	return jsonRequest(t, server, http.MethodPost, "/branches", branch)
}

func putFolder(t *testing.T, server *Server, folder Folder) *httptest.ResponseRecorder {
	t.Helper()
	return jsonRequest(t, server, http.MethodPost, "/folders", folder)
}

func createInvite(t *testing.T, server *Server, createdBy string, role Role, branches, folders []string) Invitation {
	t.Helper()
	recorder := jsonRequest(t, server, http.MethodPost, "/invite", map[string]any{
		"created_by": createdBy,
		"role":       role,
		"branch_ids": branches,
		"folder_ids": folders,
	})
	requireStatus(t, recorder, http.StatusCreated)
	return decodeResponse[Invitation](t, recorder)
}

func TestAuthenticationAndMethods(t *testing.T) {
	server := testServer(t)
	for name, authorization := range map[string]string{
		"missing":   "",
		"wrong":     "Bearer wrong",
		"malformed": "Token " + testToken,
		"extra":     "Bearer " + testToken + " extra",
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/status", nil)
			if authorization != "" {
				req.Header.Set("Authorization", authorization)
			}
			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, req)
			requireStatus(t, recorder, http.StatusUnauthorized)
			if recorder.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatalf("missing bearer challenge")
			}
		})
	}

	status := request(server, http.MethodGet, "/status", nil)
	requireStatus(t, status, http.StatusOK)
	decoded := decodeResponse[StatusResponse](t, status)
	if decoded.Syncs == nil || decoded.Attachments == nil || decoded.Recoveries == nil {
		t.Fatalf("fresh status must use empty arrays, got %+v", decoded)
	}

	wrongMethod := request(server, http.MethodPost, "/sync/list", nil)
	requireStatus(t, wrongMethod, http.StatusMethodNotAllowed)
	if wrongMethod.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("Allow=%q", wrongMethod.Header().Get("Allow"))
	}
	wrongMethod = request(server, http.MethodPost, "/status", nil)
	requireStatus(t, wrongMethod, http.StatusMethodNotAllowed)
}

func TestSyncAttachmentAndBackupRoundTrips(t *testing.T) {
	server := testServer(t)
	for _, test := range []struct {
		name     string
		pushPath string
		pullPath string
		listPath string
		first    string
		second   string
		missing  string
		bad      string
		tooLarge string
	}{
		{name: "sync", pushPath: "/sync/push/ios", pullPath: "/sync/pull/ios", listPath: "/sync/list", first: "sync-one", second: "sync-two", missing: "/sync/pull/android", bad: "/sync/push/windows", tooLarge: "1234567890123456789012345678901234567890"},
		{name: "attachment", pushPath: "/attachment/upload?id=photo-1", pullPath: "/attachment/download/photo-1", listPath: "/status", first: "photo-one", second: "photo-two", missing: "/attachment/download/missing", bad: "/attachment/upload?id=../escape", tooLarge: "1234567890123456789012345678901234567890"},
		{name: "backup", pushPath: "/backup/push?id=archive-1", pullPath: "/backup/pull/archive-1", listPath: "/backup/list", first: "backup-one", second: "backup-two", missing: "/backup/pull/missing", bad: "/backup/push?id=../escape", tooLarge: "1234567890123456789012345678901234567890"},
	} {
		t.Run(test.name, func(t *testing.T) {
			first := request(server, http.MethodPut, test.pushPath, strings.NewReader(test.first))
			requireStatus(t, first, http.StatusOK)
			second := request(server, http.MethodPut, test.pushPath, strings.NewReader(test.second))
			requireStatus(t, second, http.StatusOK)

			pulled := request(server, http.MethodGet, test.pullPath, nil)
			requireStatus(t, pulled, http.StatusOK)
			if pulled.Body.String() != test.second {
				t.Fatalf("round trip=%q want=%q", pulled.Body.String(), test.second)
			}
			missing := request(server, http.MethodGet, test.missing, nil)
			requireStatus(t, missing, http.StatusNotFound)
			bad := request(server, http.MethodPut, test.bad, strings.NewReader("bad"))
			requireStatus(t, bad, http.StatusBadRequest)
			tooLarge := request(server, http.MethodPut, test.pushPath, strings.NewReader(test.tooLarge))
			requireStatus(t, tooLarge, http.StatusRequestEntityTooLarge)
			stillGood := request(server, http.MethodGet, test.pullPath, nil)
			requireStatus(t, stillGood, http.StatusOK)
			if stillGood.Body.String() != test.second {
				t.Fatalf("oversized upload destroyed good data: %q", stillGood.Body.String())
			}

			if test.name == "backup" {
				generatedOne := request(server, http.MethodPut, "/backup/push", strings.NewReader("a"))
				generatedTwo := request(server, http.MethodPut, "/backup/push", strings.NewReader("b"))
				requireStatus(t, generatedOne, http.StatusOK)
				requireStatus(t, generatedTwo, http.StatusOK)
				one := decodeResponse[uploadResult](t, generatedOne)
				two := decodeResponse[uploadResult](t, generatedTwo)
				if one.ID == two.ID || !validIdentifier(one.ID) || !validIdentifier(two.ID) {
					t.Fatalf("generated IDs collided or were invalid: %q %q", one.ID, two.ID)
				}
				list := request(server, http.MethodGet, test.listPath, nil)
				requireStatus(t, list, http.StatusOK)
				backups := decodeResponse[[]RecoveryMeta](t, list)
				if len(backups) != 3 {
					t.Fatalf("backups=%+v", backups)
				}
			}
		})
	}
}

type failingBody struct {
	data []byte
	done bool
}

func (body *failingBody) Read(buffer []byte) (int, error) {
	if body.done {
		return 0, errors.New("simulated upload interruption")
	}
	body.done = true
	n := copy(buffer, body.data)
	return n, errors.New("simulated upload interruption")
}

func (body *failingBody) Close() error { return nil }

func TestInterruptedUploadPreservesPreviousSnapshot(t *testing.T) {
	server := testServer(t)
	requireStatus(t, request(server, http.MethodPut, "/sync/push/ios", strings.NewReader("good")), http.StatusOK)
	interrupted := requestWithBody(server, http.MethodPut, "/sync/push/ios", &failingBody{data: []byte("partial")})
	requireStatus(t, interrupted, http.StatusInternalServerError)
	pulled := request(server, http.MethodGet, "/sync/pull/ios", nil)
	requireStatus(t, pulled, http.StatusOK)
	if pulled.Body.String() != "good" {
		t.Fatalf("interrupted upload changed snapshot to %q", pulled.Body.String())
	}
}

func TestConcurrentSnapshotUploadsDoNotInterleave(t *testing.T) {
	server := testServer(t)
	server.limits.Sync = 1024
	payloads := make([]string, 20)
	for index := range payloads {
		payloads[index] = strings.Repeat(string(rune('a'+index)), 100)
	}
	var group sync.WaitGroup
	statuses := make(chan int, len(payloads))
	for _, payload := range payloads {
		payload := payload
		group.Add(1)
		go func() {
			defer group.Done()
			statuses <- request(server, http.MethodPut, "/sync/push/ios", strings.NewReader(payload)).Code
		}()
	}
	group.Wait()
	close(statuses)
	for status := range statuses {
		if status != http.StatusOK {
			t.Fatalf("concurrent upload status=%d", status)
		}
	}
	pulled := request(server, http.MethodGet, "/sync/pull/ios", nil)
	requireStatus(t, pulled, http.StatusOK)
	if !reflect.DeepEqual([]byte(pulled.Body.String()), []byte(payloads[0])) {
		found := false
		for _, payload := range payloads {
			if pulled.Body.String() == payload {
				found = true
			}
		}
		if !found {
			t.Fatalf("final snapshot is an interleaved or partial payload")
		}
	}
}

func TestCollaborationValidationLifecycleAndContract(t *testing.T) {
	server := testServer(t)
	putMember(t, server, "owner", RoleOwner)
	putMember(t, server, "viewer", RoleViewer)

	badRole := jsonRequest(t, server, http.MethodPut, "/members", Member{ID: "bad", Name: "Bad", Role: Role("editor")})
	requireStatus(t, badRole, http.StatusBadRequest)
	if _, ok := server.collaboration.Members["bad"]; ok {
		t.Fatal("invalid role was persisted")
	}

	branch := Branch{ID: "branch-1", Name: "Maternal", Kind: "maternal", IsSeeded: true, MemberIDs: []string{"owner", "viewer"}, CreatedAt: 123}
	createdBranch := putBranch(t, server, branch)
	requireStatus(t, createdBranch, http.StatusCreated)
	returnedBranch := decodeResponse[Branch](t, createdBranch)
	if returnedBranch.MemberIDs == nil || !returnedBranch.IsSeeded || returnedBranch.CreatedAt != 123 {
		t.Fatalf("branch contract/fields incorrect: %+v", returnedBranch)
	}
	duplicateBranch := putBranch(t, server, branch)
	requireStatus(t, duplicateBranch, http.StatusConflict)

	createdFolder := putFolder(t, server, Folder{ID: "folder-1", BranchID: "branch-1", Name: "Letters", MemberIDs: []string{}, CreatedAt: 456})
	requireStatus(t, createdFolder, http.StatusCreated)
	returnedFolder := decodeResponse[Folder](t, createdFolder)
	if returnedFolder.MemberIDs == nil || returnedFolder.CreatedAt != 456 {
		t.Fatalf("folder contract/fields incorrect: %+v", returnedFolder)
	}
	child := putFolder(t, server, Folder{ID: "folder-2", BranchID: "branch-1", ParentID: "folder-1", Name: "Child"})
	requireStatus(t, child, http.StatusCreated)
	cycle := jsonRequest(t, server, http.MethodPut, "/folders/folder-1", map[string]any{"parent_id": "folder-2"})
	requireStatus(t, cycle, http.StatusBadRequest)
	missingBranch := putFolder(t, server, Folder{ID: "missing", BranchID: "no-such-branch", Name: "Nope"})
	requireStatus(t, missingBranch, http.StatusBadRequest)
	missingParent := putFolder(t, server, Folder{ID: "missing-parent", BranchID: "branch-1", ParentID: "no-such-folder", Name: "Nope"})
	requireStatus(t, missingParent, http.StatusBadRequest)
	wrongBranch := putBranch(t, server, Branch{ID: "branch-2", Name: "Other", Kind: "custom"})
	requireStatus(t, wrongBranch, http.StatusCreated)
	crossBranchParent := putFolder(t, server, Folder{ID: "cross", BranchID: "branch-2", ParentID: "folder-1", Name: "Nope"})
	requireStatus(t, crossBranchParent, http.StatusBadRequest)

	partialUpdate := jsonRequest(t, server, http.MethodPut, "/branches/branch-1", map[string]any{"name": "Renamed"})
	requireStatus(t, partialUpdate, http.StatusOK)
	updatedBranch := decodeResponse[Branch](t, partialUpdate)
	if updatedBranch.Name != "Renamed" || !updatedBranch.IsSeeded || updatedBranch.CreatedAt != 123 || updatedBranch.Kind != "maternal" {
		t.Fatalf("partial update reset immutable fields: %+v", updatedBranch)
	}
	nonexistentUpdate := jsonRequest(t, server, http.MethodPut, "/branches/no-such", map[string]any{"name": "fake"})
	requireStatus(t, nonexistentUpdate, http.StatusNotFound)
	emptyBranchUpdate := jsonRequest(t, server, http.MethodPut, "/branches/branch-1", map[string]any{})
	requireStatus(t, emptyBranchUpdate, http.StatusBadRequest)
	nonexistentFolderUpdate := jsonRequest(t, server, http.MethodPut, "/folders/no-such", map[string]any{"name": "fake"})
	requireStatus(t, nonexistentFolderUpdate, http.StatusNotFound)
	emptyFolderUpdate := jsonRequest(t, server, http.MethodPut, "/folders/folder-1", map[string]any{})
	requireStatus(t, emptyFolderUpdate, http.StatusBadRequest)

	invite := createInvite(t, server, "owner", RoleOrganizer, []string{"branch-1"}, []string{})
	if invite.CreatedBy != "owner" || invite.Role != RoleOrganizer || invite.Code == "" {
		t.Fatalf("invitation response did not match Swift contract: %+v", invite)
	}
	lookup := request(server, http.MethodGet, "/invite/"+invite.Code, nil)
	requireStatus(t, lookup, http.StatusOK)
	accepted := jsonRequest(t, server, http.MethodPost, "/invite/"+invite.Code, map[string]string{"member_id": "new-member", "member_name": "New Member"})
	requireStatus(t, accepted, http.StatusOK)
	acceptedJSON := decodeResponse[map[string]any](t, accepted)
	if acceptedJSON["role"] != string(RoleOrganizer) {
		t.Fatalf("accepted role=%v", acceptedJSON["role"])
	}
	duplicateAccept := jsonRequest(t, server, http.MethodPost, "/invite/"+invite.Code, map[string]string{"member_id": "another", "member_name": "Another"})
	requireStatus(t, duplicateAccept, http.StatusNotFound)
	branchAfterAccept := decodeResponse[Branch](t, request(server, http.MethodGet, "/branches/branch-1", nil))
	if !contains(branchAfterAccept.MemberIDs, "new-member") {
		t.Fatalf("accepted member was not granted branch access: %+v", branchAfterAccept.MemberIDs)
	}

	// Committed API v1 contract: the creator of an invitation does not need to
	// exist as a member yet (the Swift capability probe invites a not-yet-added
	// member), so a missing creator is not an error.
	invalidCreator := jsonRequest(t, server, http.MethodPost, "/invite", map[string]any{"created_by": "missing", "role": RoleViewer})
	requireStatus(t, invalidCreator, http.StatusCreated)
	invalidInviteRole := jsonRequest(t, server, http.MethodPost, "/invite", map[string]any{"created_by": "owner", "role": "editor"})
	requireStatus(t, invalidInviteRole, http.StatusBadRequest)
	existingMemberInvite := createInvite(t, server, "owner", RoleViewer, nil, nil)
	existingAccept := jsonRequest(t, server, http.MethodPost, "/invite/"+existingMemberInvite.Code, map[string]string{"member_id": "viewer", "member_name": "Viewer"})
	requireStatus(t, existingAccept, http.StatusConflict)
	revoked := createInvite(t, server, "owner", RoleViewer, nil, nil)
	revokeResponse := request(server, http.MethodDelete, "/invite/"+revoked.Code, nil)
	requireStatus(t, revokeResponse, http.StatusNoContent)
	revokedLookup := request(server, http.MethodGet, "/invite/"+revoked.Code, nil)
	requireStatus(t, revokedLookup, http.StatusNotFound)

	expired := createInvite(t, server, "owner", RoleViewer, nil, nil)
	server.mu.Lock()
	stored := server.collaboration.Invitations[expired.Code]
	stored.Expires = server.now().Add(-time.Hour).UnixMilli()
	server.collaboration.Invitations[expired.Code] = stored
	server.mu.Unlock()
	expiredLookup := request(server, http.MethodGet, "/invite/"+expired.Code, nil)
	requireStatus(t, expiredLookup, http.StatusGone)

	deleteViewer := request(server, http.MethodDelete, "/members?id=viewer", nil)
	requireStatus(t, deleteViewer, http.StatusNoContent)
	branchAfterDelete := decodeResponse[Branch](t, request(server, http.MethodGet, "/branches/branch-1", nil))
	if contains(branchAfterDelete.MemberIDs, "viewer") {
		t.Fatal("deleted member left a stale branch reference")
	}
}

func TestCascadeDeletionAndOwnerProtection(t *testing.T) {
	server := testServer(t)
	putMember(t, server, "owner", RoleOwner)
	branch := Branch{ID: "branch", Name: "Branch", Kind: "custom"}
	requireStatus(t, putBranch(t, server, branch), http.StatusCreated)
	requireStatus(t, putFolder(t, server, Folder{ID: "root", BranchID: "branch", Name: "Root"}), http.StatusCreated)
	requireStatus(t, putFolder(t, server, Folder{ID: "child", BranchID: "branch", ParentID: "root", Name: "Child"}), http.StatusCreated)
	invite := createInvite(t, server, "owner", RoleViewer, []string{"branch"}, nil)
	deleteBranch := request(server, http.MethodDelete, "/branches/branch", nil)
	requireStatus(t, deleteBranch, http.StatusNoContent)
	if status := request(server, http.MethodGet, "/folders/root", nil).Code; status != http.StatusNotFound {
		t.Fatalf("root folder status=%d", status)
	}
	if status := request(server, http.MethodGet, "/folders/child", nil).Code; status != http.StatusNotFound {
		t.Fatalf("child folder status=%d", status)
	}
	if status := request(server, http.MethodGet, "/invite/"+invite.Code, nil).Code; status != http.StatusNotFound {
		t.Fatalf("deleted branch invitation status=%d", status)
	}
	deleteOwner := request(server, http.MethodDelete, "/members?id=owner", nil)
	requireStatus(t, deleteOwner, http.StatusConflict)
}

func TestPersistenceRestartMalformedStateAndMigration(t *testing.T) {
	dataDir := t.TempDir()
	keys := map[string]string{"test": sha256hex([]byte(testToken))}
	server, err := newServer(dataDir, keys, defaultLimits(), "test")
	if err != nil {
		t.Fatal(err)
	}
	putMember(t, server, "owner", RoleOwner)
	requireStatus(t, putBranch(t, server, Branch{ID: "branch", Name: "Branch", Kind: "custom"}), http.StatusCreated)
	requireStatus(t, putFolder(t, server, Folder{ID: "folder", BranchID: "branch", Name: "Folder"}), http.StatusCreated)

	restarted, err := newServer(dataDir, keys, defaultLimits(), "test")
	if err != nil {
		t.Fatal(err)
	}
	members := decodeResponse[[]Member](t, request(restarted, http.MethodGet, "/members", nil))
	if len(members) != 1 || members[0].ID != "owner" {
		t.Fatalf("members did not survive restart: %+v", members)
	}
	branches := decodeResponse[[]Branch](t, request(restarted, http.MethodGet, "/branches", nil))
	folders := decodeResponse[[]Folder](t, request(restarted, http.MethodGet, "/folders", nil))
	if len(branches) != 1 || len(folders) != 1 {
		t.Fatalf("family state did not survive restart: branches=%+v folders=%+v", branches, folders)
	}
	persisted, err := os.ReadFile(filepath.Join(dataDir, "collaboration.json"))
	if err != nil || !bytes.Contains(persisted, []byte(`"version": 1`)) {
		t.Fatalf("state was not versioned: %s (%v)", persisted, err)
	}

	malformedDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(malformedDir, "collaboration.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := newServer(malformedDir, keys, defaultLimits(), "test"); err == nil {
		t.Fatal("malformed collaboration state did not stop startup")
	}
	emptyDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(emptyDir, "collaboration.json"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := newServer(emptyDir, keys, defaultLimits(), "test"); err == nil {
		t.Fatal("empty collaboration state did not stop startup")
	}
	nullDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(nullDir, "collaboration.json"), []byte(`{"members":null}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := newServer(nullDir, keys, defaultLimits(), "test"); err == nil {
		t.Fatal("null collaboration collection did not stop startup")
	}

	legacyDir := t.TempDir()
	legacy := CollaborationState{
		Members:     map[string]Member{"owner": {ID: "owner", Name: "Owner", Role: Role("editor"), Since: 100}},
		Invitations: map[string]Invitation{},
		Branches:    map[string]Branch{"branch": {ID: "branch", Name: "Branch", Kind: "custom", MemberIDs: []string{"owner"}, CreatedAt: 100}},
		Folders:     map[string]Folder{},
	}
	legacyData, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "collaboration.json"), legacyData, 0600); err != nil {
		t.Fatal(err)
	}
	migrated, err := newServer(legacyDir, keys, defaultLimits(), "test")
	if err != nil {
		t.Fatal(err)
	}
	if migrated.collaboration.Members["owner"].Role != RoleParentAdmin {
		t.Fatalf("legacy editor was not explicitly migrated: %+v", migrated.collaboration.Members["owner"])
	}
	migratedData, err := os.ReadFile(filepath.Join(legacyDir, "collaboration.json"))
	if err != nil || !bytes.Contains(migratedData, []byte(`"version": 1`)) || bytes.Contains(migratedData, []byte(`"role": "editor"`)) {
		t.Fatalf("migration was not persisted: %s (%v)", migratedData, err)
	}
}

func TestSaveFailureDoesNotReportSuccessOrMutateMemory(t *testing.T) {
	server := testServer(t)
	path := filepath.Join(server.dataDir, "collaboration.json")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	recorder := jsonRequest(t, server, http.MethodPut, "/members", Member{ID: "owner", Name: "Owner", Role: RoleOwner})
	requireStatus(t, recorder, http.StatusInternalServerError)
	server.mu.RLock()
	_, exists := server.collaboration.Members["owner"]
	server.mu.RUnlock()
	if exists {
		t.Fatal("member was mutated in memory after persistence failure")
	}
}

func TestAPIKeyParsing(t *testing.T) {
	tests := []struct {
		name string
		data string
		good bool
	}{
		{name: "valid", data: "# comment\niphone:token-one\nandroid:token-two\n", good: true},
		{name: "missing separator", data: "iphone-token\n", good: false},
		{name: "blank token", data: "iphone:\n", good: false},
		{name: "duplicate name", data: "iphone:one\niphone:two\n", good: false},
		{name: "duplicate token", data: "iphone:one\nandroid:one\n", good: false},
		{name: "whitespace token", data: "iphone:token with spaces\n", good: false},
		{name: "comments only", data: "# nothing active\n\n", good: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "keys.txt")
			if err := os.WriteFile(path, []byte(test.data), 0600); err != nil {
				t.Fatal(err)
			}
			keys, err := loadAPIKeys(path, false)
			if test.good {
				if err != nil || len(keys) != 2 {
					t.Fatalf("keys=%v err=%v", keys, err)
				}
			} else if err == nil {
				t.Fatalf("malformed key file accepted: %+v", keys)
			}
		})
	}
	missing, err := loadAPIKeys(filepath.Join(t.TempDir(), "missing"), false)
	if err == nil || missing != nil {
		t.Fatalf("missing key file fallback was not rejected: keys=%v err=%v", missing, err)
	}
	dev, err := loadAPIKeys(filepath.Join(t.TempDir(), "missing"), true)
	if err != nil || len(dev) != 1 {
		t.Fatalf("explicit development fallback failed: keys=%v err=%v", dev, err)
	}
}

func TestDeterministicListOrdering(t *testing.T) {
	server := testServer(t)
	putMember(t, server, "owner", RoleOwner)
	for _, branch := range []Branch{
		{ID: "z-branch", Name: "Z", Kind: "custom", CreatedAt: 20},
		{ID: "a-branch", Name: "A", Kind: "custom", CreatedAt: 10},
	} {
		requireStatus(t, putBranch(t, server, branch), http.StatusCreated)
	}
	branches := decodeResponse[[]Branch](t, request(server, http.MethodGet, "/branches", nil))
	got := []string{branches[0].ID, branches[1].ID}
	if !reflect.DeepEqual(got, []string{"a-branch", "z-branch"}) {
		t.Fatalf("branch order=%v", got)
	}
	// The IDs are also a deterministic tie-breaker when timestamps match.
	for _, branch := range []Branch{
		{ID: "b-branch", Name: "B", Kind: "custom", CreatedAt: 10},
	} {
		requireStatus(t, putBranch(t, server, branch), http.StatusCreated)
	}
	branches = decodeResponse[[]Branch](t, request(server, http.MethodGet, "/branches", nil))
	ids := []string{branches[0].ID, branches[1].ID, branches[2].ID}
	want := append([]string(nil), ids...)
	sort.Strings(want[0:2])
	if ids[0] != "a-branch" || ids[1] != "b-branch" || ids[2] != "z-branch" {
		t.Fatalf("tie ordering=%v", ids)
	}
}

func TestJSONBodyValidation(t *testing.T) {
	server := testServer(t)
	invalid := request(server, http.MethodPut, "/members", strings.NewReader(`{"id":"x"} trailing`))
	requireStatus(t, invalid, http.StatusBadRequest)
	oversized := request(server, http.MethodPut, "/members", strings.NewReader(`{"id":"`+strings.Repeat("x", 300)+`","name":"Member","role":"viewer"}`))
	requireStatus(t, oversized, http.StatusRequestEntityTooLarge)
}

func TestIdentifierAndBranchKindValidation(t *testing.T) {
	for _, value := range []string{"", ".", "..", "../escape", `..\\escape`, "a/b", "a b", "é", strings.Repeat("a", 129), "-starts-with-dash"} {
		if validIdentifier(value) {
			t.Fatalf("unsafe identifier accepted: %q", value)
		}
	}
	for _, value := range []string{"a", "A-1", "uuid_1", "attachment.jpg"} {
		if !validIdentifier(value) {
			t.Fatalf("safe identifier rejected: %q", value)
		}
	}
	server := testServer(t)
	putMember(t, server, "owner", RoleOwner)
	badKind := putBranch(t, server, Branch{ID: "branch", Name: "Branch", Kind: "not-a-kind"})
	requireStatus(t, badKind, http.StatusBadRequest)
}

func TestByteCountParsing(t *testing.T) {
	valid := []struct {
		text string
		want int64
	}{
		{"512", 512},
		{"1K", 1024},
		{"2M", 2 << 20},
		{"1G", 1 << 30},
		{"1g", 1 << 30},
		{" 4k ", 4 << 10},
	}
	for _, test := range valid {
		got, err := parseByteCount(test.text)
		if err != nil || got != test.want {
			t.Fatalf("parseByteCount(%q) = %d, %v; want %d", test.text, got, err, test.want)
		}
	}
	invalid := []string{"", "0", "-5", "abc", "1.5M", "1X", "999999999999999999999999G"}
	for _, text := range invalid {
		if _, err := parseByteCount(text); err == nil {
			t.Fatalf("parseByteCount(%q) succeeded, want error", text)
		}
	}
}

func TestLimitsFromEnv(t *testing.T) {
	// Invalid MAX_UPLOAD_BYTES must fail startup, not silently become 0.
	t.Setenv("MAX_UPLOAD_BYTES", "not-a-number")
	if _, err := limitsFromEnv(); err == nil {
		t.Fatal("invalid MAX_UPLOAD_BYTES did not fail")
	}
	// MAX_UPLOAD_BYTES applies to upload limits but not the JSON body limit.
	t.Setenv("MAX_UPLOAD_BYTES", "64M")
	limits, err := limitsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if limits.Sync != 64<<20 || limits.Attachment != 64<<20 || limits.Backup != 64<<20 {
		t.Fatalf("MAX_UPLOAD_BYTES not applied to upload limits: %+v", limits)
	}
	if limits.JSONBody != defaultJSONBodyLimit {
		t.Fatalf("MAX_UPLOAD_BYTES must not change JSON body limit: %+v", limits)
	}
	// Granular variables take precedence over the shared knob.
	t.Setenv("MAX_BACKUP_SIZE", "1G")
	limits, err = limitsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if limits.Backup != 1<<30 || limits.Sync != 64<<20 {
		t.Fatalf("granular override failed: %+v", limits)
	}
	// Negative and zero are rejected.
	t.Setenv("MAX_BACKUP_SIZE", "-1")
	if _, err := limitsFromEnv(); err == nil {
		t.Fatal("negative MAX_BACKUP_SIZE did not fail")
	}
	t.Setenv("MAX_BACKUP_SIZE", "0")
	if _, err := limitsFromEnv(); err == nil {
		t.Fatal("zero MAX_BACKUP_SIZE did not fail")
	}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
