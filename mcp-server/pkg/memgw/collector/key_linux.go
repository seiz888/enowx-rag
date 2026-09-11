//go:build linux

package collector

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The spool key on Linux.
//
// On Windows the key is generated on first use and wrapped with DPAPI, so the
// process can create its own key and still not hold anything an attacker can
// copy to another machine. Linux has no per-user wrapping primitive the process
// can call for itself. What it has is the service manager: systemd decrypts a
// credential -- sealed to the TPM, or to the root-only host key in
// /var/lib/systemd/credential.secret -- and drops the plaintext into a ramfs
// mounted 0700 for this unit alone, named by $CREDENTIALS_DIRECTORY.
//
// So on Linux the key is loaded, never created. That asymmetry is the point: a
// key this process could create is a key it would have to write somewhere, and
// the only place it could write it unaided is a file beside the ciphertext it
// protects. Calling that "encrypted at rest" would be a lie with a chmod on it.
// If the credential is not there, the collector refuses to start. It does not
// fall back to a plaintext key file, and there is no flag that makes it.
//
// Setting it up is three commands, and they are in the refusal message rather
// than only in a runbook, because the operator who hits this is at a terminal.
const (
	// spoolCredentialName is what the unit must call the credential. It is
	// fixed rather than configurable: a name that can be pointed elsewhere is a
	// name that can be pointed at a file somebody else can write.
	spoolCredentialName = "memgw-collector-spool-key"

	// credentialsDirEnv is set by the service manager, and only by the service
	// manager. Its presence is not proof of anything on its own -- the mode
	// check below is -- but its absence is proof that this process was not
	// started with a credential.
	credentialsDirEnv = "CREDENTIALS_DIRECTORY"
)

// LoadOrCreateKey returns the spool key handed to this unit by the service
// manager. The path argument names where the Windows build would keep its
// wrapped key; here it is used only to say what will not be written there.
func LoadOrCreateKey(path string) ([]byte, error) {
	dir := os.Getenv(credentialsDirEnv)
	if dir == "" {
		return nil, fmt.Errorf(
			"collector: %s is not set, so this process was not given a spool key by the service manager, "+
				"and no plaintext key will be written at %s. Seal one into the unit:\n"+
				"  head -c 32 /dev/urandom | xxd -p -c 64 | systemd-creds encrypt --name=%s - /etc/memgw/%s.cred\n"+
				"  # then in the unit: LoadCredentialEncrypted=%s:/etc/memgw/%s.cred",
			credentialsDirEnv, path, spoolCredentialName, spoolCredentialName,
			spoolCredentialName, spoolCredentialName)
	}
	file := filepath.Join(dir, spoolCredentialName)
	info, err := os.Stat(file)
	if err != nil {
		return nil, fmt.Errorf(
			"collector: the unit provides credentials but not %q (%v); add "+
				"LoadCredentialEncrypted=%s:<file> to the unit",
			spoolCredentialName, err, spoolCredentialName)
	}
	// systemd writes credentials 0400 into a 0700 ramfs. Anything looser did
	// not come from systemd, or came from a unit that was edited into being
	// insecure, and either way the key is readable by somebody who should not
	// have it. Refusing is the only useful answer: the key cannot be un-read.
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf(
			"collector: the spool key at %s is mode %04o; a credential readable beyond its owner is already compromised",
			file, info.Mode().Perm())
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("collector: read the spool key: %w", err)
	}
	key, err := decodeSpoolKey(raw)
	if err != nil {
		// The message never carries the bytes, only their shape.
		return nil, fmt.Errorf("collector: the spool key at %s is unusable: %w", file, err)
	}
	return key, nil
}

// decodeSpoolKey accepts the two shapes a credential file plausibly has: the
// raw key, and the hex a human piped through systemd-creds. Length tells them
// apart with no ambiguity, so there is no format flag to get wrong. Anything
// else is refused rather than hashed into the right length, because stretching
// a short secret into a 32-byte key hides how weak it was.
func decodeSpoolKey(raw []byte) ([]byte, error) {
	trimmed := strings.TrimSpace(string(raw))
	switch {
	case len(raw) == KeyLen:
		return append([]byte(nil), raw...), nil
	case len(trimmed) == hex.EncodedLen(KeyLen):
		key, err := hex.DecodeString(trimmed)
		if err != nil {
			return nil, fmt.Errorf("it is %d characters long but not hex", len(trimmed))
		}
		return key, nil
	default:
		return nil, fmt.Errorf("it holds %d bytes; want %d raw bytes or %d hex characters",
			len(raw), KeyLen, hex.EncodedLen(KeyLen))
	}
}
