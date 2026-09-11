package projection

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/enowdev/enowx-rag/pkg/memgw/domain"
	"github.com/enowdev/enowx-rag/pkg/memgw/outbox"
	"github.com/enowdev/enowx-rag/pkg/rag"
)

// Config bounds the applier. The defaults are the conservative ones.
type Config struct {
	// MaxSensitivity is the highest class that may be embedded. Anything above
	// it is withheld. Empty means "internal".
	MaxSensitivity string
}

func (c Config) ceiling() int {
	if r, ok := sensitivityRank[c.MaxSensitivity]; ok {
		return r
	}
	return sensitivityRank["internal"]
}

// Applier projects canonical events into a retrieval index. It satisfies
// outbox.Applier.
type Applier struct {
	events    EventSource
	deletions Deletions
	index     Index
	cfg       Config
}

// New returns an applier over the three things it needs and nothing else. It
// has no pool, no writer and no way to reach the ledger except by reading.
func New(events EventSource, deletions Deletions, index Index, cfg Config) *Applier {
	return &Applier{events: events, deletions: deletions, index: index, cfg: cfg}
}

// projected lists the event types that reach the index at all. It is a closed
// table with no default: an event type added to the contract without a decision
// here is not projected, which is the safe direction. See doc.go for why
// fact.candidate_proposed and evidence.recorded are deliberately absent.
var projected = map[string]bool{
	"checkpoint.recorded": true,
	"fact.promoted":       true,
	"fact.superseded":     true,
	"fact.retracted":      true,
	"tombstone.issued":    true,
}

// Apply implements outbox.Applier.
func (a *Applier) Apply(ctx context.Context, it outbox.Item) error {
	rec, err := a.events.Event(ctx, it.EventID)
	if err != nil {
		return err
	}
	if !projected[rec.Type] {
		// Not a failure and not a silence: the row is done because there is
		// nothing this projection is supposed to do with this event.
		return nil
	}
	if rec.Type == "tombstone.issued" {
		return a.erase(ctx, rec)
	}

	// The sensitivity gate comes before anything that could embed, and applies
	// to removals too -- harmlessly, since a withheld document was never
	// written, and a removal that ran anyway would still find nothing.
	if rank, ok := sensitivityRank[rec.Sensitivity]; !ok || rank > a.cfg.ceiling() {
		return nil
	}

	switch rec.Type {
	case "checkpoint.recorded":
		return a.projectCheckpoint(ctx, rec)
	case "fact.promoted":
		return a.projectPromotion(ctx, rec)
	default: // fact.superseded, fact.retracted
		return a.removeFact(ctx, rec)
	}
}

func (a *Applier) projectCheckpoint(ctx context.Context, rec Record) error {
	// The deletion journal is consulted before the document is built, so a
	// rebuild replaying an event older than a tombstone does not even render
	// the content it is not allowed to write.
	subjects := [][2]string{
		{"checkpoint", rec.EventID.String()},
		{"event", rec.EventID.String()},
		{"session", rec.SessionID},
	}
	if rec.WorkID != nil {
		subjects = append(subjects, [2]string{"work", rec.WorkID.String()})
	}
	if skip, err := a.anyTombstoned(ctx, subjects); err != nil {
		return err
	} else if skip {
		return outbox.ErrSkippedTombstoned
	}
	doc, err := checkpointDocument(rec)
	if err != nil {
		return err
	}
	return a.write(ctx, rec, doc)
}

func (a *Applier) projectPromotion(ctx context.Context, rec Record) error {
	var p domain.PromotionPayload
	if err := json.Unmarshal(rec.Payload, &p); err != nil {
		return failure("payload_unreadable", "the promotion payload does not parse")
	}
	subjects := [][2]string{{"event", rec.EventID.String()}}
	if p.FactID != nil {
		subjects = append(subjects, [2]string{"fact", p.FactID.String()})
	}
	if p.CandidateID != uuid.Nil {
		subjects = append(subjects, [2]string{"candidate", p.CandidateID.String()})
	}
	if skip, err := a.anyTombstoned(ctx, subjects); err != nil {
		return err
	} else if skip {
		return outbox.ErrSkippedTombstoned
	}
	doc, err := factDocument(rec, p)
	if err != nil {
		return err
	}
	if err := a.write(ctx, rec, doc); err != nil {
		return err
	}
	// A promotion that supersedes named facts removes them in the same pass.
	// For a single-valued slot the upsert above has already replaced the
	// standing value; this is what reaches the values of a multi-valued slot,
	// which live in documents of their own.
	for _, id := range p.SupersedesFacts {
		if id == uuid.Nil || (p.FactID != nil && id == *p.FactID) {
			continue
		}
		if _, err := a.index.DeleteMatching(ctx, rec.ProjectID.String(), map[string]string{"fact_id": id.String()}); err != nil {
			return failure("index_delete_failed", "removing a superseded fact from the index failed")
		}
	}
	return nil
}

