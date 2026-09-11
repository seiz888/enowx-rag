package collector

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// KeyLen is the length of the spool key. AES-256 is chosen over AES-128 for no
// better reason than that the key is protected by the operating system rather
// than by a password, so the larger key costs nothing anyone will notice.
const KeyLen = 32

// ErrKeyMismatch is returned when a ciphertext will not open under the current
// key. It is reported separately from a corrupt row because the operator
// response is different: a mismatched key means the spool belongs to another
// user or another machine, and the fix is to find the right key, never to
// delete the queue.
var ErrKeyMismatch = errors.New("collector: the spool key does not open this row")

// envelope encrypts and decrypts one payload.
//
// The additional data binds a ciphertext to the row it belongs to. Without it,
// a spool file could be edited to move an old payload onto a new event id and
// the forwarder would send it under an identity it was never written with.
// GCM catches that as an authentication failure rather than an odd log line.
type envelope struct{ aead cipher.AEAD }

func newEnvelope(key []byte) (*envelope, error) {
	if len(key) != KeyLen {
		return nil, fmt.Errorf("collector: key is %d bytes, want %d", len(key), KeyLen)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &envelope{aead: aead}, nil
}

// seal returns the nonce and the ciphertext separately so the caller can store
// them in their own columns. A nonce concatenated onto the ciphertext would
// work too; keeping them apart makes it obvious in the schema that a row
// without a nonce is unreadable and not merely empty.
func (e *envelope) seal(aad, plaintext []byte) (nonce, ciphertext []byte, err error) {
	nonce = make([]byte, e.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return nonce, e.aead.Seal(nil, nonce, plaintext, aad), nil
}

func (e *envelope) open(aad, nonce, ciphertext []byte) ([]byte, error) {
	if len(nonce) != e.aead.NonceSize() {
		return nil, ErrKeyMismatch
	}
	out, err := e.aead.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, ErrKeyMismatch
	}
	return out, nil
}

// aadFor is the associated data for one row: the two identifiers that decide
// where the payload will be sent and under what name.
func aadFor(eventID, idempotencyKey string) []byte {
	return []byte(eventID + "\x00" + idempotencyKey)
}
