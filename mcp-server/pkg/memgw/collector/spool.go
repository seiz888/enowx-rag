package collector

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	_ "modernc.org/sqlite" // pure-Go SQLite driver; the repository already uses it
)

// State is where one queued event stands. Every state except pending and
// sending is terminal, and terminal is not the same as delivered: quarantined
// and dead exist so a failure is something an operator can see and count,
// rather than something that quietly disappeared.
type State string

const (
	StatePending     State = "pending"
	StateSending     State = "sending"
	StateSent        State = "sent"
	StateQuarantined State = "quarantined"
	StateDead        State = "dead"
)

// eventNamespace makes the event id a function of the idempotency key.
//
// This is the property the whole restart story rests on: a collector that dies
// after writing the row and before hearing a receipt will, on the next attempt,
// present the same event id it presented before. The gateway then answers from
// its receipt instead of writing a second event. Generating a fresh UUID would
// turn every crash into a duplicate.
var eventNamespace = uuid.MustParse("a1b6cb26-6c48-4a15-9c1a-1a2f0b8f0f01")

// EventID is the id the collector will submit for a given idempotency key. It
// is exported because the caller is entitled to know the id before the event
// has been delivered -- that is what makes a local receipt useful.
func EventID(idempotencyKey string) uuid.UUID {
	return uuid.NewSHA1(eventNamespace, []byte(idempotencyKey))
}

// Event is one submission handed to the collector by a local agent.
//
// The body is the gateway submission exactly as it will be sent. The collector
// does not rewrite it: a queue that re-serialises the thing it was given is a
// queue that can change it, and the idempotency contract at the far end is over
// bytes.
type Event struct {
	IdempotencyKey string
	ProjectID      string
	WorkspaceID    string
	WorkID         string
	SessionID      string
	BranchID       string
	Type           string
	Sensitivity    string
	Body           json.RawMessage
}

// Priority decides who may use the reserve at the bottom of the queue.
//
// The failure this exists for is concrete: a loop that emits evidence every few
// seconds fills the spool, and then the one checkpoint that would have let the
// next session resume is the write that gets refused. Checkpoints and
// tombstones therefore have a reserved slice of the queue that routine events
// may not touch. A tombstone counts because a deletion that cannot be recorded
// is worse than a checkpoint that cannot: it leaves content alive that somebody
// asked to have removed.
func (e Event) Priority() int {
	switch {
	case strings.HasPrefix(e.Type, "checkpoint."):
		return 1
	case strings.HasSuffix(e.Type, ".tombstoned"), strings.HasPrefix(e.Type, "tombstone."):
		return 1
	default:
		return 0
	}
}

// Row is a queued event as the forwarder sees it.
type Row struct {
	Seq            int64
	EventID        string
	IdempotencyKey string
	ProjectID      string
	Type           string
	Attempts       int
	Body           json.RawMessage
}

// Accepted is what the collector tells the caller once the event is on disk.
// Durable is always true when the error is nil -- there is no path that returns
// an acceptance for something still in memory.
type Accepted struct {
	EventID string `json:"event_id"`
	Seq     int64  `json:"seq"`
	Durable bool   `json:"durable"`
}

// Stats is what an operator needs to answer "is anything stuck".
type Stats struct {
	Pending     int64 `json:"pending"`
	Sending     int64 `json:"sending"`
	Sent        int64 `json:"sent"`
	Quarantined int64 `json:"quarantined"`
	Dead        int64 `json:"dead"`
	AckedSeq    int64 `json:"acked_seq"`
}

// Errors the caller is expected to distinguish. They are values rather than
// strings because the pipe protocol turns them into a class the agent can act
// on: a full queue is a reason to stop and tell somebody, a duplicate is not.
var (
	ErrQueueFull  = errors.New("collector: the spool is full")
	ErrDuplicate  = errors.New("collector: this idempotency key is already queued")
	ErrBodyTooBig = errors.New("collector: the event body is larger than the collector accepts")
	// ErrDuplicateHeld is a duplicate whose existing row is quarantined or
	// dead. It is a duplicate -- errors.Is reports both -- but it is the one
	// case where answering "durable" alone would be a lie: the row it
	// deduplicates against is never going to be delivered, so a host told only
	// "already queued" would believe an event reached the ledger that never
	// will. An operator has to release or purge the held row first.
	ErrDuplicateHeld = fmt.Errorf("%w and is held, so this event will not be delivered until an operator releases or purges it", ErrDuplicate)
)

