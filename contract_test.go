package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These fixtures mirror the CodingKeys and required properties in the
// read-only LettersToMy self-hosted adapter (Sources/LettersToMyCore/
// SelfHostedAPI.swift). They make the wire contract explicit without
// modifying or importing the companion Swift repository.
func TestLettersToMyWireContractFixtures(t *testing.T) {
	invitationJSON := []byte(`{"code":"ABC123DEF456","created_by":"owner","role":"organizer","branch_ids":["branch-1"],"folder_ids":[],"expires":1700000000000}`)
	var invitation Invitation
	if err := json.Unmarshal(invitationJSON, &invitation); err != nil {
		t.Fatal(err)
	}
	if invitation.CreatedBy != "owner" || invitation.Role != RoleOrganizer || invitation.Expires != 1700000000000 {
		t.Fatalf("invitation fixture mismatch: %+v", invitation)
	}

	memberJSON := []byte(`{"id":"member-1","name":"Member","role":"parentAdmin","since":1700000000000}`)
	var member Member
	if err := json.Unmarshal(memberJSON, &member); err != nil {
		t.Fatal(err)
	}
	if member.Role != RoleParentAdmin || member.Since != 1700000000000 {
		t.Fatalf("member fixture mismatch: %+v", member)
	}

	branchJSON := []byte(`{"id":"branch-1","name":"Family","kind":"chosenFamily","is_seeded":false,"member_ids":[],"created_at":1700000000000}`)
	var branch Branch
	if err := json.Unmarshal(branchJSON, &branch); err != nil {
		t.Fatal(err)
	}
	if branch.MemberIDs == nil || branch.CreatedAt != 1700000000000 {
		t.Fatalf("branch fixture mismatch: %+v", branch)
	}

	folderJSON := []byte(`{"id":"folder-1","branch_id":"branch-1","parent_id":null,"name":"Letters","member_ids":[],"created_at":1700000000000}`)
	var folder Folder
	if err := json.Unmarshal(folderJSON, &folder); err != nil {
		t.Fatal(err)
	}
	if folder.ParentID != "" || folder.MemberIDs == nil || folder.CreatedAt != 1700000000000 {
		t.Fatalf("folder fixture mismatch: %+v", folder)
	}

	encoded, err := json.Marshal(folder)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if _, exists := wire["parent_id"]; exists {
		t.Fatalf("nil Swift parentFolderID should be omitted, got %s", encoded)
	}
	if _, exists := wire["member_ids"]; !exists {
		t.Fatalf("folder member_ids must be present, got %s", encoded)
	}
}

// TestStatusIdentityContract locks the /status identity fields the Swift
// client validates: service, api_version (==1), server_version, and
// capabilities must include collaboration/backups/attachments.
func TestStatusIdentityContract(t *testing.T) {
	server := testServer(t)
	recorder := request(server, http.MethodGet, "/status", nil)
	requireStatus(t, recorder, http.StatusOK)
	status := decodeResponse[StatusResponse](t, recorder)

	if status.Service != serviceName {
		t.Fatalf("service=%q want %q", status.Service, serviceName)
	}
	if status.APIVersion != apiVersion {
		t.Fatalf("api_version=%d want %d", status.APIVersion, apiVersion)
	}
	if status.ServerVersion == "" {
		t.Fatal("server_version must not be empty")
	}
	for _, wanted := range []string{"collaboration", "backups", "attachments"} {
		found := false
		for _, c := range status.Capabilities {
			if c == wanted {
				found = true
			}
		}
		if !found {
			t.Fatalf("capabilities missing %q: %v", wanted, status.Capabilities)
		}
	}
	// Collections must be arrays, never null.
	if status.Syncs == nil || status.Attachments == nil || status.Recoveries == nil {
		t.Fatalf("collections must be non-null arrays: %+v", status)
	}
}

