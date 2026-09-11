package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/google/uuid"
)

// Decision is what happens to one source record. The set is closed and every
// record gets exactly one.
type Decision string

const (
	// Imported: the record will produce a legacy chunk mapping.
	Imported Decision = "imported"
	// Quarantined: the record is held back for a human. It is not lost and it
	// is not written; it is listed, with the reason, and somebody decides.
	Quarantined Decision = "quarantined"
	// Rejected: the record cannot be understood well enough to act on.
	Rejected Decision = "rejected"
	// Skipped: the record is already accounted for.
	Skipped Decision = "skipped"
)

// Reasons are a closed vocabulary. Free text here would be a column in a report
// that nobody can count, and counting is the entire purpose of a dry run.
const (
	ReasonOK                = "record is complete and not yet mapped"
	ReasonNoChunkID         = "the record has no chunk id"
	ReasonNoProject         = "the record names no rag project"
	ReasonNoDocument        = "the record has neither a document id nor content to derive one from"
	ReasonBadDigest         = "the record's source digest is not a sha256"
	ReasonTooLarge          = "the record is larger than the contract's payload bound"
	ReasonSecretSuspected   = "the record matched a credential pattern and must be reviewed before anything is written"
	ReasonUnknownSensitive  = "the record claims a sensitivity class that is not in the contract"
	ReasonAlreadyMapped     = "the chunk is already mapped to this document with this digest"
	ReasonRemappedDigest    = "the chunk is mapped to a different document or digest and must be reviewed"
	ReasonDuplicateInSource = "the source handed the same chunk id over more than once"
	ReasonFactShaped        = "the record looks like a durable claim; a migration may not promote it, so it is listed as a candidate for review"
)

// Entry is one record's line in the plan.
//
// There is no text field, and there will not be one. See the package comment.
type Entry struct {
	ChunkID      string    `json:"chunk_id"`
	RAGProject   string    `json:"rag_project"`
	DocumentID   string    `json:"document_id"`
	ProjectID    uuid.UUID `json:"project_id"`
	SourceDigest string    `json:"source_digest"`
	Bytes        int64     `json:"bytes"`
	Decision     Decision  `json:"decision"`
	Reason       string    `json:"reason"`
	// Candidate marks a record a reviewer should look at as a possible fact.
	// It is a flag on a plan line, never an action: nothing downstream reads it
	// and writes anything.
	Candidate bool `json:"candidate,omitempty"`
}

// Plan is the whole dry run.
type Plan struct {
	// PlanVersion changes when the shape of a plan changes, so a plan produced
	// by one build and applied by another is detected rather than misread.
	PlanVersion string `json:"plan_version"`
	Source      string `json:"source"`
	// Namespace is the uuid namespace every derived project id came from. It is
	// recorded because a plan whose identifiers cannot be re-derived is a plan
	// that cannot be checked.
	Namespace uuid.UUID `json:"namespace"`
	// ProjectID is the ledger project every entry is filed under, and
	// ProjectIDSource says whether an operator declared it or it was derived.
	// A reader has to be able to tell those apart without re-running anything.
	ProjectID       uuid.UUID `json:"project_id"`
	ProjectIDSource string    `json:"project_id_source"`

	Counts map[Decision]int64 `json:"counts"`
	Total  int64              `json:"total"`
	// Digest covers every entry in order. Two runs over one source produce one
	// digest; that is the property the rehearsal exists to demonstrate.
	Digest  string  `json:"digest"`
	Entries []Entry `json:"entries"`

	// GeneratedAt is outside the digest on purpose: a plan that changed because
	// time passed would make the identical-runs check meaningless.
	GeneratedAt time.Time `json:"generated_at"`
}

// PlanVersion is the current shape.
const PlanVersion = "memgw-migrate-plan/1"

// finish sorts, counts and digests. Sorting by chunk id is what makes the
// output independent of the order the source handed records over -- a Qdrant
// scroll and a file export of the same corpus must produce the same plan.
func (p *Plan) finish() {
	sort.Slice(p.Entries, func(i, j int) bool { return p.Entries[i].ChunkID < p.Entries[j].ChunkID })
	p.Counts = map[Decision]int64{Imported: 0, Quarantined: 0, Rejected: 0, Skipped: 0}
	h := sha256.New()
	for _, e := range p.Entries {
		p.Counts[e.Decision]++
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%s\x00%d\x00%s\x00%s\x00%t\x00",
			e.ChunkID, e.RAGProject, e.DocumentID, e.ProjectID, e.SourceDigest,
			e.Bytes, e.Decision, e.Reason, e.Candidate)
	}
	p.Total = int64(len(p.Entries))
	p.Digest = hex.EncodeToString(h.Sum(nil))
	p.PlanVersion = PlanVersion
}

// Balanced reports whether every record was accounted for. It is called by the
// planner and by the tests, because "every record gets a decision" is the kind
// of property that holds until the day somebody adds a fifth branch.
func (p *Plan) Balanced() bool {
	var sum int64
	for _, n := range p.Counts {
		sum += n
	}
	return sum == p.Total && p.Total == int64(len(p.Entries))
}

// Write saves the plan.
func (p *Plan) Write(path string) error {
	body, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return os.WriteFile(path, body, 0o600)
}

// ReadPlan loads a plan and re-derives its digest, refusing one that has been
// edited. A plan is a document people pass around; an edited plan that still
// claimed its original digest would be worse than no plan at all.
func ReadPlan(path string) (Plan, error) {
	var p Plan
	body, err := os.ReadFile(path)
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return p, fmt.Errorf("migrate: %s is not a plan: %w", path, err)
	}
	claimed := p.Digest
	p.finish()
	if p.Digest != claimed {
		return Plan{}, fmt.Errorf("migrate: the plan's digest does not match its entries; it has been edited since it was written")
	}
	return p, nil
}
