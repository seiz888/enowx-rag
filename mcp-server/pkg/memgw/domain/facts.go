package domain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/enowdev/enowx-rag/pkg/memgw/errclass"
)

// A candidate is a proposal and nothing else. It is not readable as current
// state, it does not answer a current-state query, and it cannot authorise its
// own promotion: the promotion is a separate event by a principal holding
// fact_promote, naming the candidate and at least one piece of evidence.
//
// That separation is the whole of INV-10, and it is why extracted text -- which
// may itself be an instruction addressed at the agent -- can never become a
// fact by being convincing.

func prepareCandidate(ctx context.Context, tx DB, ev Event) error {
	var p CandidatePayload
	if err := decode(ev.Payload, &p); err != nil {
		return err
	}
	switch {
	case p.CandidateID == uuid.Nil:
		return errclass.New(errclass.PolicyRejected, "a candidate must carry a candidate_id")
	case p.Subject == "" || p.Predicate == "":
		return errclass.New(errclass.PolicyRejected, "a candidate must name a subject and a predicate")
	case len(p.Object) == 0 || string(p.Object) == "null":
		return errclass.New(errclass.PolicyRejected, "a candidate must carry an object")
	case p.Cardinality != "single" && p.Cardinality != "multi":
		return errclass.New(errclass.PolicyRejected, "a candidate must declare cardinality single or multi")
	case p.Source == "" || p.Extractor == "":
		// model and model_version may be absent -- a deterministic extractor
		// has neither -- but a claim with no traceable origin is not reviewable.
		return errclass.New(errclass.PolicyRejected, "a candidate must name its source and its extractor")
	}
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM fact_candidates WHERE candidate_id = $1)`, p.CandidateID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return errclass.New(errclass.PolicyRejected, "this candidate id has already been proposed")
	}
	return nil
}

func applyCandidate(ctx context.Context, tx DB, ev Event, seq int64) error {
	var p CandidatePayload
	if err := decode(ev.Payload, &p); err != nil {
		return err
	}
	digest, canonical, err := canonicalDigest(p.Object)
	if err != nil {
		return err
	}
	class := p.EvidenceClass
	if class == "" {
		class = "derived"
	}
	if !validEvidenceClass[class] {
		return errclass.New(errclass.PolicyRejected, "a candidate's evidence_class is not a known class")
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO fact_candidates (candidate_id, project_id, workspace_id, subject, predicate, object,
		                             object_digest, cardinality, source, extractor, model, model_version,
		                             confidence, evidence_class, review_status,
		                             proposed_by_principal_id, proposed_event_id, proposed_at, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,'pending',$15,$16,$17,$18)`,
		p.CandidateID, ev.ProjectID, ev.WorkspaceID, p.Subject, p.Predicate, canonical,
		digest, p.Cardinality, p.Source, p.Extractor, p.Model, p.ModelVersion,
		p.Confidence, class, ev.PrincipalID, ev.EventID, ev.OccurredAt, p.ExpiresAt); err != nil {
		return err
	}
	// The extraction's own source is provenance in its own right, separate from
	// whatever evidence a later promotion cites.
	_, err = tx.Exec(ctx, `
		INSERT INTO provenance_refs (provenance_id, project_id, candidate_id, source_type, source_ref, evidence_class)
		VALUES ($1,$2,$3,'document',$4,$5)`,
		uuid.New(), ev.ProjectID, p.CandidateID, truncate(p.Source, 400), class)
	_ = seq
	return err
}

