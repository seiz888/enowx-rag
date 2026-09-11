package adapter

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// keyVersion is mixed into every derivation.
//
// It exists so the derivation can be changed without the change being silent.
// If a later version derives keys differently, every key it produces differs
// from every key this version produced, and the ledger shows two eras of
// identifiers rather than one era with an unexplained duplicate in the middle.
const keyVersion = "mga1"

// Key derives the idempotency key for a hook.
//
// # The rule
//
// The key is a function of what the event means and of nothing else. It reads
// no clock, no random source, no process id, no counter held in memory and no
// file on disk. That is what makes it crash-stable: a hook that fires, is
// written to the spool, and is fired again after the machine loses power
// produces the same key both times, so the gateway recognises the second
// submission as the first one and the ledger holds one event.
//
// Every host in this fleet is a process that can be killed at any moment --
// that is the normal way an agent session ends -- so a key derived from a
// timestamp would not be an edge case, it would duplicate routinely.
//
// # What goes in
//
// The host, the canonical lifecycle, the session id, and whatever the host
// offered that distinguishes one occurrence of this event from the next. The
// checkpoint content, when there is one, is included in full: two different
// handover notes in one session are two events, and re-submitting the same note
// after a crash is one.
//
// # What it costs, stated plainly
//
// When a host offers nothing that separates two occurrences -- Claude Code
// announces every automatic compaction with a byte-identical payload -- the
// second occurrence derives the same key as the first and the gateway answers
// "duplicate". One compaction is recorded where two happened.
//
// That is a real loss of fidelity and it is the deliberate choice. The
// alternative is a key with a clock in it, which turns every crash-and-restart
// into a fabricated second event, and a history that invents moments is worse
// than one that merges two identical-looking ones. A host that can supply an
// occurrence number, a transcript digest or a monotonic session-local counter
// should put it in Hook.Discriminator, and the loss disappears for that host.
func Key(h Hook) string {
	var b strings.Builder
	b.WriteString(keyVersion)
	b.WriteByte('\x00')
	b.WriteString(h.Host)
	b.WriteByte('\x00')
	b.WriteString(string(h.Lifecycle))
	b.WriteByte('\x00')
	b.WriteString(h.SessionID)
	b.WriteByte('\x00')
	b.WriteString(h.ParentSessionID)
	b.WriteByte('\x00')
	b.WriteString(sortedDiscriminator(h.Discriminator))
	if h.BranchPointSeq != nil {
		b.WriteByte('\x00')
		b.WriteString("branch_point=")
		b.WriteString(itoa(*h.BranchPointSeq))
	}
	if h.Checkpoint != nil {
		b.WriteByte('\x00')
		b.WriteString(checkpointFingerprint(h.Checkpoint))
	}
	sum := sha256.Sum256([]byte(b.String()))
	// host.lifecycle in the clear, then the digest. The prefix is for the human
	// reading a spool or a receipts table; the digest is what makes it unique.
	// The whole thing is 4 + 1 + host + 1 + lifecycle + 1 + 64 bytes, which is
	// under the contract's 128-byte bound for every host in Hosts.
	return keyVersion + "." + h.Host + "." + shortLifecycle(h.Lifecycle) + "." + hex.EncodeToString(sum[:])
}

// shortLifecycle keeps the readable prefix inside the key bound without
// reducing it to something ambiguous.
var shortLifecycle = func(l Lifecycle) string {
	switch l {
	case SessionStart:
		return "start"
	case SessionResume:
		return "resume"
	case SessionBranch:
		return "branch"
	case SessionCompact:
		return "compact"
	case SessionEnd:
		return "end"
	case Checkpoint:
		return "ckpt"
	default:
		return "other"
	}
}

// checkpointFingerprint folds a handover note into the key.
//
// Every field is length-prefixed rather than merely joined, so two different
// checkpoints cannot be made to hash alike by moving a separator from the end
// of one field to the start of the next.
func checkpointFingerprint(c *CheckpointInput) string {
	var b strings.Builder
	write := func(s string) {
		b.WriteString(itoa(int64(len(s))))
		b.WriteByte(':')
		b.WriteString(s)
	}
	write(c.WorkID.String())
	write(itoa(c.ExpectedRevision))
	write(c.Objective)
	write(c.CompletedWork)
	write(c.PendingActions)
	write(c.Blockers)
	write(c.NextSafeAction)
	// The file list is hashed in the order given. Re-ordering it is a different
	// checkpoint as far as this key is concerned, which is the safe direction:
	// it produces a second event rather than silently merging two notes that
	// were not the same.
	for _, f := range c.ModifiedFiles {
		write(f)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	var buf [20]byte
	i := len(buf)
	u := uint64(v)
	if neg {
		u = uint64(-v)
	}
	for u > 0 {
		i--
		buf[i] = byte('0' + u%10)
		u /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