// removeFact handles fact.superseded and fact.retracted.
//
// The two are the same act as far as the index is concerned: a value that used
// to stand no longer does, and leaving it searchable would let a query return
// a retracted answer with nothing to mark it as one.
func (a *Applier) removeFact(ctx context.Context, rec Record) error {
	var p domain.FactChangePayload
	if err := json.Unmarshal(rec.Payload, &p); err != nil {
		return failure("payload_unreadable", "the fact change payload does not parse")
	}
	switch {
	case p.FactID != nil && *p.FactID != uuid.Nil:
		if _, err := a.index.DeleteMatching(ctx, rec.ProjectID.String(), map[string]string{"fact_id": p.FactID.String()}); err != nil {
			return failure("index_delete_failed", "removing a fact from the index failed")
		}
		return nil
	case strings.TrimSpace(p.Subject) != "" && strings.TrimSpace(p.Predicate) != "":
		// No fact id, so the event does not say which value: the slot is the
		// only thing it identifies. Removing the whole slot over-deletes a
		// multi-valued slot, and that is the deliberate choice -- the other
		// direction leaves a retracted value in the index.
		slot := domain.SlotID(rec.ProjectID, strings.TrimSpace(p.Subject), strings.TrimSpace(p.Predicate))
		if _, err := a.index.DeleteMatching(ctx, rec.ProjectID.String(), map[string]string{"slot_id": slot.String()}); err != nil {
			return failure("index_delete_failed", "removing a fact slot from the index failed")
		}
		return nil
	default:
		// Failing loudly rather than doing nothing: a retraction that quietly
		// no-ops leaves the value searchable and nobody finds out.
		return failure("payload_unreadable", "the fact change names neither a fact nor a slot, so nothing can be removed")
	}
}

// tombstoneMetaKey maps a tombstone subject class to the payload field that
// identifies it in the index. "candidate" and "document" are present with an
// empty key on purpose: neither has documents of its own here (a candidate is
// never projected; a pre-gateway document is reached through the legacy chunk
// map), and listing them makes that a decision rather than an omission.
var tombstoneMetaKey = map[string]string{
	"checkpoint": "event_id",
	"event":      "event_id",
	"work":       "work_id",
	"session":    "session_id",
	"fact":       "fact_id",
	"candidate":  "",
	"document":   "",
}

// erase applies a tombstone: it removes what the deletion reaches, including
// points written before this gateway existed.
func (a *Applier) erase(ctx context.Context, rec Record) error {
	var p domain.TombstonePayload
	if err := json.Unmarshal(rec.Payload, &p); err != nil {
		return failure("payload_unreadable", "the tombstone payload does not parse")
	}
	key, known := tombstoneMetaKey[p.SubjectType]
	if !known {
		return failure("unknown_subject_type", "the tombstone names a subject class this projection does not know how to erase")
	}
	project := rec.ProjectID.String()
	if key != "" && strings.TrimSpace(p.SubjectID) != "" {
		if _, err := a.index.DeleteMatching(ctx, project, map[string]string{key: p.SubjectID}); err != nil {
			return failure("index_delete_failed", "erasing a tombstoned subject from the index failed")
		}
	}

	// Points written before the gateway existed have ids the deletion cannot
	// derive, so the legacy chunk map is what connects them. Both the ids
	// carried on the event and the ones the journal already knows for this
	// project are removed: the event is the order, the map is the memory of
	// what the order reaches.
	ids := append([]string(nil), p.LegacyChunkIDs...)
	mapped, err := a.deletions.TombstonedChunkIDs(ctx, rec.ProjectID)
	if err != nil {
		return failure("journal_read_failed", "the legacy chunk map could not be read")
	}
	ids = append(ids, mapped...)
	if len(ids) > 0 {
		if err := a.index.Delete(ctx, project, dedupe(ids)); err != nil {
			return failure("index_delete_failed", "erasing legacy chunks from the index failed")
		}
	}
	return nil
}

// write upserts one document, unless the index already holds a newer version of
// it. See doc.go for why this is a guard and not a transaction.
func (a *Applier) write(ctx context.Context, rec Record, doc rag.Document) error {
	for k := range doc.Meta {
		if reservedMeta[k] {
			// A metadata key that collides with a payload field the provider
			// writes would either be overwritten or would overwrite it. Either
			// way the document stops meaning what it says.
			return failure("document_invalid", "a projected document uses a reserved payload key")
		}
	}
	if err := a.index.EnsureCollection(ctx, rec.ProjectID.String()); err != nil {
		return failure("collection_failed", "the project collection could not be created")
	}
	existing, found, err := a.index.Lookup(ctx, rec.ProjectID.String(), doc.ID)
	if err != nil {
		return failure("index_read_failed", "the index could not be read before writing")
	}
	if found && newerThan(existing, rec.Seq) {
		// Replaying an older event over a newer projection would move the index
		// backwards. Skipping is correct and idempotent, which is what makes a
		// rebuild and a retry safe.
		return nil
	}
	if err := a.index.Upsert(ctx, rec.ProjectID.String(), []rag.Document{doc}); err != nil {
		return failure("index_write_failed", "the document could not be written to the index")
	}
	return nil
}

// newerThan reports whether an indexed document came from a later event than
// seq. A document with no readable seq is treated as older, so a point written
// by an earlier version of this code is replaced rather than protected.
func newerThan(pt rag.PointInfo, seq int64) bool {
	raw, ok := pt.Meta["event_seq"]
	if !ok {
		return false
	}
	have, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return false
	}
	return have > seq
}

func (a *Applier) anyTombstoned(ctx context.Context, subjects [][2]string) (bool, error) {
	for _, s := range subjects {
		if strings.TrimSpace(s[1]) == "" {
			continue
		}
		yes, err := a.deletions.IsTombstoned(ctx, s[0], s[1])
		if err != nil {
			return false, failure("journal_read_failed", "the deletion journal could not be read")
		}
		if yes {
			return true, nil
		}
	}
	return false, nil
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
