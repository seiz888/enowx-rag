package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// maxHookBytes bounds what will be read from a hook's stdin. A host that pipes
// a whole transcript into a hook -- by accident or by a later version's
// redesign -- should be refused rather than allowed to make this process grow
// until the machine notices.
const maxHookBytes = 1 << 20

// Options are the knobs the command line sets.
type Options struct {
	Host   string
	Config Config
	// Token is the gateway credential, already read from the path in the
	// configuration. It is empty when no gateway is configured.
	Token string
	// Timeout bounds the whole delivery. A hook runs inside the agent's own
	// process tree and a hook that hangs is an agent that hangs.
	Timeout time.Duration
	// DryRun translates and prints without submitting anywhere. It is how an
	// operator checks a fragment before letting it write, and it needs no
	// credential because it reaches nothing.
	DryRun bool
}

// Outcome is the adapter's answer for one hook, and what it prints.
type Outcome struct {
	Host      string    `json:"host"`
	Native    string    `json:"native_event,omitempty"`
	Lifecycle Lifecycle `json:"event"`
	// Recorded is false for a host event with no canonical meaning. It is not
	// a failure; see Translate.
	Recorded bool `json:"recorded"`
	// Key is printed so an operator debugging a duplicate can compare the key
	// two invocations derived without having to re-run anything.
	Key string `json:"idempotency_key,omitempty"`
	// Submission is filled in only by a dry run: it is what would have been
	// sent, so the check is against the actual bytes rather than against a
	// summary of them.
	Submission json.RawMessage `json:"submission,omitempty"`
	Result     *Result         `json:"result,omitempty"`
	Error      string          `json:"error,omitempty"`
}

// Run reads one hook payload, submits it, and returns the outcome.
//
// It returns an error only when the outcome is genuinely unknown -- nothing was
// written and nothing can be concluded. A refusal the sink understood comes
// back inside the Outcome, because a hook that treats "the gateway says this
// project is not yours" the same as "the network is down" will retry the first
// forever and give up on the second.
func Run(ctx context.Context, in io.Reader, opts Options) (Outcome, error) {
	raw, err := io.ReadAll(io.LimitReader(in, maxHookBytes+1))
	if err != nil {
		return Outcome{Host: opts.Host}, fmt.Errorf("adapter: the hook payload could not be read: %w", err)
	}
	if len(raw) > maxHookBytes {
		return Outcome{Host: opts.Host}, fmt.Errorf("adapter: the hook payload is larger than %d bytes", maxHookBytes)
	}

	hook, err := Decode(opts.Host, raw)
	if err != nil {
		return Outcome{Host: opts.Host}, err
	}
	out := Outcome{Host: hook.Host, Native: hook.Native, Lifecycle: hook.Lifecycle}

	// A workstation config names no fixed project: the working directory the
	// host reported decides where the event lands. Resolving here, after the
	// hook is decoded, keeps the resolution out of the idempotency key -- the
	// key is about what happened, not about where the operator pointed the
	// mapping on the day it happened.
	cfg := opts.Config
	if cfg.Resolve.MapPath != "" || cfg.Resolve.GitRemote {
		resolved, err := cfg.ResolveForCwd(hook.Cwd)
		if err != nil {
			return out, err
		}
		cfg = resolved
	}

	sub, record, err := Translate(hook, cfg)
	if err != nil {
		return out, err
	}
	if !record {
		return out, nil
	}
	out.Recorded = true
	out.Key = sub.IdempotencyKey

	if opts.DryRun {
		body, err := json.Marshal(sub)
		if err != nil {
			return out, err
		}
		out.Submission = body
		return out, nil
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	res, err := deliver(ctx, sub, opts)
	if err != nil {
		return out, err
	}
	out.Result = &res
	return out, nil
}

// SubmitHook submits a hook that the caller built in full, rather than one read
// from a host's stdin. It exists for the checkpoint producer, which authors the
// hook itself (the canonical checkpoint shape) instead of translating a host
// payload. It shares Run's delivery and refusal discipline, but skips Decode
// because there is no host payload to decode.
func SubmitHook(ctx context.Context, h Hook, opts Options) (Outcome, error) {
	if err := h.Validate(); err != nil {
		return Outcome{Host: h.Host}, err
	}
	out := Outcome{Host: h.Host, Native: h.Native, Lifecycle: h.Lifecycle}

	sub, record, err := Translate(h, opts.Config)
	if err != nil {
		return out, err
	}
	if !record {
		return out, nil
	}
	out.Recorded = true
	out.Key = sub.IdempotencyKey

	if opts.DryRun {
		body, err := json.Marshal(sub)
		if err != nil {
			return out, err
		}
		out.Submission = body
		return out, nil
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	res, err := deliver(ctx, sub, opts)
	if err != nil {
		return out, err
	}
	out.Result = &res
	return out, nil
}

// deliver chooses where the event goes.
//
// The collector first when there is one, because it is the only path that makes
// the event durable before the hook returns. If the collector is configured but
// cannot be reached, and a gateway is configured too, the event is sent
// straight to the gateway rather than dropped -- and the Result says it went
// that way, so an operator reading the hook's output can see that the local
// durable queue was not involved. Silently degrading is the thing being avoided
// here, not degrading.
func deliver(ctx context.Context, sub Submission, opts Options) (Result, error) {
	var firstErr error
	if opts.Config.Collector.Address() != "" {
		sink := NewCollectorSink(opts.Config.Collector.Address(), opts.Timeout)
		res, err := sink.Submit(ctx, sub)
		if err == nil {
			return res, nil
		}
		firstErr = err
		if opts.Config.Gateway.BaseURL == "" {
			return Result{}, fmt.Errorf("adapter: the local collector could not be reached and no gateway is configured: %w", err)
		}
	}
	if opts.Config.Gateway.BaseURL == "" {
		return Result{}, fmt.Errorf("adapter: no sink is configured")
	}
	sink, err := NewGatewaySink(opts.Config.Gateway.BaseURL, opts.Token, opts.Timeout)
	if err != nil {
		return Result{}, err
	}
	res, err := sink.Submit(ctx, sub)
	if err != nil {
		if firstErr != nil {
			return Result{}, fmt.Errorf("adapter: neither sink accepted this event (collector: %v; gateway: %w)", firstErr, err)
		}
		return Result{}, err
	}
	if firstErr != nil {
		res.Message = "the local collector was unreachable; this event went straight to the gateway and was not queued locally"
	}
	return res, nil
}

// Print writes the outcome as one JSON object. Hosts collect a hook's stdout
// into their own logs, so this is deliberately one line, machine-readable, and
// free of any payload content: the key, the class and the ids, nothing that was
// written.
func Print(w io.Writer, o Outcome) error {
	enc := json.NewEncoder(w)
	return enc.Encode(o)
}
