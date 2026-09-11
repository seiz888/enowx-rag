//go:build windows

package collector

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestTheKeyOnDiskIsNotTheKey(t *testing.T) {
	// The whole reason DPAPI is here. A key file that contained the key would
	// make the spool's encryption a formality: whoever could copy the spool
	// could copy the thing that opens it.
	path := filepath.Join(t.TempDir(), "spool.key")
	key, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != KeyLen {
		t.Fatalf("the key is %d bytes", len(key))
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(blob, key) {
		t.Fatal("the key file contains the key in clear")
	}
	if len(blob) <= KeyLen {
		t.Fatalf("the key file is %d bytes, which is too small to be a wrapped blob", len(blob))
	}

	// Second call returns the same key rather than replacing it. A key that
	// changed on restart would leave every queued row unreadable.
	again, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(key, again) {
		t.Fatal("reopening the key file produced a different key")
	}
}

func TestAKeyFileThatCannotBeUnwrappedIsNotSilentlyReplaced(t *testing.T) {
	// The realistic cause is a spool directory copied from another account or
	// machine. Generating a fresh key there would leave the queue permanently
	// unreadable while looking like a successful start.
	path := filepath.Join(t.TempDir(), "spool.key")
	if err := os.WriteFile(path, bytes.Repeat([]byte{0xAB}, 128), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateKey(path); err == nil {
		t.Fatal("an unwrappable key file was accepted")
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(blob, bytes.Repeat([]byte{0xAB}, 128)) {
		t.Fatal("the unreadable key file was overwritten instead of reported")
	}
}