// SpoolConfig bounds the queue. Every field has a working default; a zero
// config is a usable config, because a collector that will not start until it
// is configured is a collector that does not run.
type SpoolConfig struct {
	MaxRows       int64         // total non-terminal rows allowed
	ReserveRows   int64         // of those, how many only priority events may use
	MaxBodyBytes  int           // per event
	MaxAttempts   int           // before a row is dead-lettered
	BaseBackoff   time.Duration // first retry delay
	MaxBackoff    time.Duration // ceiling
	SentRetention time.Duration // how long delivered rows are kept for reconciliation
}

func (c SpoolConfig) withDefaults() SpoolConfig {
	if c.MaxRows <= 0 {
		c.MaxRows = 20000
	}
	if c.ReserveRows <= 0 {
		c.ReserveRows = 2000
	}
	if c.ReserveRows >= c.MaxRows {
		c.ReserveRows = c.MaxRows / 10
	}
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = 1 << 20
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 12
	}
	if c.BaseBackoff <= 0 {
		c.BaseBackoff = 2 * time.Second
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = 5 * time.Minute
	}
	if c.SentRetention <= 0 {
		c.SentRetention = 24 * time.Hour
	}
	return c
}

// Spool is the durable queue.
type Spool struct {
	db  *sql.DB
	env *envelope
	cfg SpoolConfig
	now func() time.Time
}

const spoolSchema = `
CREATE TABLE IF NOT EXISTS spool (
	seq              INTEGER PRIMARY KEY AUTOINCREMENT,
	event_id         TEXT    NOT NULL UNIQUE,
	idempotency_key  TEXT    NOT NULL UNIQUE,
	project_id       TEXT    NOT NULL,
	workspace_id     TEXT    NOT NULL DEFAULT '',
	work_id          TEXT    NOT NULL DEFAULT '',
	session_id       TEXT    NOT NULL DEFAULT '',
	branch_id        TEXT    NOT NULL DEFAULT '',
	type             TEXT    NOT NULL,
	sensitivity      TEXT    NOT NULL DEFAULT '',
	priority         INTEGER NOT NULL DEFAULT 0,
	state            TEXT    NOT NULL,
	attempts         INTEGER NOT NULL DEFAULT 0,
	enqueued_at      INTEGER NOT NULL,
	next_attempt_at  INTEGER NOT NULL,
	settled_at       INTEGER,
	fail_class       TEXT,
	fail_reason      TEXT,
	receipt_state    TEXT,
	nonce            BLOB    NOT NULL,
	ciphertext       BLOB    NOT NULL
);
CREATE INDEX IF NOT EXISTS spool_ready_idx ON spool (state, next_attempt_at, seq);
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
INSERT OR IGNORE INTO meta (key, value) VALUES ('acked_seq', '0');
INSERT OR IGNORE INTO meta (key, value) VALUES ('schema_version', '1');
`

// OpenSpool opens or creates the queue at path.
//
// The pragmas are part of the contract, not tuning. journal_mode=WAL keeps a
// reader from blocking the enqueue; synchronous=FULL is what makes "accepted"
// mean "on the platter". Dropping the second one would make the benchmarks
// look better and the promise false.
func OpenSpool(path string, key []byte, cfg SpoolConfig) (*Spool, error) {
	env, err := newEnvelope(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection. The collector is not a throughput problem, and a single
	// writer removes every SQLITE_BUSY interleaving that would otherwise have to
	// be reasoned about at three in the morning.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec(spoolSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("collector: create spool schema: %w", err)
	}
	var mode, sync string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		db.Close()
		return nil, err
	}
	if err := db.QueryRow(`PRAGMA synchronous`).Scan(&sync); err != nil {
		db.Close()
		return nil, err
	}
	// 2 is FULL. Asserting it here means a driver that silently ignored the
	// pragma is a startup failure rather than a durability claim nobody checked.
	if !strings.EqualFold(mode, "wal") || sync != "2" {
		db.Close()
		return nil, fmt.Errorf("collector: spool opened with journal_mode=%s synchronous=%s, want wal/2", mode, sync)
	}
	return &Spool{db: db, env: env, cfg: cfg.withDefaults(), now: time.Now}, nil
}