// TestBackupMetadataContract locks the backup push response shape the Swift
// client decodes: id, timestamp, size, and letter_count must all be present.
func TestBackupMetadataContract(t *testing.T) {
	server := testServer(t)
	recorder := request(server, http.MethodPut, "/backup/push?id=meta-probe&letter_count=7", strings.NewReader("archive-bytes"))
	requireStatus(t, recorder, http.StatusOK)
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode push response: %v", err)
	}
	for _, key := range []string{"id", "timestamp", "size", "letter_count"} {
		if _, exists := body[key]; !exists {
			t.Fatalf("backup push response missing %q: %s", key, recorder.Body.String())
		}
	}
	if body["id"] != "meta-probe" || body["letter_count"] != float64(7) {
		t.Fatalf("backup push response wrong values: %s", recorder.Body.String())
	}

	// The list must report the persisted letter_count from the sidecar.
	listRecorder := request(server, http.MethodGet, "/backup/list", nil)
	requireStatus(t, listRecorder, http.StatusOK)
	backups := decodeResponse[[]RecoveryMeta](t, listRecorder)
	if len(backups) != 1 || backups[0].ID != "meta-probe" || backups[0].LetterCount != 7 {
		t.Fatalf("backup list metadata mismatch: %+v", backups)
	}

	// A push without letter_count defaults to 0 and stays decodable.
	plain := request(server, http.MethodPut, "/backup/push?id=plain-probe", strings.NewReader("x"))
	requireStatus(t, plain, http.StatusOK)
	var plainBody map[string]any
	if err := json.Unmarshal(plain.Body.Bytes(), &plainBody); err != nil {
		t.Fatal(err)
	}
	if plainBody["letter_count"] != float64(0) {
		t.Fatalf("omitted letter_count should default to 0: %s", plain.Body.String())
	}

	// A negative or non-numeric letter_count is rejected.
	bad := request(server, http.MethodPut, "/backup/push?id=bad-probe&letter_count=-1", strings.NewReader("x"))
	requireStatus(t, bad, http.StatusBadRequest)
	bad = request(server, http.MethodPut, "/backup/push?id=bad-probe2&letter_count=abc", strings.NewReader("x"))
	requireStatus(t, bad, http.StatusBadRequest)
}

// TestAttachmentListContract locks /attachment/list metadata: id,
// content_type, and size are all present for every attachment.
func TestAttachmentListContract(t *testing.T) {
	server := testServer(t)
	requireStatus(t, request(server, http.MethodPut, "/attachment/upload?id=photo.jpg", strings.NewReader("jpeg-bytes")), http.StatusOK)

	recorder := request(server, http.MethodGet, "/attachment/list", nil)
	requireStatus(t, recorder, http.StatusOK)
	attachments := decodeResponse[[]AttachmentMeta](t, recorder)
	if len(attachments) != 1 || attachments[0].ID != "photo.jpg" || attachments[0].ContentType != "image/jpeg" {
		t.Fatalf("attachment list metadata mismatch: %+v", attachments)
	}

	// DELETE /attachment/:id must work and then 404.
	del := request(server, http.MethodDelete, "/attachment/photo.jpg", nil)
	requireStatus(t, del, http.StatusNoContent)
	after := request(server, http.MethodGet, "/attachment/list", nil)
	requireStatus(t, after, http.StatusOK)
	if got := len(decodeResponse[[]AttachmentMeta](t, after)); got != 0 {
		t.Fatalf("attachment list after delete has %d entries", got)
	}
}

// TestBackupDeleteContract locks DELETE /backup/:id.
func TestBackupDeleteContract(t *testing.T) {
	server := testServer(t)
	requireStatus(t, request(server, http.MethodPut, "/backup/push?id=del-probe", strings.NewReader("x")), http.StatusOK)
	del := request(server, http.MethodDelete, "/backup/del-probe", nil)
	requireStatus(t, del, http.StatusNoContent)
	after := request(server, http.MethodGet, "/backup/list", nil)
	requireStatus(t, after, http.StatusOK)
	if got := len(decodeResponse[[]RecoveryMeta](t, after)); got != 0 {
		t.Fatalf("backup list after delete has %d entries", got)
	}
	missing := request(server, http.MethodDelete, "/backup/never-existed", nil)
	requireStatus(t, missing, http.StatusNotFound)
}

