package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The fixtures are the contract in machine-readable form. These tests are what
// stops the contract from drifting into whatever an implementation happens to
// do: a fixture that names an unknown event type, an outcome that claims an
// effect it is not allowed to have, or an invariant nobody covers, fails here
// before any ledger code exists to argue with.

type contractDoc struct {
	SchemaVersion      string            `json:"schema_version"`
	PolicyVersion      string            `json:"policy_version"`
	EnvelopeRequired   []string          `json:"envelope_required"`
	EnvelopeOptional   []string          `json:"envelope_optional"`
	ServerAssigned     []string          `json:"server_assigned"`
	EventTypes         []string          `json:"event_types"`
	ErrorClasses       []string          `json:"error_classes"`
	Outcomes           []string          `json:"outcomes"`
	ReceiptStates      []string          `json:"receipt_states"`
	Roles              []string          `json:"roles"`
	PrincipalTypes     []string          `json:"principal_types"`
	SensitivityClasses []string          `json:"sensitivity_classes"`
	Bounds             map[string]int    `json:"bounds"`
	Invariants         map[string]string `json:"invariants"`
	Fixtures           []string          `json:"fixtures"`
}

type fixtureDoc struct {
	Fixture    string          `json:"fixture"`
	Title      string          `json:"title"`
	Invariants []string        `json:"invariants"`
	Narrative  string          `json:"narrative"`
	Principals []principalDoc  `json:"principals"`
	Steps      []stepDoc       `json:"steps"`
	Relation   *digestRelation `json:"digest_relation"`
	Assertions []string        `json:"assertions"`
}

type principalDoc struct {
	PrincipalID   string   `json:"principal_id"`
	PrincipalType string   `json:"principal_type"`
	Roles         []string `json:"roles"`
}

type stepDoc struct {
	Name      string                 `json:"name"`
	Operation string                 `json:"operation"`
	Submit    map[string]interface{} `json:"submit"`
	Expect    map[string]interface{} `json:"expect"`
}

type digestRelation struct {
	Equal     [][]string `json:"equal"`
	Different [][]string `json:"different"`
}

const testdataDir = "testdata"

// computedDigest is the sentinel a fixture writes instead of a literal hash.
// Hard-coding hashes would make every fixture edit a hand-computation exercise
// and would not test anything the test cannot compute itself; what matters is
// the relation between digests, which is what the contract keys off.
const computedDigest = "@computed"

// effects a step may claim. Anything else is a fixture that has invented a
// semantic the contract does not define.
var validEffects = map[string]bool{
	"none":           true,
	"state_advanced": true,
	"quarantined":    true,
}

func loadContract(t *testing.T) contractDoc {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(testdataDir, "contract.json"))
	if err != nil {
		t.Fatalf("read contract: %v", err)
	}
	var c contractDoc
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("parse contract: %v", err)
	}
	return c
}

func loadFixtures(t *testing.T) map[string]fixtureDoc {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(testdataDir, "fixtures", "*.json"))
	if err != nil {
		t.Fatalf("glob fixtures: %v", err)
	}
	out := make(map[string]fixtureDoc, len(paths))
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		var f fixtureDoc
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		base := strings.TrimSuffix(filepath.Base(p), ".json")
		if f.Fixture != base {
			t.Errorf("%s: fixture name %q does not match filename", p, f.Fixture)
		}
		out[base] = f
	}
	return out
}

func set(values []string) map[string]bool {
	m := make(map[string]bool, len(values))
	for _, v := range values {
		m[v] = true
	}
	return m
}