// candidateFor loads the candidate a promotion names and checks it may be
// promoted at all.
func candidateFor(ctx context.Context, tx DB, ev Event, p PromotionPayload) (candidateRow, error) {
	var c candidateRow
	err := tx.QueryRow(ctx, `
		SELECT candidate_id, project_id, subject, predicate, object, object_digest, cardinality,
		       review_status, evidence_class, expires_at
		FROM fact_candidates WHERE candidate_id = $1`, p.CandidateID,
	).Scan(&c.id, &c.projectID, &c.subject, &c.predicate, &c.object, &c.objectDigest, &c.cardinality,
		&c.reviewStatus, &c.evidenceClass, &c.expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, errclass.New(errclass.PolicyRejected, "the candidate this promotion names does not exist")
	}
	if err != nil {
		return c, err
	}
	if c.projectID != ev.ProjectID {
		return c, errclass.New(errclass.ScopeDenied, "the candidate belongs to a different project")
	}
	if c.reviewStatus != "pending" {
		return c, errclass.New(errclass.PolicyRejected, "the candidate is %s and cannot be promoted again", c.reviewStatus)
	}
	if c.expiresAt != nil && !c.expiresAt.After(ev.OccurredAt) {
		return c, errclass.New(errclass.PolicyRejected, "the candidate expired before this promotion")
	}
	// A restatement is allowed for readability but must agree. A promotion that
	// could redefine what it promotes would let a reviewer approve one claim
	// and a writer commit another.
	if p.Subject != "" && p.Subject != c.subject {
		return c, errclass.New(errclass.PolicyRejected, "the promotion restates a different subject than the candidate")
	}
	if p.Predicate != "" && p.Predicate != c.predicate {
		return c, errclass.New(errclass.PolicyRejected, "the promotion restates a different predicate than the candidate")
	}
	if p.Cardinality != "" && p.Cardinality != c.cardinality {
		return c, errclass.New(errclass.PolicyRejected, "the promotion restates a different cardinality than the candidate")
	}
	if len(p.Object) > 0 && string(p.Object) != "null" {
		digest, _, err := canonicalDigest(p.Object)
		if err != nil {
			return c, err
		}
		if digest != c.objectDigest {
			return c, errclass.New(errclass.PolicyRejected, "the promotion restates a different object than the candidate")
		}
	}
	return c, nil
}

type candidateRow struct {
	id            uuid.UUID
	projectID     uuid.UUID
	subject       string
	predicate     string
	object        []byte
	objectDigest  string
	cardinality   string
	reviewStatus  string
	evidenceClass string
	expiresAt     *time.Time
}

