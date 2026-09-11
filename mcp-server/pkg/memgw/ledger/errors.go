package ledger

import (
	"github.com/enowdev/enowx-rag/pkg/memgw/errclass"
)

// The frozen error classes live in pkg/memgw/errclass, because the domain layer
// has to produce them too and the ledger imports the domain rather than the
// other way round. These aliases keep the ledger's API unchanged for callers
// that already branch on ledger.ClassX.
type (
	// Class is a frozen error class from the Phase 2 contract.
	Class = errclass.Class
	// Error carries a frozen error class and a reason safe to log.
	Error = errclass.Error
)

const (
	ClassDuplicate           = errclass.Duplicate
	ClassPayloadMismatch     = errclass.PayloadMismatch
	ClassCASConflict         = errclass.CASConflict
	ClassScopeDenied         = errclass.ScopeDenied
	ClassPolicyRejected      = errclass.PolicyRejected
	ClassQuarantined         = errclass.Quarantined
	ClassStaleRevision       = errclass.StaleRevision
	ClassWriterEpochFenced   = errclass.WriterEpochFenced
	ClassUnknownCommitStatus = errclass.UnknownCommitStatus
	ClassPayloadTooLarge     = errclass.PayloadTooLarge
)

func classErr(class Class, format string, args ...any) *Error {
	return errclass.New(class, format, args...)
}

// ClassOf returns the frozen class of an error, or "" when the error did not
// come from the gateway.
func ClassOf(err error) Class { return errclass.Of(err) }

// IsClass reports whether err carries the given class.
func IsClass(err error, class Class) bool { return errclass.Is(err, class) }
