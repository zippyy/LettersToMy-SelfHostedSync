package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Regression tests for defects found in the adversarial release review.
// Each test reproduces a confirmed defect; the fix makes it green.

// F1 — POST /invite without a role must default to viewer (documented),
// not 500. The handler stored request.Role (empty) instead of the
// defaulted inviteRole, so validateState rejected the persisted invite
// and the commit returned 500 storage_failure.
func TestInviteWithoutRoleDefaultsToViewer(t *testing.T) {
	server := testServer(t)
	recorder := jsonRequest(t, server, http.MethodPost, "/invite", map[string]any{
		"created_by": "not-yet-a-member",
	})
	requireStatus(t, recorder, http.StatusCreated)
	invite := decodeResponse[Invitation](t, recorder)
	if invite.Role != RoleViewer {
		t.Fatalf("invite without role: got role=%q want %q", invite.Role, RoleViewer)
	}
	// And the persisted copy must also carry the defaulted role.
	server.mu.RLock()
	invite = server.collaboration.Invitations[invite.Code]
	server.mu.RUnlock()
	if invite.Role != RoleViewer {
		t.Fatalf("persisted invite role=%q want viewer", invite.Role)
	}
}

// F2 — PUT /members must not allow demoting the FINAL owner, or the
// workspace becomes permanently ownerless (the last-owner protection on
// DELETE is trivially bypassed by demoting first).
func TestCannotDemoteFinalOwner(t *testing.T) {
	server := testServer(t)
	putMember(t, server, "owner-id", RoleOwner)

	recorder := jsonRequest(t, server, http.MethodPut, "/members", Member{
		ID: "owner-id", Name: "Owner", Role: RoleViewer,
	})
	requireStatus(t, recorder, http.StatusConflict)
	// The owner must remain owner.
	server.mu.RLock()
	member, exists := server.collaboration.Members["owner-id"]
	server.mu.RUnlock()
	if !exists || member.Role != RoleOwner {
		t.Fatalf("final owner was demoted: %+v", member)
	}
}

// F2b — with TWO owners, demoting one is legal.
func TestCanDemoteOwnerWhenAnotherOwnerRemains(t *testing.T) {
	server := testServer(t)
	putMember(t, server, "owner-1", RoleOwner)
	putMember(t, server, "owner-2", RoleOwner)

	recorder := jsonRequest(t, server, http.MethodPut, "/members", Member{
		ID: "owner-2", Name: "Owner 2", Role: RoleOrganizer,
	})
	requireStatus(t, recorder, http.StatusOK)
	server.mu.RLock()
	member := server.collaboration.Members["owner-2"]
	server.mu.RUnlock()
	if member.Role != RoleOrganizer {
		t.Fatalf("second owner demote: got %q want organizer", member.Role)
	}
}

// F3 — DELETE /backup/<id> when no archive exists but a sidecar does
// must remove the orphaned sidecar instead of leaving it behind forever.
func TestBackupDeleteCleansOrphanedSidecar(t *testing.T) {
	server := testServer(t)
	sidecar := filepath.Join(server.dataDir, "backup", "ghost.letterstomy.meta")
	if err := os.WriteFile(sidecar, []byte(`{"timestamp":1,"letter_count":5}`), 0600); err != nil {
		t.Fatal(err)
	}

	recorder := request(server, http.MethodDelete, "/backup/ghost", nil)
	requireStatus(t, recorder, http.StatusNotFound)
	if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
		t.Fatalf("orphaned sidecar was not cleaned up after 404 delete")
	}
}

// F4 — backup push must not replace the archive when the metadata
// sidecar write fails. Reproduction: block the sidecar path with a
// directory, push different bytes, expect 500 AND the archive unchanged.
func TestBackupPushArchiveUnchangedWhenMetaWriteFails(t *testing.T) {
	server := testServer(t)
	backupDir := filepath.Join(server.dataDir, "backup")
	archivePath := filepath.Join(backupDir, "ord.letterstomy")

	// Initial push with letter_count=5.
	rc := request(server, http.MethodPut, "/backup/push?id=ord&letter_count=5", strings.NewReader("AAAA"))
	requireStatus(t, rc, http.StatusOK)

	// Sabotage: replace the sidecar file with a directory so the meta
	// write's rename fails.
	metaPath := filepath.Join(backupDir, "ord.letterstomy.meta")
	if err := os.Remove(metaPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(metaPath, 0755); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(metaPath)

	// Push different bytes with letter_count=9 — must FAIL and leave A.
	rc = request(server, http.MethodPut, "/backup/push?id=ord&letter_count=9", strings.NewReader("BBBB"))
	requireStatus(t, rc, http.StatusInternalServerError)

	data, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "AAAA" {
		t.Fatalf("archive was replaced despite meta failure: got %q want \"AAAA\"", data)
	}
}