func preparePromotion(ctx context.Context, tx DB, ev Event, cas CAS) (Aggregate, *int64, error) {
	var p PromotionPayload
	if err := decode(ev.Payload, &p); err != nil {
		return Aggregate{}, nil, err
	}
	if p.CandidateID == uuid.Nil {
		return Aggregate{}, nil, errclass.New(errclass.PolicyRejected,
			"a promotion must name the candidate it promotes; a fact with no reviewed proposal behind it is an assertion")
	}
	if len(ev.EvidenceRefs) == 0 {
		return Aggregate{}, nil, errclass.New(errclass.PolicyRejected,
			"a promotion must cite at least one evidence reference")
	}
	c, err := candidateFor(ctx, tx, ev, p)
	if err != nil {
		return Aggregate{}, nil, err
	}

	slotID := SlotID(ev.ProjectID, c.subject, c.predicate)
	if err := ensureSlot(ctx, tx, ev.ProjectID, slotID, c.subject, c.predicate, c.cardinality); err != nil {
		return Aggregate{}, nil, err
	}

	// Only a single-valued predicate is contended: two writers promoting
	// different values into it are deciding the same thing. Multi-valued
	// promotions are additive and never supersede -- INV-11, which is why they
	// carry no aggregate and never produce a cas_conflict.
	agg := Aggregate{}
	if c.cardinality == "single" {
		agg = Aggregate{Type: AggregateFactSlot, ID: slotID, Mutating: true}
	}
	revision, err := cas(ctx, agg)
	if err != nil {
		return Aggregate{}, nil, err
	}

	// The same value promoted twice into a live slot is not a second fact.
	var duplicate bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM facts WHERE slot_id = $1 AND object_digest = $2 AND status <> 'retracted')`,
		slotID, c.objectDigest).Scan(&duplicate); err != nil {
		return Aggregate{}, nil, err
	}
	if duplicate {
		return Aggregate{}, nil, errclass.New(errclass.PolicyRejected,
			"this value already stands for this predicate")
	}
	return agg, revision, nil
}

func ensureSlot(ctx context.Context, tx DB, projectID, slotID uuid.UUID, subject, predicate, cardinality string) error {
	var existing string
	err := tx.QueryRow(ctx, `SELECT cardinality FROM fact_slots WHERE slot_id = $1`, slotID).Scan(&existing)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		_, err := tx.Exec(ctx, `
			INSERT INTO fact_slots (slot_id, project_id, subject, predicate, cardinality)
			VALUES ($1,$2,$3,$4,$5)`, slotID, projectID, subject, predicate, cardinality)
		return err
	case err != nil:
		return err
	}
	if existing != cardinality {
		// Changing a predicate from multi to single would retroactively make
		// coexisting values into a conflict nobody declared.
		return errclass.New(errclass.PolicyRejected,
			"this predicate is already %s-valued and its cardinality cannot be changed by a promotion", existing)
	}
	return nil
}

func applyPromotion(ctx context.Context, tx DB, ev Event, seq int64, revision *int64) error {
	var p PromotionPayload
	if err := decode(ev.Payload, &p); err != nil {
		return err
	}
	c, err := candidateFor(ctx, tx, ev, p)
	if err != nil {
		return err
	}
	slotID := SlotID(ev.ProjectID, c.subject, c.predicate)

	factID := uuid.New()
	if p.FactID != nil {
		factID = *p.FactID
	}
	validFrom := nowOr(p.ValidFrom, ev.OccurredAt)
	class := p.EvidenceClass
	if class == "" {
		class = c.evidenceClass
	}

	factRevision := int64(1)
	status := "active"

	if c.cardinality == "single" {
		if revision == nil {
			return errclass.New(errclass.PolicyRejected, "a single-valued promotion is compare-and-set and must carry a revision")
		}
		factRevision = *revision

		// Everything still standing for this predicate with a different value.
		rows, err := tx.Query(ctx,
			`SELECT fact_id FROM facts WHERE slot_id = $1 AND status IN ('active','conflicted') AND object_digest <> $2`,
			slotID, c.objectDigest)
		if err != nil {
			return err
		}
		var standing []uuid.UUID
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			standing = append(standing, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		named := map[uuid.UUID]bool{}
		for _, id := range p.SupersedesFacts {
			named[id] = true
		}
		var unresolved []uuid.UUID
		for _, id := range standing {
			if !named[id] {
				unresolved = append(unresolved, id)
			}
		}
		if len(unresolved) > 0 {
			// INV-08. The new value is preserved and both sides stay readable
			// with their evidence. Nothing is chosen by recency, and no model
			// is asked to reconcile them: resolution is an explicit
			// fact.superseded or fact.retracted by a principal holding
			// fact_promote.
			status = "conflicted"
		}
	} else {
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE(MAX(revision), 0) + 1 FROM facts WHERE slot_id = $1`, slotID).Scan(&factRevision); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO facts (fact_id, slot_id, project_id, workspace_id, subject, predicate, object, object_digest,
		                   cardinality, status, evidence_class, sensitivity_class, valid_from, recorded_at,
		                   promoted_by_principal_id, promoted_event_id, candidate_id, revision)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
		factID, slotID, ev.ProjectID, ev.WorkspaceID, c.subject, c.predicate, c.object, c.objectDigest,
		c.cardinality, status, class, ev.SensitivityClass, validFrom, ev.OccurredAt,
		ev.PrincipalID, ev.EventID, c.id, factRevision); err != nil {
		return err
	}

	if c.cardinality == "single" {
		if status == "conflicted" {
			if _, err := tx.Exec(ctx,
				`UPDATE facts SET status = 'conflicted' WHERE slot_id = $1 AND status = 'active' AND fact_id <> $2`,
				slotID, factID); err != nil {
				return err
			}
		}
		for _, id := range p.SupersedesFacts {
			tag, err := tx.Exec(ctx, `
				UPDATE facts SET status = 'superseded', superseded_by = $2, valid_to = $3
				WHERE fact_id = $1 AND slot_id = $4 AND status IN ('active','conflicted')`,
				id, factID, validFrom, slotID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return errclass.New(errclass.PolicyRejected,
					"a fact named as superseded is not a live value of this predicate")
			}
		}
		if err := setSlot(ctx, tx, slotID, *revision, status); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE fact_candidates
		SET review_status = 'promoted', promoted_fact_id = $2, promoted_event_id = $3, reviewed_at = $4
		WHERE candidate_id = $1`, c.id, factID, ev.EventID, ev.OccurredAt); err != nil {
		return err
	}

	// Each cited evidence event becomes provenance on the fact, so a reader can
	// see what the promotion rested on without reading the event log.
	for _, ref := range ev.EvidenceRefs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO provenance_refs (provenance_id, project_id, fact_id, source_type, source_ref, evidence_class)
			VALUES ($1,$2,$3,'event',$4,$5)`,
			uuid.New(), ev.ProjectID, factID, ref.String(), class); err != nil {
			return err
		}
	}
	_ = seq
	return nil
}