// TestErrorCodeNormalizationContract locks the public API v1 error-code
// vocabulary the Swift client maps to typed errors. Internal diagnostic
// codes must be normalized onto the stable set.
func TestErrorCodeNormalizationContract(t *testing.T) {
	server := testServer(t)

	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		noAuth     bool
		wantStatus int
		wantCode   string
	}{
		{"unauthorized", http.MethodGet, "/status", "", true, http.StatusUnauthorized, "unauthorized"},
		{"not found backup", http.MethodGet, "/backup/pull/missing", "", false, http.StatusNotFound, "not_found"},
		{"not found folder", http.MethodGet, "/folders/missing", "", false, http.StatusNotFound, "not_found"},
		{"invalid id attachment", http.MethodPut, "/attachment/upload?id=../escape", "x", false, http.StatusBadRequest, "invalid_request"},
		{"invalid platform", http.MethodPut, "/sync/push/windows", "x", false, http.StatusBadRequest, "invalid_request"},
		{"invalid role", http.MethodPut, "/members", `{"id":"m1","name":"M","role":"superuser"}`, false, http.StatusBadRequest, "invalid_request"},
		{"method not allowed", http.MethodPost, "/sync/list", "", false, http.StatusMethodNotAllowed, "method_not_allowed"},
		{"invalid json", http.MethodPut, "/members", "{not-json", false, http.StatusBadRequest, "invalid_request"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			if test.method == http.MethodPut || test.method == http.MethodPost {
				req.Header.Set("Content-Type", "application/json")
			}
			if !test.noAuth {
				req.Header.Set("Authorization", "Bearer "+testToken)
			}
			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, req)

			if recorder.Code != test.wantStatus {
				t.Fatalf("status=%d want=%d body=%s", recorder.Code, test.wantStatus, recorder.Body.String())
			}
			var envelope struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("error body not structured JSON: %v (%s)", err, recorder.Body.String())
			}
			if envelope.Error.Code != test.wantCode {
				t.Fatalf("error.code=%q want=%q body=%s", envelope.Error.Code, test.wantCode, recorder.Body.String())
			}
		})
	}

	// Conflict path: accepting an invite as an already-existing member must
	// be 409 with the client-compatible "conflict" code.
	putMember(t, server, "existing", RoleViewer)
	invite := createInvite(t, server, "nobody", RoleViewer, nil, nil)
	conflict := jsonRequest(t, server, http.MethodPost, "/invite/"+invite.Code, map[string]string{"member_id": "existing", "member_name": "Existing"})
	requireStatus(t, conflict, http.StatusConflict)
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(conflict.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("conflict body not structured JSON: %v (%s)", err, conflict.Body.String())
	}
	if envelope.Error.Code != "conflict" {
		t.Fatalf("conflict error.code=%q want %q body=%s", envelope.Error.Code, "conflict", conflict.Body.String())
	}
}

// TestInviteWithoutCreatorContract locks that an invite may be created for a
// not-yet-existing member (the Swift capability probe does exactly this).
func TestInviteWithoutCreatorContract(t *testing.T) {
	server := testServer(t)
	recorder := jsonRequest(t, server, http.MethodPost, "/invite", map[string]any{
		"created_by": "not-yet-a-member",
		"role":       "organizer",
		"branch_ids": []string{},
		"folder_ids": []string{},
	})
	requireStatus(t, recorder, http.StatusCreated)
	invite := decodeResponse[Invitation](t, recorder)
	if invite.Role != RoleOrganizer {
		t.Fatalf("invite role=%q", invite.Role)
	}
}

func containsJSONKey(data []byte, key string) bool {
	var object map[string]any
	if json.Unmarshal(data, &object) != nil {
		return false
	}
	_, exists := object[key]
	return exists
}
