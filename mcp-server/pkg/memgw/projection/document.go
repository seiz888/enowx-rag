package projection

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/enowdev/enowx-rag/pkg/memgw/domain"
	"github.com/enowdev/enowx-rag/pkg/rag"
)

// docNamespace derives document ids that are stable across rebuilds. A rebuild
// that produced new ids would not rebuild anything: it would write a second copy
// beside the first and leave the reader to guess which is current.
var docNamespace = uuid.MustParse("a1b6cb26-6c48-4a15-9c1a-1a2f0b8f0f02")

// Reserved payload keys. The provider promotes "content" to a field of its own
// and writes "doc_id", "embed_model" and "embed_dim" itself, so nothing built
// here may use those names.
var reservedMeta = map[string]bool{
	"content": true, "doc_id": true, "embed_model": true, "embed_dim": true,
}

// sensitivityRank orders the classes. A document is written only when its class
// ranks at or below the configured ceiling.
var sensitivityRank = map[string]int{
	"public": 0, "internal": 1, "confidential": 2, "restricted": 3,
}

// slotDocID is the document that holds the value currently standing in a
// single-valued slot: one document per slot, replaced by each promotion.
func slotDocID(slotID uuid.UUID) uuid.UUID {
	return uuid.NewSHA1(docNamespace, []byte("fact-slot\x00"+slotID.String()))
}

// valueDocID is the document for one value of a multi-valued slot. Those values
// coexist, so they cannot share a document; the digest of the object is what
// makes re-promoting the same value land on the same document instead of
// accumulating copies.
func valueDocID(slotID uuid.UUID, object []byte) uuid.UUID {
	sum := sha256.Sum256(object)
	return uuid.NewSHA1(docNamespace, []byte("fact-value\x00"+slotID.String()+"\x00"+hex.EncodeToString(sum[:])))
}

// baseMeta is what every projected document carries regardless of its kind: the
// event that produced it, where that event sits in the log, and the scope it
// belongs to. It is ids and classes only -- never content, and never anything
// the caller supplied as free text.
func baseMeta(r Record, kind string) map[string]string {
	m := map[string]string{
		"memgw":        "1",
		"kind":         kind,
		"event_id":     r.EventID.String(),
		"event_seq":    strconv.FormatInt(r.Seq, 10),
		"project_id":   r.ProjectID.String(),
		"workspace_id": r.WorkspaceID.String(),
		"session_id":   r.SessionID,
		"branch_id":    r.BranchID.String(),
		"principal_id": r.PrincipalID.String(),
		"sensitivity":  r.Sensitivity,
		"occurred_at":  r.OccurredAt.UTC().Format(time.RFC3339),
	}
	if r.WorkID != nil {
		m["work_id"] = r.WorkID.String()
	}
	if r.Revision != nil {
		m["revision"] = strconv.FormatInt(*r.Revision, 10)
	}
	return m
}

// checkpointDocument renders a checkpoint as the note a later session reads.
//
// The layout is fixed and the fields are labelled because the same text is both
// what gets embedded and what a human reads when a search returns it. Empty
// fields are omitted rather than rendered as empty labels: a label with nothing
// after it is text that dilutes the vector and tells the reader nothing.
func checkpointDocument(r Record) (rag.Document, error) {
	var p domain.CheckpointPayload
	if err := json.Unmarshal(r.Payload, &p); err != nil {
		return rag.Document{}, failure("payload_unreadable", "the checkpoint payload does not parse")
	}
	var b strings.Builder
	write := func(label, value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(label)
		b.WriteString(": ")
		b.WriteString(value)
	}
	write("Objective", p.Objective)
	write("Completed", p.CompletedWork)
	write("Pending", p.PendingActions)
	write("Blockers", blockersText(p.Blockers))
	write("Next safe action", p.NextSafeAction)
	if len(p.ModifiedFiles) > 0 {
		files := append([]string(nil), p.ModifiedFiles...)
		sort.Strings(files)
		write("Modified files", strings.Join(files, ", "))
	}
	if b.Len() == 0 {
		// The contract requires an objective and a next safe action, so this is
		// unreachable through the gateway. It is checked anyway because an
		// empty document embeds to a vector that matches everything equally.
		return rag.Document{}, failure("payload_unreadable", "the checkpoint carries no projectable text")
	}
	return rag.Document{
		ID:      r.EventID.String(),
		Content: b.String(),
		Meta:    baseMeta(r, "checkpoint"),
	}, nil
}

// factDocument renders the value a promotion put into a slot.
func factDocument(r Record, p domain.PromotionPayload) (rag.Document, error) {
	subject := strings.TrimSpace(p.Subject)
	predicate := strings.TrimSpace(p.Predicate)
	if subject == "" || predicate == "" {
		return rag.Document{}, failure("payload_unreadable", "the promotion names no subject or no predicate")
	}
	slot := domain.SlotID(r.ProjectID, subject, predicate)
	object := canonicalObject(p.Object)

	id := slotDocID(slot)
	if p.Cardinality == "multi" {
		id = valueDocID(slot, p.Object)
	}
	meta := baseMeta(r, "fact")
	meta["slot_id"] = slot.String()
	meta["subject"] = subject
	meta["predicate"] = predicate
	if p.Cardinality != "" {
		meta["cardinality"] = p.Cardinality
	}
	if p.EvidenceClass != "" {
		meta["evidence_class"] = p.EvidenceClass
	}
	if p.FactID != nil {
		meta["fact_id"] = p.FactID.String()
	}
	return rag.Document{
		ID:      id.String(),
		Content: "Subject: " + subject + "\n\nPredicate: " + predicate + "\n\nValue: " + object,
		Meta:    meta,
	}, nil
}

// canonicalObject renders a fact's object for embedding. A JSON string is
// unquoted so the text that gets embedded is the value and not its encoding;
// anything else is left as the compact JSON it already is.
func canonicalObject(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

// blockersText renders the blockers field, which the frozen fixtures write both
// as a list and as a string.
func blockersText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []any:
		parts := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "; ")
	default:
		return ""
	}
}