func (s *Spool) Close() error { return s.db.Close() }

// Config returns the bounds in force, so a caller can report them without
// keeping a second copy that drifts.
func (s *Spool) Config() SpoolConfig { return s.cfg }

// Enqueue writes one event to disk and returns only after the write is durable.
//
// The order matters: the row, the queue-limit check and the cursor are one
// transaction. A crash inside it leaves nothing, and the caller never got an
// acceptance, so there is no state where an agent believes an event was
// accepted and the disk disagrees.
func (s *Spool) Enqueue(ctx context.Context, e Event) (Accepted, error) {
	if len(e.Body) > s.cfg.MaxBodyBytes {
		return Accepted{}, ErrBodyTooBig
	}
	if e.IdempotencyKey == "" || e.ProjectID == "" || e.Type == "" {
		return Accepted{}, errors.New("collector: an event needs an idempotency key, a project and a type")
	}
	eventID := EventID(e.IdempotencyKey).String()
	nonce, ciphertext, err := s.env.seal(aadFor(eventID, e.IdempotencyKey), e.Body)
	if err != nil {
		return Accepted{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Accepted{}, err
	}
	defer tx.Rollback()

	var live int64
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM spool WHERE state IN ('pending','sending')`).Scan(&live); err != nil {
		return Accepted{}, err
	}
	priority := e.Priority()
	limit := s.cfg.MaxRows
	if priority == 0 {
		limit = s.cfg.MaxRows - s.cfg.ReserveRows
	}
	if live >= limit {
		return Accepted{}, fmt.Errorf("%w: %d queued, limit %d for priority %d", ErrQueueFull, live, limit, priority)
	}

	now := s.now().UTC()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO spool (event_id, idempotency_key, project_id, workspace_id, work_id,
		                    session_id, branch_id, type, sensitivity, priority, state,
		                    attempts, enqueued_at, next_attempt_at, nonce, ciphertext)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,0,?,?,?,?)`,
		eventID, e.IdempotencyKey, e.ProjectID, e.WorkspaceID, e.WorkID,
		e.SessionID, e.BranchID, e.Type, e.Sensitivity, priority, string(StatePending),
		now.UnixMilli(), now.UnixMilli(), nonce, ciphertext)
	if err != nil {
		if isUniqueViolation(err) {
			// Already queued. This is the normal shape of an agent retrying
			// after its own crash, and it is not an error worth alarming about
			// -- unless the row it collides with is one nobody is retrying any
			// more, which is a different fact and has to be said differently.
			var state string
			if err := tx.QueryRowContext(ctx,
				`SELECT state FROM spool WHERE idempotency_key = ?`, e.IdempotencyKey).Scan(&state); err != nil {
				return Accepted{}, err
			}
			if state == string(StateQuarantined) || state == string(StateDead) {
				return Accepted{EventID: eventID, Durable: true}, ErrDuplicateHeld
			}
			return Accepted{EventID: eventID, Durable: true}, ErrDuplicate
		}
		return Accepted{}, err
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return Accepted{}, err
	}
	if err := tx.Commit(); err != nil {
		return Accepted{}, err
	}
	return Accepted{EventID: eventID, Seq: seq, Durable: true}, nil
}

