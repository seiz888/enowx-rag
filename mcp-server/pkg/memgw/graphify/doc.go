// Package graphify coordinates the repository code index.
//
// The index is a projection. It is derived from the working tree, it is
// rebuildable from nothing, and it is never authority: an answer read out of it
// is a hint that the caller must confirm against live source before acting on
// it. Nothing in this package writes to the ledger and nothing in the ledger
// depends on it.
//
// What the package actually provides is the part that is easy to get wrong:
//
//   - One rebuild at a time per repository, enforced by an OS lock rather than
//     by a lock file whose contents somebody has to trust. A lock file that
//     records a pid tells you a pid was written; it does not tell you the
//     process is alive, and the recovery procedure for a stale one is a human
//     deciding whether to delete somebody else's lock. An OS lock is released
//     by the kernel when the holder dies, which is the only crash recovery that
//     needs no procedure.
//
//   - Publication that is atomic. A generation is built in a directory nobody
//     reads, and becomes visible by replacing one small pointer file. A reader
//     therefore sees the previous generation or the next one, never a directory
//     halfway through being written.
//
//   - A manifest that says what the index was built from: the commit, a digest
//     of the dirty working tree, the tool and schema versions, and the moment.
//     That is what lets a reader decide the index is stale instead of trusting
//     it because it exists.
//
//   - A recheck between building and publishing. The working tree can change
//     while the walk is running, and an index published against a tree that has
//     moved on is an index that is wrong in a way nobody can see. If the digest
//     changed, the generation is published as stale, with the reason recorded.
//
//   - Exclusion of paths that hold secrets, applied to the walk itself rather
//     than to the output. See exclude.go.
//
// There is no network semantic extraction here and no place to configure one.
// Sending source to a provider to have it described is an egress decision, and
// an egress decision does not belong inside a rebuild that runs unattended.
//
// Hindsight is not referenced by this package at all. Shadow evaluation is
// optional and must never be able to block or delay a rebuild.
package graphify
