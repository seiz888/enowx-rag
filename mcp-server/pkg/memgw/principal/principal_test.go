package principal

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// The intersection rule is pure logic, so it is tested without a database. The
// integration tests in scope_test.go then prove the stored form behaves the
// same way, which is the pair that matters: a rule that is right in Go and
// wrong in SQL is still wrong.

func ptr[T any](v T) *T { return &v }

func TestGrantCoversNarrowsOnly(t *testing.T) {
	projectA := uuid.New()
	projectB := uuid.New()
	wsA := uuid.New()
	wsB := uuid.New()

	projectWide := Grant{ProjectID: projectA}
	workspacePinned := Grant{ProjectID: projectA, WorkspaceID: &wsA}

	cases := []struct {
		name  string
		grant Grant
		scope Scope
		want  bool
	}{
		{"same project", projectWide, Scope{ProjectID: projectA}, true},
		{"other project", projectWide, Scope{ProjectID: projectB}, false},
		{"project grant admits any workspace", projectWide, Scope{ProjectID: projectA, WorkspaceID: &wsB}, true},
		{"pinned workspace admits itself", workspacePinned, Scope{ProjectID: projectA, WorkspaceID: &wsA}, true},
		{"pinned workspace refuses another", workspacePinned, Scope{ProjectID: projectA, WorkspaceID: &wsB}, false},
		{"pinned workspace refuses an unnamed one", workspacePinned, Scope{ProjectID: projectA}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.grant.Covers(tc.scope); got != tc.want {
				t.Fatalf("Covers = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGrantActive(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	if (Grant{}).Active(now) != true {
		t.Fatal("a grant with no expiry and no revocation is active")
	}
	if (Grant{ExpiresAt: ptr(now.Add(-time.Second))}).Active(now) {
		t.Fatal("an expired grant is not active")
	}
	if (Grant{RevokedAt: ptr(now.Add(-time.Second))}).Active(now) {
		t.Fatal("a revoked grant is not active")
	}
	if !(Grant{ExpiresAt: ptr(now.Add(time.Second))}).Active(now) {
		t.Fatal("a grant expiring later is still active")
	}
}
