// Package errclass holds the frozen error classes and the error type that
// carries one.
//
// It is its own package because two layers need to produce a classified
// refusal: the ledger, which owns the write path, and the domain, which owns
// what a work transition or a fact promotion is allowed to mean. Putting the
// type in the ledger would force the domain to import it, and the ledger
// already imports the domain.
//
// The classes are the strings the Phase 2 contract froze. Callers branch on the
// class and never on the message.
package errclass

import (
	"errors"
	"fmt"
)

// Class is a frozen error class.
type Class string

const (
	Duplicate           Class = "duplicate"
	PayloadMismatch     Class = "idempotency_payload_mismatch"
	CASConflict         Class = "cas_conflict"
	ScopeDenied         Class = "scope_denied"
	PolicyRejected      Class = "policy_rejected"
	Quarantined         Class = "quarantined"
	StaleRevision       Class = "stale_revision"
	WriterEpochFenced   Class = "writer_epoch_fenced"
	UnknownCommitStatus Class = "unknown_commit_status"
	PayloadTooLarge     Class = "payload_too_large"
)

// Error carries a frozen class and a reason that is safe to log.
//
// The reason never contains the payload, a submitted identifier's contents, or
// anything the caller sent verbatim. A rejection is exactly the moment a naive
// error message copies the offending secret into a log file.
type Error struct {
	Class  Class
	Reason string
}

func (e *Error) Error() string { return fmt.Sprintf("memgw: %s: %s", e.Class, e.Reason) }

// New builds a classified error.
func New(class Class, format string, args ...any) *Error {
	return &Error{Class: class, Reason: fmt.Sprintf(format, args...)}
}

// Of returns the frozen class of an error, or "" when it did not come from the
// gateway.
func Of(err error) Class {
	var e *Error
	if errors.As(err, &e) {
		return e.Class
	}
	return ""
}

// Is reports whether err carries the given class.
func Is(err error, class Class) bool { return Of(err) == class }