// Claim takes the oldest event that is due, marking it sending.
//
// Priority is honoured before age, so a checkpoint written during a backlog is
// not stuck behind an hour of evidence. Within a priority the order is the
// order events happened, which is what makes the receiving ledger's revisions
// line up with what the agent actually did.
func (s *Spool) Claim(ctx context.Context) (*Row, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var (
		r          Row
		nonce, ct  []byte
		idem, eid  string
		projectID  string
		eventType  string
		attemptsIn int
		seq        int64
	)
	err = tx.QueryRowContext(ctx,
		`SELECT seq, event_id, idempotency_key, project_id, type, attempts, nonce, ciphertext
		   FROM spool
		  WHERE state = 'pending' AND next_attempt_at <= ?
		  ORDER BY priority DESC, seq ASC
		  LIMIT 1`, s.now().UTC().UnixMilli()).
		Scan(&seq, &eid, &idem, &projectID, &eventType, &attemptsIn, &nonce, &ct)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	body, err := s.env.open(aadFor(eid, idem), nonce, ct)
	if err != nil {
		// The row cannot be sent and never will be under this key. Quarantine
		// rather than retry: a bad key does not improve with waiting, and a
		// loop over an unreadable row would hide every row behind it.
		if _, qerr := tx.ExecContext(ctx,
			`UPDATE spool SET state='quarantined', settled_at=?, fail_class='unreadable',
			                  fail_reason='the stored payload did not authenticate under the current key'
			  WHERE seq = ?`, s.now().UTC().UnixMilli(), seq); qerr != nil {
			return nil, qerr
		}
		if cerr := tx.Commit(); cerr != nil {
			return nil, cerr
		}
		return nil, fmt.Errorf("collector: row %d quarantined: %w", seq, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE spool SET state='sending' WHERE seq = ?`, seq); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	r = Row{Seq: seq, EventID: eid, IdempotencyKey: idem, ProjectID: projectID,
		Type: eventType, Attempts: attemptsIn, Body: body}
	return &r, nil
}

// Settle records a delivered event and advances the cursor.
//
// receiptState is the gateway's word, kept verbatim, because "committed" and
// "duplicate" are different facts about what happened and collapsing them would
// destroy the only evidence that a retry was harmless.
func (s *Spool) Settle(ctx context.Context, seq int64, receiptState string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now().UTC()
	if _, err := tx.ExecContext(ctx,
		`UPDATE spool SET state='sent', settled_at=?, receipt_state=?, fail_class=NULL, fail_reason=NULL
		  WHERE seq = ?`, now.UnixMilli(), receiptState, seq); err != nil {
		return err
	}
	if err := advanceCursor(ctx, tx); err != nil {
		return err
	}
	// Bounded history: delivered rows are kept long enough to answer "did this
	// go" and then removed, so the spool does not grow without limit on a host
	// that never fails.
	cutoff := now.Add(-s.cfg.SentRetention).UnixMilli()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM spool WHERE state='sent' AND settled_at < ?`, cutoff); err != nil {
		return err
	}
	return tx.Commit()
}

// Fail records an attempt that did not deliver.
//
// retryable is the caller's judgement, and the two outcomes are different in
// kind: a transport error will be tried again until the attempt budget runs
// out, whereas a refusal the gateway classified (a rejected policy, a denied
// scope) is quarantined at once. Retrying a refusal is a way of turning one
// mistake into a permanent load.
//
// The stored reason is derived from the class by reasonFor, and no caller text
// reaches the disk. That is stricter than truncating a server message, and it
// has to be: a refusal message can quote the submission it refused, and the
// submission is the thing this file encrypts. A 200-character excerpt of a
// checkpoint is still an excerpt of a checkpoint.
func (s *Spool) Fail(ctx context.Context, seq int64, class string, retryable bool) (State, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	var attempts int
	if err := tx.QueryRowContext(ctx, `SELECT attempts FROM spool WHERE seq = ?`, seq).Scan(&attempts); err != nil {
		return "", err
	}
	attempts++
	now := s.now().UTC()
	class = knownClass(class)
	reason := reasonFor(class)

	next := StatePending
	switch {
	case !retryable:
		next = StateQuarantined
	case attempts >= s.cfg.MaxAttempts:
		// Dead-letter, not deletion. The event is still on disk and still
		// decryptable; what has ended is the automatic retrying.
		next = StateDead
	}
	if next == StatePending {
		delay := backoff(s.cfg.BaseBackoff, s.cfg.MaxBackoff, attempts)
		if _, err := tx.ExecContext(ctx,
			`UPDATE spool SET state='pending', attempts=?, next_attempt_at=?, fail_class=?, fail_reason=?
			  WHERE seq = ?`, attempts, now.Add(delay).UnixMilli(), class, reason, seq); err != nil {
			return "", err
		}
	} else {
		if _, err := tx.ExecContext(ctx,
			`UPDATE spool SET state=?, attempts=?, settled_at=?, fail_class=?, fail_reason=?
			  WHERE seq = ?`, string(next), attempts, now.UnixMilli(), class, reason, seq); err != nil {
			return "", err
		}
		if err := advanceCursor(ctx, tx); err != nil {
			return "", err
		}
	}
	return next, tx.Commit()
}

// Recover puts rows that were in flight when the process died back in the
// queue. It runs at startup, before anything is sent.
//
// A row left in sending is not evidence that the event was not delivered: the
// process may have died between the server's commit and the reply. That is
// exactly why re-sending is safe -- same event id, same idempotency key, and
// the gateway answers the second attempt from its receipt.
func (s *Spool) Recover(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE spool SET state='pending' WHERE state='sending'`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Spool) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	rows, err := s.db.QueryContext(ctx, `SELECT state, count(*) FROM spool GROUP BY state`)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var n int64
		if err := rows.Scan(&state, &n); err != nil {
			return st, err
		}
		switch State(state) {
		case StatePending:
			st.Pending = n
		case StateSending:
			st.Sending = n
		case StateSent:
			st.Sent = n
		case StateQuarantined:
			st.Quarantined = n
		case StateDead:
			st.Dead = n
		}
	}
	if err := rows.Err(); err != nil {
		return st, err
	}
	var acked string
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='acked_seq'`).Scan(&acked); err != nil {
		return st, err
	}
	fmt.Sscanf(acked, "%d", &st.AckedSeq)
	return st, nil
}

