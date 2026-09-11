//go:build linux

package collector

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCredential lays out what systemd would: a 0700 directory holding a file
// only its owner can read, with CREDENTIALS_DIRECTORY pointing at it.
func writeCredential(t *testing.T, content []byte, mode os.FileMode) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, spoolCredentialName)
	if err := os.WriteFile(file, content, mode); err != nil {
		t.Fatal(err)
	}
	// WriteFile applies the umask; the test is about the mode on disk.
	if err := os.Chmod(file, mode); err != nil {
		t.Fatal(err)
	}
	t.Setenv(credentialsDirEnv, dir)
	return dir
}

func TestWithoutACredentialTheCollectorRefusesInsteadOfMakingAKey(t *testing.T) {
	// The whole reason this file differs from the Windows one. A key this
	// process could create is a key it would write beside the ciphertext it
	// protects, and that is not encryption at rest, it is a chmod.
	t.Setenv(credentialsDirEnv, "")
	path := filepath.Join(t.TempDir(), "spool.key")
	_, err := LoadOrCreateKey(path)
	if err == nil {
		t.Fatal("a spool key was produced with no credential from the service manager")
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Fatal("a plaintext key file was written")
	}
	// The refusal has to be actionable: an operator reading it at a terminal
	// should not need the runbook to get past it.
	if !strings.Contains(err.Error(), "systemd-creds encrypt") ||
		!strings.Contains(err.Error(), "LoadCredentialEncrypted") {
		t.Fatalf("the refusal does not say how to fix it: %v", err)
	}
}

func TestACredentialReadableBeyondItsOwnerIsRefused(t *testing.T) {
	// systemd writes credentials into a 0700 ramfs, readable by the owner only.
	// Anything looser did not come from systemd, and the key cannot be un-read.
	writeCredential(t, bytes.Repeat([]byte{0x11}, KeyLen), 0o644)
	_, err := LoadOrCreateKey("/nonexistent/spool.key")
	if err == nil {
		t.Fatal("a world-readable credential was accepted")
	}
	if !strings.Contains(err.Error(), "readable beyond its owner") {
		t.Fatalf("the refusal does not say what went wrong: %v", err)
	}
}

func TestTheCredentialIsAcceptedRawOrAsHex(t *testing.T) {
	want := bytes.Repeat([]byte{0x2b}, KeyLen)

	writeCredential(t, want, 0o400)
	raw, err := LoadOrCreateKey("/nonexistent/spool.key")
	if err != nil {
		t.Fatalf("a raw %d-byte credential was refused: %v", KeyLen, err)
	}
	if !bytes.Equal(raw, want) {
		t.Fatal("the raw credential was not returned unchanged")
	}

	// The shape a human produces, newline and all.
	writeCredential(t, []byte(hex.EncodeToString(want)+"\n"), 0o400)
	fromHex, err := LoadOrCreateKey("/nonexistent/spool.key")
	if err != nil {
		t.Fatalf("a hex credential was refused: %v", err)
	}
	if !bytes.Equal(fromHex, want) {
		t.Fatal("the hex credential decoded to a different key")
	}
}

func TestAShortCredentialIsNotStretchedIntoAKey(t *testing.T) {
	// Hashing a weak secret up to 32 bytes would hide how weak it was, and the
	// spool would be encrypted with something an operator could guess.
	writeCredential(t, []byte("hunter2"), 0o400)
	_, err := LoadOrCreateKey("/nonexistent/spool.key")
	if err == nil {
		t.Fatal("a 7-byte credential was accepted as a 32-byte key")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("the refusal quotes the secret: %v", err)
	}
}

func TestAMissingCredentialNamesTheOneItWanted(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(credentialsDirEnv, dir)
	_, err := LoadOrCreateKey("/nonexistent/spool.key")
	if err == nil {
		t.Fatal("an absent credential was accepted")
	}
	if !strings.Contains(err.Error(), spoolCredentialName) {
		t.Fatalf("the refusal does not name the credential: %v", err)
	}
}
