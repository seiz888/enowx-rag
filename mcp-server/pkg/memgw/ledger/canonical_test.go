package ledger

import (
	"encoding/json"
	"strings"
	"testing"
)

// The digest is the contract's identity for a payload, so what matters is not
// which canonical form was chosen but that two callers who mean the same thing
// arrive at the same bytes. These tests pin that, because the day it stops
// being true is the day an honest retry is reported as a payload mismatch.

func TestCanonicalJSONOrdersKeys(t *testing.T) {
	a, _, err := PayloadDigest(map[string]any{"b": 1, "a": 2, "c": map[string]any{"z": 1, "y": 2}})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := PayloadDigest(map[string]any{"c": map[string]any{"y": 2, "z": 1}, "a": 2, "b": 1})
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("key order changed the digest: %s vs %s", a, b)
	}
}

func TestCanonicalJSONMatchesDecodedJSON(t *testing.T) {
	// A caller holding a Go struct and a caller holding raw JSON must agree.
	type payload struct {
		Objective string   `json:"objective"`
		Blockers  []string `json:"blockers"`
	}
	fromStruct, _, err := PayloadDigest(payload{Objective: "freeze the ledger contract", Blockers: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	var raw any
	if err := json.Unmarshal([]byte(`{"blockers":[],"objective":"freeze the ledger contract"}`), &raw); err != nil {
		t.Fatal(err)
	}
	fromRaw, _, err := PayloadDigest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if fromStruct != fromRaw {
		t.Fatalf("a struct and its JSON produced different digests: %s vs %s", fromStruct, fromRaw)
	}
}

func TestCanonicalJSONDistinguishesDifferentPayloads(t *testing.T) {
	a, _, _ := PayloadDigest(map[string]any{"objective": "one"})
	b, _, _ := PayloadDigest(map[string]any{"objective": "two"})
	if a == b {
		t.Fatal("different payloads must have different digests")
	}
}

func TestCanonicalJSONArrayOrderIsSignificant(t *testing.T) {
	// Arrays are ordered data, not sets. Sorting them would make two genuinely
	// different payloads look like a retry of each other.
	a, _, _ := PayloadDigest([]any{"a", "b"})
	b, _, _ := PayloadDigest([]any{"b", "a"})
	if a == b {
		t.Fatal("array order must affect the digest")
	}
}

func TestDigestIsLowercaseHex(t *testing.T) {
	d, _, err := PayloadDigest(map[string]any{"x": 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(d) != 64 || strings.ToLower(d) != d {
		t.Fatalf("digest %q is not 64 lowercase hex characters", d)
	}
}

func TestCanonicalJSONRefusesUnencodable(t *testing.T) {
	if _, _, err := PayloadDigest(map[string]any{"f": func() {}}); err == nil {
		t.Fatal("a payload that cannot be encoded must fail rather than digest to something")
	}
}

func TestErrorClassHelpers(t *testing.T) {
	err := classErr(ClassCASConflict, "another writer committed this revision first")
	if !IsClass(err, ClassCASConflict) {
		t.Fatalf("IsClass failed for %v", err)
	}
	if ClassOf(nil) != "" {
		t.Fatal("a nil error has no class")
	}
	if got := ClassOf(err); got != ClassCASConflict {
		t.Fatalf("ClassOf = %q", got)
	}
}
