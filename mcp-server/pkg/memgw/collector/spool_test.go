package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The spool tests are white-box on purpose: two of the properties that matter
// (the clock the backoff uses, and what is on disk in plaintext) are not
// visible through the exported API, and testing them through a mock would be
// testing the mock.

func testKey() []byte {
	k := make([]byte, KeyLen)
	for i := range k {
		k[i] = byte(i * 7)
	}
	return k
}

func newSpool(t *testing.T, cfg SpoolConfig) *Spool {
	t.Helper()
	path := filepath.Join(t.TempDir(), "spool.db")
	s, err := OpenSpool(path, testKey(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func event(key, kind string, payload any) Event {
	body, _ := json.Marshal(map[string]any{
		"idempotency_key": key,
		"event_id":        EventID(key).String(),
		"project_id":      "11111111-1111-1111-1111-111111111111",
		"type":            kind,
		"payload":         payload,
	})
	return Event{
		IdempotencyKey: key,
		ProjectID:      "11111111-1111-1111-1111-111111111111",
		Type:           kind,
		Body:           body,
	}
}

func TestAnAcceptedEventIsOnDiskBeforeTheCallerIsTold(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spool.db")
	s, err := OpenSpool(path, testKey(), SpoolConfig{})
	if err != nil {
		t.Fatal(err)
	}
	acc, err := s.Enqueue(t.Context(), event("agent:plan:1", "work.planned", map[string]any{"title": "t"}))
	if err != nil {
		t.Fatal(err)
	}
	if !acc.Durable || acc.Seq == 0 {
		t.Fatalf("acceptance did not claim durability: %+v", acc)
	}
	// Close without any graceful drain, then reopen: what survives is what was
	// committed, which is the only definition of durable that helps anyone.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := OpenSpool(path, testKey(), SpoolConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	row, err := again.Claim(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if row == nil {
		t.Fatal("the event accepted before the close was not there after it")
	}
	if row.EventID != acc.EventID {
		t.Fatalf("event id changed across the restart: %s then %s", acc.EventID, row.EventID)
	}
}

func TestTheEventIDIsTheSameAfterARestart(t *testing.T) {
	// The property the whole duplicate story rests on. It is a pure function,
	// so the test is small; it is here because if it ever stops being true,
	// every restart silently doubles the ledger.
	first := EventID("agent:checkpoint:42")
	second := EventID("agent:checkpoint:42")
	if first != second {
		t.Fatalf("the same idempotency key produced two ids: %s and %s", first, second)
	}
	if EventID("agent:checkpoint:43") == first {
		t.Fatal("different idempotency keys produced the same event id")
	}
}

func TestTheSameKeyIsNotQueuedTwice(t *testing.T) {
	s := newSpool(t, SpoolConfig{})
	if _, err := s.Enqueue(t.Context(), event("k", "work.planned", "a")); err != nil {
		t.Fatal(err)
	}
	acc, err := s.Enqueue(t.Context(), event("k", "work.planned", "b"))
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("second enqueue: %v", err)
	}
	if acc.EventID != EventID("k").String() {
		t.Fatalf("the duplicate answer named a different event: %s", acc.EventID)
	}
	st, err := s.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if st.Pending != 1 {
		t.Fatalf("a duplicate created a second row: %+v", st)
	}
}

func TestTheReserveKeepsRoomForACheckpoint(t *testing.T) {
	// The failure this defends against: a loop emitting evidence fills the
	// queue, and the checkpoint that would have let the next session resume is
	// the write that gets refused.
	s := newSpool(t, SpoolConfig{MaxRows: 10, ReserveRows: 3})
	for i := 0; i < 7; i++ {
		if _, err := s.Enqueue(t.Context(), event(fmt.Sprintf("ev:%d", i), "evidence.recorded", i)); err != nil {
			t.Fatalf("routine event %d: %v", i, err)
		}
	}
	_, err := s.Enqueue(t.Context(), event("ev:8", "evidence.recorded", 8))
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("the eighth routine event should have hit the limit minus the reserve, got %v", err)
	}
	// The reserve is still open to the events it exists for.
	for i := 0; i < 3; i++ {
		if _, err := s.Enqueue(t.Context(), event(fmt.Sprintf("cp:%d", i), "checkpoint.written", i)); err != nil {
			t.Fatalf("checkpoint %d was refused while the reserve was empty: %v", i, err)
		}
	}
	if _, err := s.Enqueue(t.Context(), event("cp:4", "checkpoint.written", 4)); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("the reserve did not end: %v", err)
	}
	// And a full queue refuses loudly rather than dropping: nothing was written.
	st, _ := s.Stats(t.Context())
	if st.Pending != 10 {
		t.Fatalf("the queue holds %d rows, want exactly the limit", st.Pending)
	}
}