// canonicalDigest is the digest rule the contract names: sha256 over canonical
// JSON, lowercase hex. Round-tripping through interface{} gives canonical form
// because encoding/json emits map keys in sorted order.
func canonicalDigest(t *testing.T, payload interface{}) string {
	t.Helper()
	buf, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var normalised interface{}
	if err := json.Unmarshal(buf, &normalised); err != nil {
		t.Fatalf("normalise payload: %v", err)
	}
	canonical, err := json.Marshal(normalised)
	if err != nil {
		t.Fatalf("marshal canonical payload: %v", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

func TestEveryContractFixtureExists(t *testing.T) {
	c := loadContract(t)
	fixtures := loadFixtures(t)

	for _, name := range c.Fixtures {
		if _, ok := fixtures[name]; !ok {
			t.Errorf("contract lists fixture %q but no such file exists", name)
		}
	}
	listed := set(c.Fixtures)
	for name := range fixtures {
		if !listed[name] {
			t.Errorf("fixture %q exists but is not listed in the contract", name)
		}
	}
}

func TestEveryInvariantIsCovered(t *testing.T) {
	c := loadContract(t)
	fixtures := loadFixtures(t)

	covered := map[string]bool{}
	for name, f := range fixtures {
		if len(f.Invariants) == 0 {
			t.Errorf("%s: fixture covers no invariant", name)
		}
		for _, inv := range f.Invariants {
			if _, ok := c.Invariants[inv]; !ok {
				t.Errorf("%s: references unknown invariant %q", name, inv)
			}
			covered[inv] = true
		}
	}
	for inv := range c.Invariants {
		if !covered[inv] {
			t.Errorf("%s is stated in the contract but no fixture exercises it", inv)
		}
	}
}

func TestEnvelopesAreComplete(t *testing.T) {
	c := loadContract(t)
	fixtures := loadFixtures(t)

	required := c.EnvelopeRequired
	known := set(append(append(append([]string{}, c.EnvelopeRequired...), c.EnvelopeOptional...), c.ServerAssigned...))
	eventTypes := set(c.EventTypes)
	sensitivity := set(c.SensitivityClasses)

	for name, f := range fixtures {
		for _, step := range f.Steps {
			if step.Submit == nil {
				continue
			}
			for _, field := range required {
				if _, ok := step.Submit[field]; !ok {
					t.Errorf("%s/%s: envelope is missing required field %q", name, step.Name, field)
				}
			}
			for field := range step.Submit {
				if !known[field] {
					t.Errorf("%s/%s: envelope carries unknown field %q", name, step.Name, field)
				}
			}
			if et, _ := step.Submit["event_type"].(string); !eventTypes[et] {
				t.Errorf("%s/%s: unknown event_type %q", name, step.Name, et)
			}
			if sc, _ := step.Submit["sensitivity_class"].(string); !sensitivity[sc] {
				t.Errorf("%s/%s: unknown sensitivity_class %q", name, step.Name, sc)
			}
			if v, _ := step.Submit["schema_version"].(string); v != c.SchemaVersion {
				t.Errorf("%s/%s: schema_version %q, want %q", name, step.Name, v, c.SchemaVersion)
			}
			if v, _ := step.Submit["policy_version"].(string); v != c.PolicyVersion {
				t.Errorf("%s/%s: policy_version %q, want %q", name, step.Name, v, c.PolicyVersion)
			}
			if d, _ := step.Submit["payload_digest"].(string); d != computedDigest {
				t.Errorf("%s/%s: payload_digest is %q; fixtures declare %q and let the harness compute it", name, step.Name, d, computedDigest)
			}
		}
	}
}

func TestEnvelopesRespectBounds(t *testing.T) {
	c := loadContract(t)
	fixtures := loadFixtures(t)

	for name, f := range fixtures {
		for _, step := range f.Steps {
			if step.Submit == nil {
				continue
			}
			if key, _ := step.Submit["idempotency_key"].(string); len(key) > c.Bounds["idempotency_key_bytes_max"] {
				t.Errorf("%s/%s: idempotency_key is %d bytes, max %d", name, step.Name, len(key), c.Bounds["idempotency_key_bytes_max"])
			}
			if sid, _ := step.Submit["session_id"].(string); len(sid) > c.Bounds["session_id_bytes_max"] {
				t.Errorf("%s/%s: session_id is %d bytes, max %d", name, step.Name, len(sid), c.Bounds["session_id_bytes_max"])
			}
			if refs, ok := step.Submit["evidence_refs"].([]interface{}); ok && len(refs) > c.Bounds["evidence_refs_max"] {
				t.Errorf("%s/%s: %d evidence refs, max %d", name, step.Name, len(refs), c.Bounds["evidence_refs_max"])
			}
			payload, err := json.Marshal(step.Submit["payload"])
			if err != nil {
				t.Fatalf("%s/%s: marshal payload: %v", name, step.Name, err)
			}
			if len(payload) > c.Bounds["payload_bytes_max"] {
				t.Errorf("%s/%s: payload is %d bytes, max %d", name, step.Name, len(payload), c.Bounds["payload_bytes_max"])
			}
		}
	}
}

func TestExpectationsUseContractVocabulary(t *testing.T) {
	c := loadContract(t)
	fixtures := loadFixtures(t)

	outcomes := set(c.Outcomes)
	errorClasses := set(c.ErrorClasses)
	receiptStates := set(c.ReceiptStates)

	for name, f := range fixtures {
		for _, step := range f.Steps {
			if step.Expect == nil {
				t.Errorf("%s/%s: step states no expectation", name, step.Name)
				continue
			}
			outcome, _ := step.Expect["outcome"].(string)
			if !outcomes[outcome] {
				t.Errorf("%s/%s: unknown outcome %q", name, step.Name, outcome)
			}
			if ec, ok := step.Expect["error_class"].(string); ok && !errorClasses[ec] {
				t.Errorf("%s/%s: unknown error_class %q", name, step.Name, ec)
			}
			if rs, ok := step.Expect["receipt_state"].(string); ok && !receiptStates[rs] {
				t.Errorf("%s/%s: unknown receipt_state %q", name, step.Name, rs)
			}
			if eff, ok := step.Expect["effect"].(string); ok && !validEffects[eff] {
				t.Errorf("%s/%s: unknown effect %q", name, step.Name, eff)
			}
		}
	}
}

// TestRejectionsHaveNoEffect is the shape of INV-02, INV-03 and INV-07 stated
// once over every fixture: an outcome that is not a commit must not claim to
// have moved anything, and a duplicate must not move state a second time.
func TestRejectionsHaveNoEffect(t *testing.T) {
	fixtures := loadFixtures(t)

	for name, f := range fixtures {
		for _, step := range f.Steps {
			if step.Expect == nil {
				continue
			}
			outcome, _ := step.Expect["outcome"].(string)
			effect, _ := step.Expect["effect"].(string)
			switch outcome {
			case "rejected", "duplicate":
				if effect != "none" {
					t.Errorf("%s/%s: outcome %q claims effect %q; it must be none", name, step.Name, outcome, effect)
				}
				if outcome == "rejected" {
					if _, ok := step.Expect["error_class"]; !ok {
						t.Errorf("%s/%s: a rejection must name an error class", name, step.Name)
					}
				}
			case "quarantined":
				if effect != "quarantined" {
					t.Errorf("%s/%s: a quarantined write must claim effect quarantined, got %q", name, step.Name, effect)
				}
			}
		}
	}
}

// TestDigestRelationsHold checks the property duplicate detection rests on:
// identical payloads must digest identically, and the mismatch fixture must
// really carry a different payload rather than only claiming to.
func TestDigestRelationsHold(t *testing.T) {
	fixtures := loadFixtures(t)

	for name, f := range fixtures {
		if f.Relation == nil {
			continue
		}
		digests := map[string]string{}
		for _, step := range f.Steps {
			if step.Submit == nil {
				continue
			}
			digests[step.Name] = canonicalDigest(t, step.Submit["payload"])
		}
		for _, pair := range f.Relation.Equal {
			if len(pair) != 2 {
				t.Fatalf("%s: digest relation must name two steps, got %v", name, pair)
			}
			if digests[pair[0]] != digests[pair[1]] {
				t.Errorf("%s: steps %s and %s must have identical payload digests", name, pair[0], pair[1])
			}
		}
		for _, pair := range f.Relation.Different {
			if len(pair) != 2 {
				t.Fatalf("%s: digest relation must name two steps, got %v", name, pair)
			}
			if digests[pair[0]] == digests[pair[1]] {
				t.Errorf("%s: steps %s and %s must have different payload digests", name, pair[0], pair[1])
			}
		}
	}
}

// TestIdempotentRetryKeepsEventID is INV-01 stated over the fixtures that model
// a retry: the same idempotency key, submitted twice by the same principal,
// must carry the same event_id. A fixture that regenerated it would be modelling
// a defect as if it were correct behaviour.
func TestIdempotentRetryKeepsEventID(t *testing.T) {
	fixtures := loadFixtures(t)

	for name, f := range fixtures {
		type submission struct{ eventID, digest, step string }
		byKey := map[string][]submission{}
		for _, step := range f.Steps {
			if step.Submit == nil {
				continue
			}
			key, _ := step.Submit["idempotency_key"].(string)
			id, _ := step.Submit["event_id"].(string)
			byKey[key] = append(byKey[key], submission{id, canonicalDigest(t, step.Submit["payload"]), step.Name})
		}
		for key, subs := range byKey {
			if len(subs) < 2 {
				continue
			}
			first := subs[0]
			for _, s := range subs[1:] {
				sameDigest := s.digest == first.digest
				sameID := s.eventID == first.eventID
				if sameDigest && !sameID {
					t.Errorf("%s: key %q resubmits the same payload under a different event_id (%s vs %s)", name, key, first.step, s.step)
				}
				if !sameDigest && sameID {
					t.Errorf("%s: key %q reuses event_id %s for a different payload (%s vs %s)", name, key, s.eventID, first.step, s.step)
				}
			}
		}
	}
}

func TestPrincipalsUseContractVocabulary(t *testing.T) {
	c := loadContract(t)
	fixtures := loadFixtures(t)

	types := set(c.PrincipalTypes)
	roles := set(c.Roles)

	for name, f := range fixtures {
		for _, p := range f.Principals {
			if !types[p.PrincipalType] {
				t.Errorf("%s: unknown principal_type %q", name, p.PrincipalType)
			}
			for _, r := range p.Roles {
				if !roles[r] {
					t.Errorf("%s: unknown role %q", name, r)
				}
			}
		}
	}
}

// credentialShapes mirrors the live write guard's intent. A fixture about
// leaked secrets that itself carried a plausible secret would be the leak it
// tests for, so the fixtures name shapes instead of reproducing them and this
// test holds them to it.
var credentialShapes = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.`),
	regexp.MustCompile(`\b(gh[pousr]|github_pat)_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`(?i)\b(password|api[_-]?key|secret|token)\s*[:=]\s*["']?[A-Za-z0-9/+_-]{16,}`),
}

func TestFixturesCarryNoCredentialShapedContent(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(testdataDir, "fixtures", "*.json"))
	if err != nil {
		t.Fatalf("glob fixtures: %v", err)
	}
	paths = append(paths, filepath.Join(testdataDir, "contract.json"))

	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		for _, re := range credentialShapes {
			if re.Match(raw) {
				// The pattern is named, never the match, for the same reason
				// the guard never echoes a rejected payload.
				t.Errorf("%s: contains content matching a credential shape (%s)", filepath.Base(p), re.String())
			}
		}
	}
}
