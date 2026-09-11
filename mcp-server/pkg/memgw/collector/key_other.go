//go:build !windows && !linux

package collector

import "errors"

// The spool key needs a key store the operating system holds, not a file the
// process can print. Windows has DPAPI; Linux has systemd credentials. This
// build has neither implemented.
//
// Rather than fall back to an unprotected key file and call the result
// "encrypted", the key store refuses. A refusal is honest; a key sitting next
// to the ciphertext it protects would not be.
func LoadOrCreateKey(string) ([]byte, error) {
	return nil, errors.New("collector: the spool key requires an OS-protected key store, which is implemented for Windows and Linux only")
}
