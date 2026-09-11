package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Forwarder drains the spool into the gateway.
//
// It is the only component that holds the credential, and it holds it in memory
// only: the spool on disk contains no token, so a stolen spool file yields
// metadata and ciphertext, not access.
type Forwarder struct {
	spool  *Spool
	client *http.Client
	base   string
	token  string
	log    *slog.Logger

	idle time.Duration
}

// ForwarderConfig is what the forwarder needs to reach a gateway.
//
// The token is passed in rather than read from a file here, so that the only
// code that decides where a credential lives is the code that starts the
// process. A library that reads secrets from the environment on its own is a
// library that reads them somewhere nobody expected.
type ForwarderConfig struct {
	BaseURL string // e.g. http://127.0.0.1:7777
	Token   string
	Client  *http.Client
	Logger  *slog.Logger
	Idle    time.Duration // how long to wait when the queue is empty
}

func NewForwarder(s *Spool, cfg ForwarderConfig) (*Forwarder, error) {
	if s == nil {
		return nil, errors.New("collector: a forwarder needs a spool")
	}
	u, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("collector: %q is not a usable gateway base URL", cfg.BaseURL)
	}
	if cfg.Token == "" {
		return nil, errors.New("collector: a forwarder needs a credential")
	}
	client := cfg.Client
	if client == nil {
		// A bounded client, because an unbounded one turns a hung gateway into a
		// collector that stops draining and never says why.
		client = &http.Client{Timeout: 30 * time.Second}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	idle := cfg.Idle
	if idle <= 0 {
		idle = time.Second
	}
	return &Forwarder{spool: s, client: client, base: u.String(), token: cfg.Token, log: logger, idle: idle}, nil
}

// Run drains until the context ends.
//
// Recover runs first and unconditionally: anything left in sending by a crash
// goes back to pending before a single new send is attempted, so the queue is
// never draining around a row it has forgotten about.
func (f *Forwarder) Run(ctx context.Context) error {
	recovered, err := f.spool.Recover(ctx)
	if err != nil {
		return err
	}
	if recovered > 0 {
		f.log.Info("collector recovered in-flight rows", "rows", recovered)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		worked, err := f.Once(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// An error from Once is a spool error, not a delivery failure --
			// delivery failures are recorded on the row. Log and pause rather
			// than spin.
			f.log.Error("collector spool", "error", err)
		}
		if worked {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(f.idle):
		}
	}
}

// Once attempts one row. It reports whether it did any work, so Run can drain a
// backlog without sleeping between rows.
func (f *Forwarder) Once(ctx context.Context) (bool, error) {
	row, err := f.spool.Claim(ctx)
	if err != nil {
		// A quarantined-on-claim row is work: the queue moved, and the next call
		// should look again rather than sleep.
		return true, err
	}
	if row == nil {
		return false, nil
	}

	// Reconciliation before re-sending.
	//
	// A row with attempts on it has been sent at least once, and the reason it
	// is here again may be that the answer was lost rather than that the write
	// failed. Asking for the receipt first turns "I do not know" into a fact,
	// and costs one cheap GET on a path that is by definition already unusual.
	if row.Attempts > 0 {
		state, found, err := f.receipt(ctx, row.IdempotencyKey)
		if err == nil && found {
			f.log.Info("collector reconciled a lost response",
				"seq", row.Seq, "event_id", row.EventID, "receipt_state", state)
			return true, f.spool.Settle(ctx, row.Seq, state)
		}
	}

	state, class, retryable, err := f.send(ctx, row)
	if err != nil {
		next, ferr := f.spool.Fail(ctx, row.Seq, class, retryable)
		if ferr != nil {
			return true, ferr
		}
		if next != StatePending {
			f.log.Warn("collector stopped retrying an event",
				"seq", row.Seq, "event_id", row.EventID, "state", next, "class", class)
		}
		return true, nil
	}
	return true, f.spool.Settle(ctx, row.Seq, state)
}

// send performs one submission and classifies the outcome.
//
// The three-way split is the important part: delivered, retryable, and refused.
// Retrying a refusal forever is how a single malformed event becomes a
// permanent load on a server that has already said no.
func (f *Forwarder) send(ctx context.Context, row *Row) (receiptState, class string, retryable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		f.base+"/memgw/v1/events", bytes.NewReader(row.Body))
	if err != nil {
		return "", "request", false, err
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := f.client.Do(req)
	if err != nil {
		// The response was lost, which is not the same as the write failing.
		// The next attempt reconciles before re-sending.
		return "", "transport", true, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	switch {
	case resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK:
		var r struct {
			State string `json:"state"`
		}
		if jerr := json.Unmarshal(body, &r); jerr != nil || r.State == "" {
			// Accepted with an answer we cannot read: treat as unknown, not as
			// delivered. Reconciliation will settle it on the next pass.
			return "", "unreadable_receipt", true, errors.New("collector: unreadable receipt")
		}
		return r.State, "", false, nil

	case resp.StatusCode == http.StatusUnauthorized:
		// The credential is wrong or revoked. Retrying is right -- an operator
		// can fix it and the queue should drain afterwards -- but it must be
		// loud, because nothing will move until somebody acts.
		f.log.Error("collector credential refused by the gateway", "status", resp.StatusCode)
		return "", "unauthenticated", true,
			errors.New("collector: credential refused")

	case resp.StatusCode == http.StatusServiceUnavailable:
		return "", classOf(body, "unavailable"), true,
			errors.New("collector: gateway unavailable")

	case resp.StatusCode >= 500:
		return "", classOf(body, "server_error"), true,
			fmt.Errorf("collector: gateway status %d", resp.StatusCode)

	case resp.StatusCode == http.StatusTooManyRequests:
		return "", "rate_limited", true,
			errors.New("collector: rate limited")

	default:
		// 4xx: the gateway understood and refused. The reason stored is the
		// class, never the server's message, which can quote the payload.
		c := classOf(body, "refused")
		return "", c, false,
			fmt.Errorf("collector: gateway refused with %d (%s)", resp.StatusCode, c)
	}
}

// receipt asks what happened to an idempotency key. A 404 is a real answer --
// "nothing was recorded" -- and is reported as not found rather than as an
// error, because the two lead to different next steps.
func (f *Forwarder) receipt(ctx context.Context, key string) (state string, found bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		f.base+"/memgw/v1/receipts/"+url.PathEscape(key), nil)
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, err := f.client.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode == http.StatusNotFound {
		return "", false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("collector: receipt lookup answered %d", resp.StatusCode)
	}
	var r struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.State == "" {
		return "", false, errors.New("collector: receipt lookup returned an unreadable body")
	}
	return r.State, true, nil
}

// classOf pulls the frozen error class out of a problem body. It reads only the
// class field: the message may quote the submission, and the submission is
// exactly what this package keeps encrypted.
func classOf(body []byte, fallback string) string {
	var p struct {
		Class string `json:"class"`
	}
	if err := json.Unmarshal(body, &p); err == nil && p.Class != "" {
		return p.Class
	}
	return fallback
}
