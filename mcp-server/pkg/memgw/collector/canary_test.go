package collector

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What is actually on the disk.
//
// "The payload is encrypted" is a claim about bytes, and the only way to check
// a claim about bytes is to read the bytes. This test writes a canary -- a
// distinctive string that is not a secret and never appears anywhere else --
// through the whole enqueue path, then reads every file the spool touched,
// including the write-ahead log and the shared-memory file, and asserts the
// canary is not in any of them.
//
// It also asserts the opposite for the metadata, which is the honest half: the
// project id, the event type and the idempotency key ARE in plaintext on disk,
// deliberately, so the forwarder can order and deduplicate without the key. A
// test that only checked the good news would let the documentation drift into a
// claim of whole-database encryption that was never true.

const canary = "CANARY-9f4b2c-not-a-secret-just-distinctive"

func spoolFiles(t *testing.T, path string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	dir := filepath.Dir(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		full := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(full)
		if err != nil {
			// A -shm file can be locked by the open connection on Windows. Say
			// so rather than skipping quietly: an unreadable file is a file this
			// test did not check.
			t.Logf("could not read %s: %v", e.Name(), err)
			continue
		}
		out[e.Name()] = b
	}
	return out
}

func TestThePayloadIsNotOnDiskInClear(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spool.db")
	s, err := OpenSpool(path, testKey(), SpoolConfig{})
	if err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]any{
		"idempotency_key": "canary:1",
		"project_id":      "11111111-1111-1111-1111-111111111111",
		"type":            "checkpoint.written",
		"payload":         map[string]any{"summary": canary, "resume_point": canary},
	})
	if _, err := s.Enqueue(t.Context(), Event{
		IdempotencyKey: "canary:1",
		ProjectID:      "11111111-1111-1111-1111-111111111111",
		Type:           "checkpoint.written",
		Body:           body,
	}); err != nil {
		t.Fatal(err)
	}

	// While the connection is still open: the row may be in the WAL and not yet
	// in the main file, and the WAL is exactly where an unencrypted payload
	// would be found by somebody who copied the directory.
	for name, content := range spoolFiles(t, path) {
		if bytes.Contains(content, []byte(canary)) {
			t.Fatalf("the payload is in clear in %s while the spool is open", name)
		}
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// After the close, which checkpoints the WAL into the database file.
	files := spoolFiles(t, path)
	if len(files) == 0 {
		t.Fatal("no spool files were found to inspect")
	}
	for name, content := range files {
		if bytes.Contains(content, []byte(canary)) {
			t.Fatalf("the payload is in clear in %s after the close", name)
		}
	}

	// The honest half. These are in plaintext by design, and the package
	// documentation says so; if that ever stops being true the documentation is
	// the thing to change, and this test is what will notice.
	joined := ""
	for _, content := range files {
		joined += string(content)
	}
	for _, expected := range []string{"canary:1", "checkpoint.written", "11111111-1111-1111-1111-111111111111"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("expected the plaintext metadata %q on disk; if it is now encrypted, "+
				"update the package documentation, which promises only payload encryption", expected)
		}
	}
}

func TestAFailureReasonDoesNotBecomeACopyOfThePayload(t *testing.T) {
	// The one field that could leak content by accident. The forwarder stores a
	// class and a class-derived sentence, never the server's message, because a
	// server message can quote the submission it refused.
	dir := t.TempDir()
	path := filepath.Join(dir, "spool.db")
	s, err := OpenSpool(path, testKey(), SpoolConfig{MaxAttempts: 5, BaseBackoff: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue(t.Context(), event("a", "work.planned", canary)); err != nil {
		t.Fatal(err)
	}
	row, err := s.Claim(t.Context())
	if err != nil || row == nil {
		t.Fatalf("claim: %v", err)
	}
	// A careless caller passes the whole server body through as a class. The
	// closed table is what stops any of it reaching the disk.
	if _, err := s.Fail(t.Context(), row.Seq, "policy_rejected "+strings.Repeat(canary+" ", 50), false); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := s.db.QueryRow(`SELECT fail_reason FROM spool WHERE seq = ?`, row.Seq).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, canary) {
		t.Fatalf("caller text reached the failure reason column: %q", stored)
	}
	var storedClass string
	if err := s.db.QueryRow(`SELECT fail_class FROM spool WHERE seq = ?`, row.Seq).Scan(&storedClass); err != nil {
		t.Fatal(err)
	}
	if storedClass != "unclassified" {
		t.Fatalf("an unrecognised class was stored verbatim as %q", storedClass)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// And nothing of it reached the disk at all. The closed class table makes
	// that provable rather than probable: there is no path by which caller text
	// becomes a plaintext column.
	count := 0
	for _, content := range spoolFiles(t, path) {
		count += bytes.Count(content, []byte(canary))
	}
	if count != 0 {
		t.Fatalf("the canary appears %d times in the spool files; a failure path is writing payload content in clear", count)
	}
}
