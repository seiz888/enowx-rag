// Package collector is the local, durable hand-off between an agent host and
// the gateway.
//
// The problem it solves is narrow and unglamorous: an agent finishes a piece of
// work at the moment the network is down, the gateway is restarting, or the
// laptop lid is closing. If the write is attempted inline, the agent either
// blocks on a socket it cannot afford to wait for, or drops the event and the
// memory is simply gone. Neither is acceptable for a checkpoint, which is the
// one record that decides whether the next session can resume.
//
// So the collector accepts the event locally, writes it to disk before saying
// yes, and forwards it later. Everything else in this package follows from that
// one promise, and from the fact that the promise is worthless if it is not
// literally true after a power cut.
//
// # What is encrypted, and what is not
//
// The event payload -- the part that carries content -- is encrypted with
// AES-256-GCM under a key that never leaves the machine in the clear. The rest
// of each row is NOT encrypted, and this must not be described as an encrypted
// database. In plaintext on disk, for every queued event:
//
//	event_id, idempotency_key, project_id, workspace_id, work_id, session_id,
//	branch_id, event type, sensitivity class, priority, enqueue time,
//	attempt count, next attempt time, state, and the last error string.
//
// That is deliberate: the forwarder has to order, deduplicate, prioritise and
// reconcile without decrypting, and a queue that must unlock its key to decide
// what to retry is a queue that cannot run unattended. The cost is that anyone
// who reads the spool file learns which projects and works this host touched
// and when -- metadata, not content. Anyone who needs the content needs the key.
//
// The last_error string is the one field that could leak content by accident,
// so it is never the server's raw message: only an error class and a bounded,
// class-only reason are stored. See recordFailure.
//
// # Why SQLite and not a directory of files
//
// The enqueue has to be atomic with the response to the agent: an event the
// agent was told was accepted must be on disk, and an event that was not
// accepted must leave nothing behind. A file plus a rename gets that far, but
// the cursor, the attempt count and the state transitions are then a second
// thing to keep consistent with the first, and crash recovery becomes a
// directory scan with a lot of guessing. One transaction in a journalled
// database is the boring answer, and boring is the requirement here.
//
// synchronous=FULL is set and not negotiable. WAL alone means the write is in
// the operating system's hands, which is fine for a cache and not fine for the
// only copy of a checkpoint.
//
// # Stable identity across restarts
//
// The event id is derived from the caller's idempotency key
// (uuid.NewSHA1 over a fixed namespace), never generated fresh. A collector
// that restarts mid-flight and re-sends therefore presents the same event id
// and the same idempotency key, and the gateway answers from its receipt
// instead of writing a second copy. This is what makes at-least-once delivery
// safe: the duplicate is not avoided, it is made harmless.
//
// # Bounded everything
//
// The queue has a row limit, and a reserve inside that limit that only
// high-priority events (checkpoints and tombstones) may use, because the
// failure mode to avoid is a flood of routine evidence filling the disk and
// leaving no room for the one event that mattered. Retries are bounded and
// end in a visible dead-letter state, not an infinite loop. Shutdown is
// bounded: an in-flight send is either finished or abandoned, and abandoning it
// is safe precisely because of the paragraph above.
package collector