// advanceCursor moves acked_seq to the highest sequence below which nothing is
// outstanding.
//
// It is deliberately not "the highest settled row": a cursor that jumped past a
// pending event would say the queue was drained to a point it was not, and the
// only use of a cursor is to answer that question honestly.
func advanceCursor(ctx context.Context, tx *sql.Tx) error {
	var lowestOpen sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT min(seq) FROM spool WHERE state IN ('pending','sending')`).Scan(&lowestOpen); err != nil {
		return err
	}
	var highest sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT max(seq) FROM spool`).Scan(&highest); err != nil {
		return err
	}
	acked := highest.Int64
	if lowestOpen.Valid {
		acked = lowestOpen.Int64 - 1
	}
	if acked < 0 {
		acked = 0
	}
	_, err := tx.ExecContext(ctx,
		`UPDATE meta SET value = ? WHERE key='acked_seq' AND CAST(value AS INTEGER) < ?`,
		fmt.Sprint(acked), acked)
	return err
}

// backoff grows the delay with the attempt count and stops growing at the
// ceiling. There is no jitter because there is one collector per host: jitter
// exists to spread a thundering herd, and a herd of one does not thunder.
func backoff(base, max time.Duration, attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	shift := attempts - 1
	if shift > 20 {
		shift = 20
	}
	d := time.Duration(float64(base) * math.Pow(2, float64(shift)))
	if d > max || d <= 0 {
		return max
	}
	return d
}

// reasons maps a class to the sentence stored beside it.
//
// The table is closed, and knownClass forces anything unrecognised to
// "unclassified". Free text from the far end never reaches this column: a
// refusal message can quote the payload, and quoting the payload into a
// plaintext column would undo the encryption a few lines above it.
var reasons = map[string]string{
	"transport":                    "the gateway could not be reached",
	"unauthenticated":              "the gateway refused this collector's credential",
	"unavailable":                  "the gateway did not report a commit outcome",
	"server_error":                 "the gateway failed to handle the submission",
	"rate_limited":                 "the gateway asked this collector to slow down",
	"unreadable_receipt":           "the gateway answered success with a receipt this collector could not read",
	"unreadable":                   "the stored payload did not authenticate under the current key",
	"request":                      "the queued event could not be turned into a request",
	"refused":                      "the gateway refused this event",
	"malformed":                    "the gateway could not parse this event",
	"cas_conflict":                 "the gateway refused this event: it was written against a revision that has moved",
	"stale_revision":               "the gateway refused this event: the revision it names is behind",
	"scope_denied":                 "the gateway refused this event: this principal may not write here",
	"policy_rejected":              "the gateway refused this event: policy",
	"quarantined":                  "the gateway quarantined this event",
	"payload_too_large":            "the gateway refused this event: too large",
	"writer_epoch_fenced":          "the gateway refused this event: this writer has been fenced",
	"idempotency_payload_mismatch": "the gateway already holds a different event under this idempotency key",
	"unclassified":                 "the attempt failed for a reason this collector could not classify",
}

func knownClass(class string) string {
	if _, ok := reasons[class]; ok {
		return class
	}
	return "unclassified"
}

func reasonFor(class string) string { return reasons[class] }

