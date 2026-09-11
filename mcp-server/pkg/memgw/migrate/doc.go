// Package migrate plans the move from the pre-gateway corpus to the gateway.
//
// What is being moved is narrower than it first looks, and the narrowness is
// the design.
//
// Historical session documents stay where they are. enowx-rag's Qdrant
// collections remain the store for history, because history is what they are
// good at and copying a corpus between stores is a way to end up with two
// disagreeing copies of the same thing. What the ledger needs from that corpus
// is one thing only: a map from each pre-gateway chunk id back to the document
// it came from, so that a tombstone issued after re-chunking can still find
// every point it has to delete. Without that map, deletion is best-effort
// against ids that no longer exist.
//
// Nothing in this package promotes anything to a current fact. A sentence in a
// months-old session transcript is not a fact the system believes; it is
// evidence that somebody once wrote it down. Facts are promoted by a principal
// with the right role, against a work, with a revision -- and a migration has
// none of those. A record that looks like a durable claim is recorded in the
// plan as a candidate for review and nothing more. There is no flag that turns
// that into a promotion.
//
// The plan is the product. Planning reads, classifies and writes a file; it
// changes nothing. Two runs over the same source must produce byte-identical
// plans, including every derived identifier, because a plan that differs
// between the rehearsal and the run is a plan that was never rehearsed. The
// identifiers are therefore derived from content -- namespaced uuidv5 over the
// source's own ids -- and never from a clock, a counter or a random source.
//
// Every record gets a decision and every decision carries a reason from a
// closed set. There is no fifth outcome and no silent drop: a record that this
// package cannot classify is rejected, loudly, with the reason. A corpus of
// thirty thousand chunks where the counts do not add up is a corpus nobody can
// sign off.
//
// The plan holds no text. Decisions are made by looking at the record; what is
// written down is the chunk id, the digest, the decision and the reason. A plan
// file is something an operator opens on a laptop and pastes into a ticket, and
// a plan that quoted the corpus would be a corpus with a second copy in it.
package migrate