func TestATombstoneCountsAsHighPriority(t *testing.T) {
	// A deletion that cannot be recorded is worse than a checkpoint that
	// cannot: it leaves content alive that somebody asked to have removed.
	if event("k", "work.tombstoned", nil).Priority() != 1 {
		t.Fatal("a tombstone was treated as routine")
	}
	if event("k", "evidence.recorded", nil).Priority() != 0 {
		t.Fatal("routine evidence claimed the reserve")
	}
}

func TestARefusalIsQuarantinedAndARetryIsNot(t *testing.T) {
	s := newSpool(t, SpoolConfig{MaxAttempts: 3, BaseBackoff: time.Millisecond})
	if _, err := s.Enqueue(t.Context(), event("a", "work.planned", 1)); err != nil {
		t.Fatal(err)
	}
	row, err := s.Claim(t.Context())
	if err != nil || row == nil {
		t.Fatalf("claim: %v %v", row, err)
	}
	state, err := s.Fail(t.Context(), row.Seq, "policy_rejected", false)
	if err != nil {
		t.Fatal(err)
	}
	if state != StateQuarantined {
		t.Fatalf("a refusal became %s; retrying a refusal forever is how one bad event becomes a permanent load", state)
	}
	if next, err := s.Claim(t.Context()); err != nil || next != nil {
		t.Fatalf("a quarantined row was claimed again: %v %v", next, err)
	}
}

