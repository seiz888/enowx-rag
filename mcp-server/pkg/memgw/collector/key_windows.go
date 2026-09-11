//go:build windows

package collector

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The spool key at rest.
//
// The key is generated once, on first use, and written to disk wrapped by
// DPAPI in user scope. That choice is the whole point: the wrapped blob is
// bound to this Windows user account on this machine, so copying the spool
// directory to another machine, or reading it as another local user, yields a
// file that will not unwrap. A passphrase would have been worse -- it would
// live in a config file or an environment variable, which is where secrets go
// to be copied into transcripts.
//
// An entropy string is mixed in so that a blob taken from this file cannot be
// unwrapped by some other program running as the same user that happens to call
// CryptUnprotectData on whatever it finds.
var keyEntropy = []byte("enowx-rag memgw collector spool key v1")

// LoadOrCreateKey returns the spool key held at path, creating it on first use.
//
// It is deliberate that there is no "export key" and no way to supply a key
// from the environment. A key the operator can print is a key that ends up in a
// terminal scrollback.
func LoadOrCreateKey(path string) ([]byte, error) {
	blob, err := os.ReadFile(path)
	switch {
	case err == nil:
		key, err := dpapiUnprotect(blob)
		if err != nil {
			// Not "corrupt": the usual cause is a spool directory copied from
			// another account or machine, and deleting it would destroy queued
			// events that are still perfectly good under the right key.
			return nil, fmt.Errorf("collector: the key file at %s cannot be unwrapped by this user on this machine (%w)", path, err)
		}
		if len(key) != KeyLen {
			return nil, fmt.Errorf("collector: the key file at %s holds %d bytes, want %d", path, len(key), KeyLen)
		}
		return key, nil
	case errors.Is(err, fs.ErrNotExist):
	default:
		return nil, err
	}

	key := make([]byte, KeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	wrapped, err := dpapiProtect(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// O_EXCL, so two collectors starting at once cannot both believe they
	// created the key: the loser reads the winner's file on the next attempt
	// rather than overwriting a key that already has ciphertext depending on it.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return LoadOrCreateKey(path)
		}
		return nil, err
	}
	if _, err := f.Write(wrapped); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return key, nil
}

func dpapiProtect(in []byte) ([]byte, error) {
	var out windows.DataBlob
	inBlob := blobOf(in)
	entropy := blobOf(keyEntropy)
	if err := windows.CryptProtectData(&inBlob, nil, &entropy, 0, nil,
		windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}

func dpapiUnprotect(in []byte) ([]byte, error) {
	var out windows.DataBlob
	inBlob := blobOf(in)
	entropy := blobOf(keyEntropy)
	if err := windows.CryptUnprotectData(&inBlob, nil, &entropy, 0, nil,
		windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	key := append([]byte(nil), unsafe.Slice(out.Data, out.Size)...)
	return key, nil
}

func blobOf(b []byte) windows.DataBlob {
	if len(b) == 0 {
		return windows.DataBlob{Size: 0, Data: nil}
	}
	return windows.DataBlob{Size: uint32(len(b)), Data: &b[0]}
}
