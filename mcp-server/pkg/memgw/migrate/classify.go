package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// ProjectNamespace turns a rag project name into a stable project id.
//
// uuidv5, so the same corpus planned on two machines a week apart maps to the
// same project. A generated id would make the rehearsal and the run disagree
// about what they were writing, which is the failure this whole package is
// arranged to prevent.
//
// It is a fallback, not the normal path. A derived id belongs to no project in
// the registry, and a tombstone issued against the real project would never
// reach chunks filed under it -- the deletion would report success and delete
// nothing. Pass the ledger project id whenever the corpus belongs to a project
// that exists; deriving one is for planning a corpus that has not been placed
// yet.
var ProjectNamespace = uuid.MustParse("a1b6cb26-6c48-4a15-9c1a-1a2f0b8f0f04")

// ProjectID derives the ledger project id for a rag project name.
func ProjectID(ragProject string) uuid.UUID {
	return uuid.NewSHA1(ProjectNamespace, []byte("memgw-rag-project\x00"+strings.ToLower(strings.TrimSpace(ragProject))))
}

// documentNamespace derives a document id for a record that has none. A chunk
// with no document is still a chunk that must be deletable, and a derived id is
// better than dropping it -- as long as it is derived from the chunk and not
// from anything that moves.
var documentNamespace = uuid.MustParse("a1b6cb26-6c48-4a15-9c1a-1a2f0b8f0f05")

func derivedDocumentID(ragProject, chunkID string) string {
	return uuid.NewSHA1(documentNamespace, []byte("memgw-rag-document\x00"+ragProject+"\x00"+chunkID)).String()
}

// payloadBytesMax mirrors the frozen contract's bound. A record larger than the
// ledger will ever accept is quarantined rather than silently truncated.
const payloadBytesMax = 262144

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

var knownSensitivity = map[string]bool{
	"public": true, "internal": true, "confidential": true, "restricted": true,
}

// credentialPatterns are what makes a record wait for a human.
//
// They are deliberately over-broad. A false positive costs one line in a
// quarantine list that somebody reads; a false negative puts a credential into
// a mapping table, and from there into whatever reads it next. The matched text
// is never captured, never logged and never written into the plan -- only the
// fact that something matched.
var credentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(api[_-]?key|secret[_-]?key|access[_-]?token|bearer|authorization)\b\s*[:=]`),
	regexp.MustCompile(`(?i)\b(password|passwd|passphrase)\b\s*[:=]`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}\b`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`),
	regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}\b`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`(?i)\b(postgres|postgresql|mysql|mongodb|redis|amqp)://[^\s/]+:[^\s@]+@`),
	regexp.MustCompile(`(?i)\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.`),
}

// looksLikeCredential reports whether a record must be held back.
func looksLikeCredential(text string) bool {
	for _, re := range credentialPatterns {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

// factShaped recognises a record that reads like a durable claim rather than a
// narration. It changes nothing about what is written: it only marks the line
// so a reviewer can find the handful worth reading in a corpus of thousands.
var factPhrases = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^\s*(decision|policy|convention|rule|invariant)\s*[:\-]`),
	regexp.MustCompile(`(?i)\b(we (?:decided|agreed|standardis|standardiz)|from now on|always use|never use|must always|must never)\b`),
}

func factShaped(text string) bool {
	for _, re := range factPhrases {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

// Known is what the planner already believes about a chunk, read from the
// target before planning. It is what turns a re-run into a list of skips
// instead of a list of rewrites.
type Known struct {
	DocumentID   string
	SourceDigest string
	ProjectID    uuid.UUID
}

// classify decides one record. It is a pure function of the record and what is
// already known, which is what makes two runs identical.
func classify(r Record, known map[string]Known, project uuid.UUID) Entry {
	e := Entry{
		ChunkID:    strings.TrimSpace(r.ChunkID),
		RAGProject: strings.TrimSpace(r.RAGProject),
		DocumentID: strings.TrimSpace(r.DocumentID),
		Bytes:      int64(len(r.Text)),
	}
	if e.ChunkID == "" {
		e.Decision, e.Reason = Rejected, ReasonNoChunkID
		return e
	}
	if e.RAGProject == "" {
		e.Decision, e.Reason = Rejected, ReasonNoProject
		return e
	}
	e.ProjectID = project
	if e.ProjectID == uuid.Nil {
		e.ProjectID = ProjectID(e.RAGProject)
	}

	e.SourceDigest = strings.ToLower(strings.TrimSpace(r.SourceDigest))
	if e.SourceDigest == "" && r.Text != "" {
		sum := sha256.Sum256([]byte(r.Text))
		e.SourceDigest = hex.EncodeToString(sum[:])
	}
	if !sha256Hex.MatchString(e.SourceDigest) {
		e.Decision, e.Reason = Rejected, ReasonBadDigest
		return e
	}
	if e.DocumentID == "" {
		if r.Text == "" {
			e.Decision, e.Reason = Rejected, ReasonNoDocument
			return e
		}
		e.DocumentID = derivedDocumentID(e.RAGProject, e.ChunkID)
	}

	if r.Sensitivity != "" && !knownSensitivity[strings.ToLower(r.Sensitivity)] {
		e.Decision, e.Reason = Quarantined, ReasonUnknownSensitive
		return e
	}
	if e.Bytes > payloadBytesMax {
		e.Decision, e.Reason = Quarantined, ReasonTooLarge
		return e
	}
	if looksLikeCredential(r.Text) {
		e.Decision, e.Reason = Quarantined, ReasonSecretSuspected
		return e
	}

	if k, ok := known[e.ChunkID]; ok {
		if k.DocumentID == e.DocumentID && k.SourceDigest == e.SourceDigest && k.ProjectID == e.ProjectID {
			e.Decision, e.Reason = Skipped, ReasonAlreadyMapped
			return e
		}
		// The same chunk id pointing somewhere else is either a re-chunk that
		// reused ids, two corpora being merged, or a row filed under the wrong
		// project. All three need a human.
		e.Decision, e.Reason = Quarantined, ReasonRemappedDigest
		return e
	}

	e.Decision, e.Reason = Imported, ReasonOK
	if factShaped(r.Text) {
		e.Candidate = true
		e.Reason = ReasonFactShaped
	}
	return e
}

// Classify exposes one record's decision without a plan around it.
//
// It exists for the shadow evaluation, which has to be able to ask "what would
// this produce" without running a migration. It is the same function the
// planner uses -- deliberately, because an evaluation that ran a second copy of
// the logic would be evaluating the copy.
func Classify(r Record, known map[string]Known, project uuid.UUID) Entry {
	return classify(r, known, project)
}
