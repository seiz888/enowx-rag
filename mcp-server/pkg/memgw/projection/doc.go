// Package projection turns canonical events into the retrieval index.
//
// It is the first thing in the gateway that writes somewhere other than
// PostgreSQL, so the rule it exists to enforce is worth stating before any of
// the code: the ledger is the truth and this package is a cache of it. Nothing
// here writes an event, a receipt, a fact or a work state. If the whole index
// were deleted, the only thing lost would be the ability to search quickly, and
// a rebuild would restore it from the events that are still there.
//
// # What is projected, and what deliberately is not
//
// Three event types produce index writes:
//
//   - checkpoint.recorded -- one document per checkpoint. This is the handover
//     note a later session reads to resume, which is the entire reason a
//     retrieval index is worth having.
//   - fact.promoted -- one document per fact slot, carrying the value that
//     currently stands. Not one per promotion: a slot that accumulated a
//     document per historical value would let a search surface a superseded
//     answer beside the current one with nothing to tell them apart.
//   - tombstone.issued -- an erasure, not a write. It removes what the deletion
//     reaches, including points written before this gateway existed, by way of
//     the legacy chunk map.
//
// Two more remove a document without adding one: fact.superseded and
// fact.retracted.
//
// Everything else is applied as a no-op, and the list is explicit rather than a
// default. In particular fact.candidate_proposed is NOT projected. A candidate
// is a proposal that has not been reviewed; indexing it would mean a retrieval
// answer could be built out of something the system has not decided is true,
// and nothing at the point of retrieval would distinguish it from a fact. That
// is the "no auto-promotion of history to current facts" rule applied one layer
// down: the index may only contain what the ledger already promoted.
//
// evidence.recorded is not projected either. Evidence is a locator -- a test
// run, a diff, a file -- and the locator is useful to follow, not to search by
// similarity. Projecting it would fill the collection with near-identical rows
// that dilute every query for no retrieval gain.
//
// # Sensitivity
//
// Indexing means embedding, and embedding means the content leaves the process
// and reaches whatever computes the vector. So the sensitivity class on the
// event, not a flag the caller passes, decides whether a document is written at
// all. The default admits public and internal and withholds confidential and
// restricted. Withholding is a success, not a failure: the row is done and
// retrying it would only withhold it again.
//
// This is the conservative direction on purpose. A confidential checkpoint that
// is not searchable is an inconvenience; a confidential checkpoint that has
// been embedded by a third party cannot be un-sent.
//
// # Tombstones win
//
// Every write checks the deletion journal first, for the checkpoint, the work,
// the session and the project. This is what makes a deletion survive a rebuild:
// a rebuild replays events that are older than the tombstone, and without the
// check it would faithfully restore exactly what somebody asked to be removed.
// A skipped subject returns outbox.ErrSkippedTombstoned, which the worker
// treats as done.
//
// # Ordering, and the honest limit of it
//
// The outbox is at-least-once and, with more than one worker, out of order. A
// document therefore carries the event seq that produced it, and a write is
// skipped when the index already holds a document from a later seq. That makes
// a re-application of an older event harmless at rest.
//
// It is a guard, not a transaction: the read and the write are two calls, so
// two workers applying two versions of the same slot in the same instant can
// still interleave. The gateway's answer to that is configuration -- one worker
// per projection, which is what the outbox lease already makes natural -- and
// the guard is what makes a retry, a rebuild, or a restart safe. It is
// deliberately not described as concurrency-safe.
//
// # Vectors
//
// The applier does not choose an embedder; it is handed one. In non-production
// verification the embedder is FixtureEmbedder, which is deterministic and has
// no semantic content whatsoever (see embed.go). Every document records the
// model that embedded it, so a collection built from fixtures can be told apart
// from a real one by looking at it rather than by remembering.
package projection