func TestRetriesAreBoundedAndEndVisibly(t *testing.T) {
	s := newSpool(t, SpoolConfig{MaxAttempts: 3, BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond})
	if _, err := s.Enqueue(t.Context(), event("a", "work.planned", 1)); err != nil {
		t.Fatal(err)
	}
	var last State
	for i := 0; i < 3; i++ {
		row, err := s.Claim(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if row == nil {
			t.Fatalf("nothing to claim on attempt %d", i+1)
		}
		if last, err = s.Fail(t.Context(), row.Seq, "transport", true); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if last != StateDead {
		t.Fatalf("after the attempt budget the row is %s, want %s", last, StateDead)
	}
	st, err := s.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if st.Dead != 1 || st.Pending != 0 {
		t.Fatalf("a dead-lettered row is not visible in the counts: %+v", st)
	}
	// Dead-lettered, not deleted. The event is still there to be inspected.
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM spool WHERE state='dead'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("the dead row was removed rather than kept for an operator to see")
	}
}

func TestBackoffGrowsAndStops(t *testing.T) {
	base, max := time.Second, 10*time.Second
	if got := backoff(base, max, 1); got != time.Second {
		t.Fatalf("first retry waits %v", got)
	}
	if got := backoff(base, max, 3); got != 4*time.Second {
		t.Fatalf("third retry waits %v", got)
	}
	if got := backoff(base, max, 30); got != max {
		t.Fatalf("a long-failing row waits %v, which is not the ceiling", got)
	}
}

func TestTheCursorNeverPassesSomethingStillQueued(t *testing.T) {
	s := newSpool(t, SpoolConfig{})
	for i := 0; i < 3; i++ {
		if _, err := s.Enqueue(t.Context(), event(fmt.Sprintf("k%d", i), "work.planned", i)); err != nil {
			t.Fatal(err)
		}
	}
	// Settle the second row only. The cursor must stay behind the first, which
	// is still outstanding -- a cursor that jumped to 2 would claim a drain
	// that did not happen.
	if err := s.Settle(t.Context(), 2, "committed"); err != nil {
		t.Fatal(err)
	}
	st, err := s.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if st.AckedSeq != 0 {
		t.Fatalf("the cursor moved to %d while row 1 was still pending", st.AckedSeq)
	}
	if err := s.Settle(t.Context(), 1, "committed"); err != nil {
		t.Fatal(err)
	}
	if st, _ = s.Stats(t.Context()); st.AckedSeq != 2 {
		t.Fatalf("after rows 1 and 2 settled the cursor is %d, want 2", st.AckedSeq)
	}
}

func TestRecoveryPutsInFlightRowsBack(t *testing.T) {
	s := newSpool(t, SpoolConfig{})
	if _, err := s.Enqueue(t.Context(), event("a", "work.planned", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(t.Context()); err != nil {
		t.Fatal(err)
	}
	// The process dies here: the row is in sending and nobody is sending it.
	if got, err := s.Recover(t.Context()); err != nil || got != 1 {
		t.Fatalf("recover returned %d, %v", got, err)
	}
	row, err := s.Claim(t.Context())
	if err != nil || row == nil {
		t.Fatal("the abandoned row was not claimable after recovery")
	}
}

func TestAPayloadThatDoesNotAuthenticateIsQuarantinedNotRetried(t *testing.T) {
	s := newSpool(t, SpoolConfig{})
	if _, err := s.Enqueue(t.Context(), event("a", "work.planned", 1)); err != nil {
		t.Fatal(err)
	}
	// Move the ciphertext onto a different identity, which is what an edited
	// spool file looks like. GCM's associated data is what catches it.
	if _, err := s.db.Exec(`UPDATE spool SET event_id = ? WHERE seq = 1`, EventID("somebody-else").String()); err != nil {
		t.Fatal(err)
	}
	row, err := s.Claim(context.Background())
	if row != nil {
		t.Fatal("a row whose payload does not authenticate was handed to the forwarder")
	}
	if !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("claim reported %v", err)
	}
	st, _ := s.Stats(context.Background())
	if st.Quarantined != 1 {
		t.Fatalf("the unreadable row is not quarantined: %+v", st)
	}
}

func TestAWrongKeyIsNotTreatedAsCorruption(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spool.db")
	s, err := OpenSpool(path, testKey(), SpoolConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue(t.Context(), event("a", "work.planned", 1)); err != nil {
		t.Fatal(err)
	}
	s.Close()

	other := make([]byte, KeyLen)
	s2, err := OpenSpool(path, other, SpoolConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	row, err := s2.Claim(t.Context())
	if row != nil || !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("opening a spool under the wrong key gave %v, %v", row, err)
	}
	// The row is still on disk. A spool from another account is a key problem,
	// and deleting the queue would destroy events that are fine under the right
	// key.
	var n int
	if err := s2.db.QueryRow(`SELECT count(*) FROM spool`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("the row was removed when the key did not match")
	}
}

func TestTheSpoolRefusesToRunWithoutFullSynchronousWrites(t *testing.T) {
	// Not a style check. WAL alone leaves the write in the operating system's
	// hands, which is fine for a cache and not fine for the only copy of a
	// checkpoint. If a future driver silently ignores the pragma, this fails at
	// open rather than in an incident.
	s := newSpool(t, SpoolConfig{})
	var sync string
	if err := s.db.QueryRow(`PRAGMA synchronous`).Scan(&sync); err != nil {
		t.Fatal(err)
	}
	if sync != "2" {
		t.Fatalf("synchronous is %s, want 2 (FULL)", sync)
	}
}

// TestHeldRowsAreVisibleAndDecidable: a refused event must be something an
// operator can see, and then act on, without reading the payload and without
// editing the database by hand. A queue whose only stuck-row procedure is
// "open spool.db in a SQLite browser" is a queue with no procedure.
func TestHeldRowsAreVisibleAndDecidable(t *testing.T) {
	s := newSpool(t, SpoolConfig{})
	ctx := t.Context()

	acc, err := s.Enqueue(ctx, event("agent:branch:1", "session.branched", map[string]any{"reason_class": "branch"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(ctx); err != nil {
		t.Fatal(err)
	}
	if state, err := s.Fail(ctx, acc.Seq, "policy_rejected", false); err != nil || state != StateQuarantined {
		t.Fatalf("a refusal should quarantine: state=%v err=%v", state, err)
	}

	held, err := s.Holds(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 1 {
		t.Fatalf("%d held rows, want 1", len(held))
	}
	h := held[0]
	if h.Seq != acc.Seq || h.State != string(StateQuarantined) || h.FailClass != "policy_rejected" {
		t.Fatalf("held row does not describe the refusal: %+v", h)
	}
	if h.Type != "session.branched" || h.IdempotencyKey != "agent:branch:1" {
		t.Fatalf("held row does not identify the event: %+v", h)
	}

	// Nothing in the listing may carry the payload: an operator triaging a
	// queue is not entitled to the content of a checkpoint.
	blob, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"reason_class", "payload", "ciphertext", "nonce"} {
		if strings.Contains(string(blob), leak) {
			t.Fatalf("the held listing leaked %q: %s", leak, blob)
		}
	}

	// Released: back in the queue, attempts reset, same bytes.
	if err := s.Release(ctx, acc.Seq); err != nil {
		t.Fatal(err)
	}
	row, err := s.Claim(ctx)
	if err != nil || row == nil {
		t.Fatalf("a released row should be claimable again: %v", err)
	}
	if row.Attempts != 0 || row.IdempotencyKey != "agent:branch:1" {
		t.Fatalf("released row changed: %+v", row)
	}
	if _, err := s.Fail(ctx, acc.Seq, "policy_rejected", false); err != nil {
		t.Fatal(err)
	}

	// Discarded: still on disk, still holding its key, no longer retried.
	if err := s.Discard(ctx, acc.Seq); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue(ctx, event("agent:branch:1", "session.branched", map[string]any{"reason_class": "rewind"})); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("a discarded row must keep its key claimed, got %v", err)
	}

	// Purged: the key is free, so a corrected event can be queued.
	if err := s.Purge(ctx, acc.Seq); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue(ctx, event("agent:branch:1", "session.branched", map[string]any{"reason_class": "rewind"})); err != nil {
		t.Fatalf("after a purge the corrected event should queue: %v", err)
	}

	// A pending row is not an operator's to decide: nobody has refused it.
	if err := s.Purge(ctx, 2); err == nil {
		t.Fatal("purging a pending row should be refused")
	}
}

// TestADuplicateOfAHeldRowIsNotReportedAsQueued is the rollback case: the
// gateway refused an event, the collector quarantined it, and the host fires
// the same hook again -- with corrected configuration, which is exactly what an
// operator does after rolling a writer forward. The corrected event derives the
// same idempotency key, so it deduplicates against the held row and is never
// delivered. Answering "duplicate, durable" and nothing else would tell the
// host its event reached the ledger.
func TestADuplicateOfAHeldRowIsNotReportedAsQueued(t *testing.T) {
	s := newSpool(t, SpoolConfig{})
	if _, err := s.Enqueue(t.Context(), event("k", "work.planned", "a")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE spool SET state = ? WHERE seq = 1`, string(StateQuarantined)); err != nil {
		t.Fatal(err)
	}

	_, err := s.Enqueue(t.Context(), event("k", "work.planned", "a"))
	if !errors.Is(err, ErrDuplicateHeld) {
		t.Fatalf("enqueue against a held row reported %v", err)
	}
	// It is still a duplicate: a caller that only asks that question gets the
	// same answer it always did.
	if !errors.Is(err, ErrDuplicate) {
		t.Fatal("a held duplicate stopped being a duplicate")
	}

	// Once the operator purges the held row the key is free and the corrected
	// event queues normally.
	if err := s.Purge(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue(t.Context(), event("k", "work.planned", "a")); err != nil {
		t.Fatalf("after a purge the corrected event was still refused: %v", err)
	}
	st, _ := s.Stats(t.Context())
	if st.Pending != 1 || st.Quarantined != 0 {
		t.Fatalf("queue after the repair: %+v", st)
	}
}

// TestAPendingDuplicateIsStillJustADuplicate: the ordinary crash-retry shape
// must not start warning about held rows.
func TestAPendingDuplicateIsStillJustADuplicate(t *testing.T) {
	s := newSpool(t, SpoolConfig{})
	if _, err := s.Enqueue(t.Context(), event("k", "work.planned", "a")); err != nil {
		t.Fatal(err)
	}
	_, err := s.Enqueue(t.Context(), event("k", "work.planned", "a"))
	if errors.Is(err, ErrDuplicateHeld) {
		t.Fatal("a pending duplicate was reported as held")
	}
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("second enqueue: %v", err)
	}
}
