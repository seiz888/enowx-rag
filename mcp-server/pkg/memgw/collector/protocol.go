package collector

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// The local protocol: one JSON object per line, one answer per request.
//
// Line-delimited JSON is chosen over anything framed because the clients are
// six different agent hosts written in three languages, and the cheapest thing
// to implement correctly in all of them is "write a line, read a line". The
// cost is that a payload may not contain a raw newline, which JSON encoding
// already guarantees.
//
// The collector answers every request, including refusals. A client that gets
// no answer cannot tell a full queue from a dead collector, and would have to
// choose between dropping the event and blocking forever.

// Request is what a local agent sends.
//
// Event carries the gateway submission with one field missing: event_id. The
// collector derives that from the idempotency key so it is stable across
// restarts, and refuses a request that tries to set it, because an id the
// client chose is an id that changes when the client restarts.
type Request struct {
	Op             string          `json:"op"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	Event          json.RawMessage `json:"event,omitempty"`
}

// Response is what it gets back. Exactly one of Accepted or Class is set.
type Response struct {
	OK        bool   `json:"ok"`
	EventID   string `json:"event_id,omitempty"`
	Seq       int64  `json:"seq,omitempty"`
	Durable   bool   `json:"durable,omitempty"`
	Class     string `json:"class,omitempty"`
	Message   string `json:"message,omitempty"`
	Stats     *Stats `json:"stats,omitempty"`
	Duplicate bool   `json:"duplicate,omitempty"`
}

// maxLine bounds one request. A client that sends more than this is refused
// rather than allowed to grow the collector's memory from the other end of a
// pipe it was merely permitted to write to.
const maxLine = 4 << 20

// requiredEventFields are the routing fields the collector stores in plaintext
// and therefore must be able to read. Their absence is a client bug, and it is
// better found here, at enqueue time, than three retries later at the gateway.
var requiredEventFields = []string{"project_id", "type"}

// Serve handles one connection until the peer closes it or the context ends.
func Serve(ctx context.Context, s *Spool, rw io.ReadWriter, log *slog.Logger) error {
	br := bufio.NewReaderSize(rw, 64<<10)
	enc := json.NewEncoder(rw)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line, err := readLine(br, maxLine)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		resp := handle(ctx, s, line, log)
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
}

func handle(ctx context.Context, s *Spool, line []byte, log *slog.Logger) Response {
	var req Request
	dec := json.NewDecoder(strings.NewReader(string(line)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return Response{Class: "malformed", Message: "the request is not a JSON object this collector accepts"}
	}
	switch req.Op {
	case "ping":
		return Response{OK: true}
	case "stats":
		st, err := s.Stats(ctx)
		if err != nil {
			return Response{Class: "spool", Message: "the spool could not be read"}
		}
		return Response{OK: true, Stats: &st}
	case "submit":
		return submit(ctx, s, req, log)
	default:
		return Response{Class: "unknown_op", Message: fmt.Sprintf("this collector has no operation %q", req.Op)}
	}
}

func submit(ctx context.Context, s *Spool, req Request, log *slog.Logger) Response {
	if req.IdempotencyKey == "" {
		return Response{Class: "malformed", Message: "a submission needs an idempotency_key"}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(req.Event, &fields); err != nil {
		return Response{Class: "malformed", Message: "the event is not a JSON object"}
	}
	if _, taken := fields["event_id"]; taken {
		// Not a nicety. An event id chosen by the client is an id that changes
		// when the client restarts, and a changed id is a duplicate the gateway
		// cannot recognise as one.
		return Response{Class: "malformed",
			Message: "the event must not carry an event_id; the collector derives it from the idempotency key so it survives a restart"}
	}
	for _, f := range requiredEventFields {
		if _, ok := fields[f]; !ok {
			return Response{Class: "malformed", Message: "the event is missing " + f}
		}
	}
	if got, ok := fields["idempotency_key"]; ok {
		var inner string
		if err := json.Unmarshal(got, &inner); err != nil || inner != req.IdempotencyKey {
			return Response{Class: "malformed",
				Message: "the event's idempotency_key must match the request's, or be omitted"}
		}
	}

	eventID := EventID(req.IdempotencyKey)
	fields["event_id"], _ = json.Marshal(eventID.String())
	fields["idempotency_key"], _ = json.Marshal(req.IdempotencyKey)
	body, err := json.Marshal(fields)
	if err != nil {
		return Response{Class: "malformed", Message: "the event could not be re-encoded"}
	}

	e := Event{
		IdempotencyKey: req.IdempotencyKey,
		ProjectID:      stringField(fields, "project_id"),
		WorkspaceID:    stringField(fields, "workspace_id"),
		WorkID:         stringField(fields, "work_id"),
		SessionID:      stringField(fields, "session_id"),
		BranchID:       stringField(fields, "branch_id"),
		Type:           stringField(fields, "type"),
		Sensitivity:    stringField(fields, "sensitivity_class"),
		Body:           body,
	}
	acc, err := s.Enqueue(ctx, e)
	switch {
	case errors.Is(err, ErrDuplicateHeld):
		// Durable, but not on its way anywhere. Saying so is the whole point:
		// the host would otherwise read "duplicate, durable" as "this reached
		// the ledger", and the row it collided with is one the gateway
		// refused.
		log.Warn("collector deduplicated against a held row", "type", e.Type, "event_id", acc.EventID)
		return Response{EventID: acc.EventID, Durable: true, Duplicate: true, Class: "held",
			Message: "an earlier event with this idempotency key is held and will not be delivered; " +
				"an operator must release or purge it"}
	case errors.Is(err, ErrDuplicate):
		// Already queued under this key. Answering OK is correct: the caller's
		// event is durable, which is the only thing it asked about.
		return Response{OK: true, EventID: acc.EventID, Durable: true, Duplicate: true}
	case errors.Is(err, ErrQueueFull):
		log.Warn("collector refused a submission: spool full", "type", e.Type, "priority", e.Priority())
		return Response{Class: "queue_full",
			Message: "the collector's spool is full; this event was not accepted and was not written"}
	case errors.Is(err, ErrBodyTooBig):
		return Response{Class: "payload_too_large", Message: "the event is larger than the collector accepts"}
	case err != nil:
		log.Error("collector could not enqueue", "error", err)
		return Response{Class: "spool", Message: "the collector could not write this event to disk"}
	}
	return Response{OK: true, EventID: acc.EventID, Seq: acc.Seq, Durable: acc.Durable}
}

func stringField(fields map[string]json.RawMessage, key string) string {
	raw, ok := fields[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// readLine reads one newline-terminated line, refusing anything longer than
// limit. bufio.Scanner would have been shorter but silently stops at its own
// buffer size, turning an oversized request into a truncated one.
func readLine(br *bufio.Reader, limit int) ([]byte, error) {
	var out []byte
	for {
		chunk, err := br.ReadSlice('\n')
		out = append(out, chunk...)
		if len(out) > limit {
			return nil, fmt.Errorf("collector: a request exceeded %d bytes", limit)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			if len(out) > 0 && errors.Is(err, io.EOF) {
				return out, nil
			}
			return nil, err
		}
		return out, nil
	}
}
