package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/enowdev/enowx-rag/pkg/memgw/collector"
)

// Result is what happened to one submission, in the terms a hook can act on.
//
// Durable is the field that matters. It is true only when the event is on
// storage that survives this process dying: the collector's encrypted spool, or
// the ledger itself. It is never set optimistically. A hook that returns
// "delivered" when the event is in a buffer somewhere is the failure mode this
// whole subsystem exists to remove.
type Result struct {
	Accepted  bool   `json:"accepted"`
	Durable   bool   `json:"durable"`
	Duplicate bool   `json:"duplicate,omitempty"`
	EventID   string `json:"event_id,omitempty"`
	Via       string `json:"via"`
	State     string `json:"state,omitempty"`
	Class     string `json:"class,omitempty"`
	Message   string `json:"message,omitempty"`
}

// Sink is somewhere a submission can be handed to.
type Sink interface {
	// Submit hands over one event. It returns an error only when the outcome is
	// unknown; a refusal the sink understood is a Result with a class, because
	// "the gateway said this event is malformed" and "the gateway could not be
	// reached" call for opposite responses from a hook.
	Submit(ctx context.Context, s Submission) (Result, error)
	// Name is what appears in Result.Via.
	Name() string
}

// GatewaySink submits straight to the gateway over HTTP.
//
// It is the fallback, not the preference. An event submitted this way is
// durable only if the gateway answered, so a host using this sink loses
// lifecycle events whenever the gateway is down or the network is not there.
// That is why the Windows hosts go through the collector instead, and why a
// host that has no collector is reported as having no offline durability rather
// than as merely working.
type GatewaySink struct {
	base   string
	token  string
	client *http.Client
}

// NewGatewaySink builds a sink from a base URL and a credential already read
// from disk. The token is passed in rather than read here, so the only code
// that decides where a secret lives is the command that starts the process.
func NewGatewaySink(baseURL, token string, timeout time.Duration) (*GatewaySink, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("adapter: %q is not a usable gateway base URL", baseURL)
	}
	if token == "" {
		return nil, errors.New("adapter: the gateway sink was given no credential")
	}
	if timeout <= 0 {
		// A hook blocks the host that fired it. An unbounded request would turn
		// a slow gateway into a wedged agent session, which is a worse outcome
		// than a lost lifecycle event.
		timeout = 10 * time.Second
	}
	return &GatewaySink{base: u.String(), token: token, client: &http.Client{Timeout: timeout}}, nil
}

// Name implements Sink.
func (g *GatewaySink) Name() string { return "gateway" }

// Submit implements Sink.
func (g *GatewaySink) Submit(ctx context.Context, s Submission) (Result, error) {
	// The event id is derived from the idempotency key by the same function the
	// collector uses, so an event that went one way once and the other way
	// after a configuration change is still the same event.
	body, err := withEventID(s)
	if err != nil {
		return Result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.base+"/memgw/v1/events", bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(req)
	if err != nil {
		// The answer was lost. Whether the write happened is unknown, and
		// saying so is the only honest answer -- the caller can reconcile
		// through the receipt endpoint, which is what the collector does.
		return Result{Via: g.Name()}, fmt.Errorf("adapter: the gateway could not be reached: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	switch {
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated:
		var r struct {
			EventID string `json:"event_id"`
			State   string `json:"state"`
		}
		if err := json.Unmarshal(raw, &r); err != nil || r.State == "" {
			return Result{Via: g.Name()}, errors.New("adapter: the gateway answered with a receipt this build cannot read")
		}
		return Result{
			Accepted: true,
			// Accepted by the gateway means it is in the ledger, which is the
			// most durable place there is in this system.
			Durable:   true,
			Duplicate: r.State == "duplicate",
			EventID:   r.EventID,
			State:     r.State,
			Via:       g.Name(),
		}, nil
	default:
		// The class is read; the message is not. A gateway message may quote
		// the submission back, and a hook's stderr goes into the host's own
		// logs, which is not somewhere content belongs.
		var p struct {
			Class string `json:"class"`
		}
		_ = json.Unmarshal(raw, &p)
		class := p.Class
		if class == "" {
			class = fmt.Sprintf("http_%d", resp.StatusCode)
		}
		return Result{Via: g.Name(), Class: class,
			Message: fmt.Sprintf("the gateway refused this event with status %d", resp.StatusCode)}, nil
	}
}

// withEventID re-encodes a submission with the derived event id added.
func withEventID(s Submission) ([]byte, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	id, err := json.Marshal(collector.EventID(s.IdempotencyKey).String())
	if err != nil {
		return nil, err
	}
	fields["event_id"] = id
	return json.Marshal(fields)
}

// pipeRequest is the collector's line protocol, restated here rather than
// imported, because collector.Request lives behind a Windows build tag on the
// server side and this encoder has to compile everywhere the adapter does.
type pipeRequest struct {
	Op             string          `json:"op"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	Event          json.RawMessage `json:"event,omitempty"`
}

type pipeResponse struct {
	OK        bool   `json:"ok"`
	EventID   string `json:"event_id,omitempty"`
	Seq       int64  `json:"seq,omitempty"`
	Durable   bool   `json:"durable,omitempty"`
	Class     string `json:"class,omitempty"`
	Message   string `json:"message,omitempty"`
	Duplicate bool   `json:"duplicate,omitempty"`
}

// submitOverPipe speaks the collector's protocol over an already-open
// connection. Keeping it separate from the dialling makes it testable on every
// platform against an in-memory pipe, which is why the protocol has a test on
// Linux as well as on the machine that has named pipes.
func submitOverPipe(rw io.ReadWriter, s Submission) (Result, error) {
	// The event goes over the wire without an event_id: the collector derives
	// it, and it refuses a request that tries to set one.
	event, err := json.Marshal(s)
	if err != nil {
		return Result{}, err
	}
	req, err := json.Marshal(pipeRequest{Op: "submit", IdempotencyKey: s.IdempotencyKey, Event: event})
	if err != nil {
		return Result{}, err
	}
	if _, err := rw.Write(append(req, '\n')); err != nil {
		return Result{Via: "collector"}, fmt.Errorf("adapter: the collector connection closed while sending: %w", err)
	}
	line, err := readOneLine(rw, 1<<20)
	if err != nil {
		return Result{Via: "collector"}, fmt.Errorf("adapter: the collector gave no answer: %w", err)
	}
	var resp pipeResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return Result{Via: "collector"}, errors.New("adapter: the collector answered with something this build cannot read")
	}
	if !resp.OK {
		return Result{Via: "collector", Class: resp.Class, Message: resp.Message}, nil
	}
	return Result{
		Accepted: true,
		// The collector only answers OK once the row is committed to its
		// encrypted spool, so this really is durability and not an
		// acknowledgement of receipt.
		Durable:   resp.Durable,
		Duplicate: resp.Duplicate,
		EventID:   resp.EventID,
		Via:       "collector",
	}, nil
}

// readOneLine reads until the first newline. bufio would be shorter but would
// read ahead past the answer, and the caller may want the connection back.
func readOneLine(r io.Reader, limit int) ([]byte, error) {
	var out []byte
	buf := make([]byte, 1)
	for {
		n, err := r.Read(buf)
		if n == 1 {
			if buf[0] == '\n' {
				return out, nil
			}
			out = append(out, buf[0])
			if len(out) > limit {
				return nil, fmt.Errorf("adapter: the collector's answer exceeded %d bytes", limit)
			}
			continue
		}
		if err != nil {
			if len(out) > 0 && errors.Is(err, io.EOF) {
				return out, nil
			}
			return nil, err
		}
	}
}
