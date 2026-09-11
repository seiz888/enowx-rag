// Package adapter turns a host's lifecycle hook into a canonical memgw event.
//
// # Why there is one adapter and not six
//
// The six hosts differ in how they announce a lifecycle moment -- a JSON object
// on stdin, a TypeScript callback, a shell script with environment variables --
// but they do not differ in what the moment means. A session started; a session
// was compacted; a session ended. Everything after that meaning is identical
// work: derive an identifier that survives a crash, build a submission the
// gateway will accept, hand it to something durable, and say plainly whether it
// was accepted.
//
// Implementing that work six times in three languages would mean six chances to
// get the idempotency key wrong, and the idempotency key is the one thing that
// decides whether a crashed host duplicates its history on restart. So the work
// lives here, in the binary, once. What each host gets is a registration
// fragment small enough to read in one screen: a settings entry, a hooks.json
// entry, a plugin that shells out. Nothing is patched into a host's
// installation, and no adapter needs its own node_modules.
//
// # Two shapes of input
//
// Hosts that invoke a command with their own JSON on stdin -- Claude Code,
// Droid, Codex -- are decoded natively, from the shape the host actually emits.
// Hosts where the fragment is code we write -- OMP, OpenCode, Hermes -- emit the
// canonical shape in [Hook] directly, because a translation written in
// TypeScript that we control is a translation we can keep honest with a schema
// rather than with a decoder.
//
// # The idempotency key is the whole point
//
// A hook fires, the adapter writes to the collector, the machine loses power,
// the host restarts and fires the hook again. If the key changed between those
// two attempts, the ledger now holds the same moment twice and no later reader
// can tell. So the key is derived from the meaning of the event and from
// nothing else: never the clock, never a random number, never a process id,
// never a counter held in memory. See [Key].
//
// The cost of that rule is stated rather than hidden. When a host offers no
// value that distinguishes one occurrence of an event from the next -- two
// compactions in one session, announced with byte-identical payloads -- the two
// occurrences collapse into one event. That is the conservative direction: a
// history that under-reports a repeated moment is recoverable, a history that
// invents duplicates on every crash is not.
//
// # What an adapter will not invent
//
// A checkpoint is a handover note with an objective, what was finished, what is
// pending and what is safe to do next. No lifecycle hook knows any of that. So
// this package never manufactures a checkpoint from a session-end hook: it
// records that the session ended. A checkpoint is submitted only when the host
// hands one over explicitly, with the work it belongs to and the revision it was
// written against, because a checkpoint that guessed its own content would be
// the most confidently wrong document in the store.
package adapter