func isUniqueViolation(err error) bool {
	// modernc.org/sqlite reports constraint failures in the message; there is no
	// exported code to compare against without importing the driver's internals.
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique constraint")
}

// --- the operator's surface for rows that stopped moving ---------------------

// Held is one row the forwarder has given up on, as an operator sees it.
//
// It carries no payload and no server message. Everything here is metadata the
// spool wrote itself: what kind of event it was, which session it belonged to,
// how many attempts it took and which frozen class ended it. Deciding what to
// do with a stuck event does not require reading the event, and reading it
// would mean decrypting a checkpoint onto a terminal.
type Held struct {
	Seq            int64  `json:"seq"`
	State          string `json:"state"`
	EventID        string `json:"event_id"`
	IdempotencyKey string `json:"idempotency_key"`
	ProjectID      string `json:"project_id"`
	SessionID      string `json:"session_id"`
	Type           string `json:"type"`
	Attempts       int    `json:"attempts"`
	FailClass      string `json:"fail_class,omitempty"`
	FailReason     string `json:"fail_reason,omitempty"`
	EnqueuedAt     string `json:"enqueued_at"`
	SettledAt      string `json:"settled_at,omitempty"`
}

// Holds lists the quarantined and dead rows, oldest first.
func (s *Spool) Holds(ctx context.Context, limit int) ([]Held, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, state, event_id, idempotency_key, project_id, session_id, type,
		        attempts, COALESCE(fail_class,''), COALESCE(fail_reason,''),
		        enqueued_at, COALESCE(settled_at,0)
		   FROM spool
		  WHERE state IN ('quarantined','dead')
		  ORDER BY seq ASC
		  LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Held
	for rows.Next() {
		var h Held
		var enq, settled int64
		if err := rows.Scan(&h.Seq, &h.State, &h.EventID, &h.IdempotencyKey, &h.ProjectID,
			&h.SessionID, &h.Type, &h.Attempts, &h.FailClass, &h.FailReason, &enq, &settled); err != nil {
			return nil, err
		}
		h.EnqueuedAt = time.UnixMilli(enq).UTC().Format(time.RFC3339Nano)
		if settled > 0 {
			h.SettledAt = time.UnixMilli(settled).UTC().Format(time.RFC3339Nano)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// Release puts a held row back in the queue with its attempt count reset.
//
// This is the action for a refusal that was about the server, not about the
// event: a policy the gateway has since been corrected on, a credential that
// has been reissued. The stored bytes are re-sent unchanged, because they are
// the bytes the agent actually produced, and a collector that edited an event
// on its way out would be a collector nobody could trust as evidence.
func (s *Spool) Release(ctx context.Context, seq int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE spool SET state='pending', attempts=0, next_attempt_at=?, settled_at=NULL
		  WHERE seq = ? AND state IN ('quarantined','dead')`,
		s.now().UTC().UnixMilli(), seq)
	if err != nil {
		return err
	}
	return oneRow(res, seq)
}

// Discard settles a held row as dead without deleting anything.
//
// The ciphertext stays on disk and the idempotency key stays claimed, so the
// event still cannot be silently re-created under a different id, and an
// auditor can still see that something was refused here.
func (s *Spool) Discard(ctx context.Context, seq int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE spool SET state='dead', settled_at=?, fail_class='operator_discarded',
		                  fail_reason='an operator stopped this event from being retried'
		  WHERE seq = ? AND state IN ('quarantined','dead')`,
		s.now().UTC().UnixMilli(), seq)
	if err != nil {
		return err
	}
	return oneRow(res, seq)
}

// Purge deletes a held row.
//
// This is the only operation in this file that destroys an event, and it exists
// for one situation: the payload itself is wrong, the host will produce a
// corrected one, and the corrected one derives the same idempotency key -- so
// while this row exists the correction is deduplicated against it and can never
// be queued. Purging frees the key.
//
// It refuses anything that is not quarantined or dead: a pending event is one
// nobody has decided about yet.
func (s *Spool) Purge(ctx context.Context, seq int64) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM spool WHERE seq = ? AND state IN ('quarantined','dead')`, seq)
	if err != nil {
		return err
	}
	return oneRow(res, seq)
}

func oneRow(res sql.Result, seq int64) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("collector: row %d is not quarantined or dead", seq)
	}
	return nil
}
