package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// MaxPayloadBytes is the frozen bound on a canonical payload: one checkpoint,
// not a transcript.
const MaxPayloadBytes = 262144

// CanonicalJSON returns the canonical encoding a payload digest is taken over.
//
// Canonical means: object keys sorted, no insignificant whitespace, one
// encoding per value. It is produced by round-tripping through interface{},
// because encoding/json emits map keys in sorted order -- the same rule the
// Phase 2 fixtures are validated with, so a digest computed here and a digest
// computed by a fixture harness agree by construction rather than by luck.
func CanonicalJSON(payload any) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("memgw: payload is not encodable: %w", err)
	}
	var normalised any
	if err := json.Unmarshal(raw, &normalised); err != nil {
		return nil, fmt.Errorf("memgw: payload is not valid JSON: %w", err)
	}
	canonical, err := json.Marshal(normalised)
	if err != nil {
		return nil, fmt.Errorf("memgw: payload could not be canonicalised: %w", err)
	}
	return canonical, nil
}

// PayloadDigest is sha256 over the canonical JSON, lowercase hex.
func PayloadDigest(payload any) (string, []byte, error) {
	canonical, err := CanonicalJSON(payload)
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), canonical, nil
}