func prepareFactChange(ctx context.Context, tx DB, ev Event, cas CAS) (Aggregate, *int64, error) {
	var p FactChangePayload
	if err := decode(ev.Payload, &p); err != nil {
		return Aggregate{}, nil, err
	}

	if ev.Type == "fact.conflict_flagged" {
		if p.Subject == "" || p.Predicate == "" {
			return Aggregate{}, nil, errclass.New(errclass.PolicyRejected,
				"flagging a conflict must name the subject and predicate it is about")
		}
		slotID := SlotID(ev.ProjectID, p.Subject, p.Predicate)
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM fact_slots WHERE slot_id = $1)`, slotID).Scan(&exists); err != nil {
			return Aggregate{}, nil, err
		}
		if !exists {
			return Aggregate{}, nil, errclass.New(errclass.PolicyRejected, "no such predicate has been promoted in this project")
		}
		return withCAS(ctx, cas, Aggregate{Type: AggregateFactSlot, ID: slotID, Mutating: true})
	}

	if p.FactID == nil {
		return Aggregate{}, nil, errclass.New(errclass.PolicyRejected, "%s must name the fact it changes", ev.Type)
	}
	var slotID, projectID uuid.UUID
	var status string
	err := tx.QueryRow(ctx, `SELECT slot_id, project_id, status FROM facts WHERE fact_id = $1`, *p.FactID).
		Scan(&slotID, &projectID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Aggregate{}, nil, errclass.New(errclass.PolicyRejected, "the fact this event names does not exist")
	}
	if err != nil {
		return Aggregate{}, nil, err
	}
	if projectID != ev.ProjectID {
		return Aggregate{}, nil, errclass.New(errclass.ScopeDenied, "the fact belongs to a different project")
	}
	if status == "retracted" || status == "superseded" {
		return Aggregate{}, nil, errclass.New(errclass.PolicyRejected, "the fact is already %s", status)
	}
	if ev.Type == "fact.superseded" {
		if p.SupersededBy == nil {
			return Aggregate{}, nil, errclass.New(errclass.PolicyRejected,
				"superseding a fact must name the value that replaces it")
		}
		var successorSlot uuid.UUID
		var successorStatus string
		err := tx.QueryRow(ctx, `SELECT slot_id, status FROM facts WHERE fact_id = $1`, *p.SupersededBy).
			Scan(&successorSlot, &successorStatus)
		if errors.Is(err, pgx.ErrNoRows) {
			return Aggregate{}, nil, errclass.New(errclass.PolicyRejected, "the replacing fact does not exist")
		}
		if err != nil {
			return Aggregate{}, nil, err
		}
		if successorSlot != slotID {
			return Aggregate{}, nil, errclass.New(errclass.PolicyRejected,
				"a fact can only be superseded by another value of the same predicate")
		}
		if *p.SupersededBy == *p.FactID {
			return Aggregate{}, nil, errclass.New(errclass.PolicyRejected, "a fact cannot supersede itself")
		}
	}
	return withCAS(ctx, cas, Aggregate{Type: AggregateFactSlot, ID: slotID, Mutating: true})
}

// withCAS resolves the revision for an aggregate a caller has just identified.
func withCAS(ctx context.Context, cas CAS, agg Aggregate) (Aggregate, *int64, error) {
	revision, err := cas(ctx, agg)
	if err != nil {
		return Aggregate{}, nil, err
	}
	return agg, revision, nil
}

func applyFactChange(ctx context.Context, tx DB, ev Event, revision *int64) error {
	var p FactChangePayload
	if err := decode(ev.Payload, &p); err != nil {
		return err
	}
	if revision == nil {
		return errclass.New(errclass.PolicyRejected, "%s is compare-and-set and must carry a revision", ev.Type)
	}

	var slotID uuid.UUID
	switch ev.Type {
	case "fact.conflict_flagged":
		slotID = SlotID(ev.ProjectID, p.Subject, p.Predicate)
		if _, err := tx.Exec(ctx,
			`UPDATE facts SET status = 'conflicted' WHERE slot_id = $1 AND status = 'active'`, slotID); err != nil {
			return err
		}
		return setSlot(ctx, tx, slotID, *revision, "conflicted")
	case "fact.superseded":
		if err := tx.QueryRow(ctx, `SELECT slot_id FROM facts WHERE fact_id = $1`, *p.FactID).Scan(&slotID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE facts SET status = 'superseded', superseded_by = $2, valid_to = $3 WHERE fact_id = $1`,
			*p.FactID, *p.SupersededBy, ev.OccurredAt); err != nil {
			return err
		}
	case "fact.retracted":
		if err := tx.QueryRow(ctx, `SELECT slot_id FROM facts WHERE fact_id = $1`, *p.FactID).Scan(&slotID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE facts SET status = 'retracted', valid_to = $2 WHERE fact_id = $1`,
			*p.FactID, ev.OccurredAt); err != nil {
			return err
		}
	}
	return resolveSlot(ctx, tx, slotID, *revision)
}

// resolveSlot recomputes whether a predicate is still contested.
//
// This is where a conflict actually ends: once explicit resolution has left one
// live value, the survivor goes back to active and so does the slot. Nothing
// resolves itself by waiting.
func resolveSlot(ctx context.Context, tx DB, slotID uuid.UUID, revision int64) error {
	var live int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM facts WHERE slot_id = $1 AND status IN ('active','conflicted')`, slotID).Scan(&live); err != nil {
		return err
	}
	status := "conflicted"
	if live <= 1 {
		status = "active"
		if _, err := tx.Exec(ctx,
			`UPDATE facts SET status = 'active' WHERE slot_id = $1 AND status = 'conflicted'`, slotID); err != nil {
			return err
		}
	}
	return setSlot(ctx, tx, slotID, revision, status)
}

func setSlot(ctx context.Context, tx DB, slotID uuid.UUID, revision int64, status string) error {
	_, err := tx.Exec(ctx,
		`UPDATE fact_slots SET revision = $2, status = $3, updated_at = now() WHERE slot_id = $1`,
		slotID, revision, status)
	return err
}

// canonicalDigest is the same rule the ledger digests a payload with: sorted
// keys, no insignificant whitespace, sha256, lowercase hex. Two hosts that
// promote the same value therefore agree it is the same value.
func canonicalDigest(raw json.RawMessage) (string, []byte, error) {
	var normalised any
	if err := json.Unmarshal(raw, &normalised); err != nil {
		return "", nil, errclass.New(errclass.PolicyRejected, "the object is not valid JSON")
	}
	canonical, err := json.Marshal(normalised)
	if err != nil {
		return "", nil, errclass.New(errclass.PolicyRejected, "the object could not be canonicalised")
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), canonical, nil
}
