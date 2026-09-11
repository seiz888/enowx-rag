package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/enowdev/enowx-rag/pkg/memgw/adapter"
)

// mustCwd is the checkpoint writer's working directory. It is what the
// resolution keys on, so a checkpoint always lands in the work the writer was
// actually in, never a work guessed from somewhere else.
func mustCwd() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return cwd
}

// runMemgwCheckpoint submits a checkpoint the host authored, or reports that it
// cannot.
//
// This is the write half of the automatic handoff. A checkpoint is never
// derived from a lifecycle hook; it is authored by a capture turn -- a model
// asked, at Stop, to state the objective, what was finished, what is pending,
// what is blocking, and what is safe to do next. This command carries that
// content to the gateway through the same adapter path every other event uses,
// so it gets the same idempotency, the same durability and the same refusal
// discipline as a lifecycle event.
//
// It has two modes:
//
//	enowx-rag memgw checkpoint --submit < checkpoint.json
//	    submit a checkpoint. The JSON is the canonical checkpoint shape:
//	    { "objective", "completed_work", "pending_actions", "blockers",
//	      "next_safe_action", "modified_files", "provenance" }.
//	    provenance is "capture" (authored by a model turn) or "observed"
//	    (read from evidence); it is required and is stamped into the payload
//	    so a later reader can tell an extraction from an observation.
//
//	enowx-rag memgw checkpoint --decide < transcript.json
//	    print whether a checkpoint is warranted and why. It reads a sanitized
//	    transcript and decides; it never submits and never fabricates content.
//
// A checkpoint that cannot name its work, its objective, or its next safe
// action is refused rather than guessed, because a confident-but-wrong
// checkpoint is the most dangerous document the store can hold.
func runMemgwCheckpoint(args []string) {
	fs := flag.NewFlagSet("memgw checkpoint", flag.ExitOnError)
	host := fs.String("host", "", "which host authored this checkpoint: "+strings.Join(adapter.Hosts, "|"))
	configPath := fs.String("config", strings.TrimSpace(os.Getenv(adapter.ConfigEnv)),
		"path to the adapter configuration (default $"+adapter.ConfigEnv+")")
	submit := fs.Bool("submit", false, "read a canonical checkpoint on stdin and submit it")
	decide := fs.Bool("decide", false, "read a sanitized transcript on stdin and print whether a checkpoint is warranted")
	quiet := fs.Bool("quiet", false, "with --capture, print nothing when there is no checkpoint to record")
	capture := fs.Bool("capture", false, "read a host hook payload on stdin and record a checkpoint from the task list it carries")
	captureTurn := fs.Bool("capture-turn", false, "read a host hook payload on stdin, project the transcript, run an isolated child model turn, and submit the authored checkpoint (provenance capture)")
	timeout := fs.Duration("timeout", 15*time.Second, "how long the whole delivery may take")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: enowx-rag memgw checkpoint --submit [flags] < checkpoint.json
       enowx-rag memgw checkpoint --decide [flags] < transcript.json
       enowx-rag memgw checkpoint --capture --host <host> [flags] < hook.json

The submit payload is the canonical checkpoint shape:
  { "objective": "...", "completed_work": "...", "pending_actions": "...",
    "blockers": "...", "next_safe_action": "...", "modified_files": [...],
    "provenance": "capture" | "observed" }

provenance is required. "capture" means a model turn authored the field;
"observed" means it was read from evidence. The distinction is stamped into the
ledger payload so a reader never mistakes an extraction for an observation.
`)
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	modes := 0
	for _, on := range []bool{*submit, *decide, *capture, *captureTurn} {
		if on {
			modes++
		}
	}
	if modes != 1 {
		fmt.Fprintln(os.Stderr, "memgw checkpoint: pass exactly one of --submit, --decide, --capture or --capture-turn")
		os.Exit(2)
	}
	// --decide is pure: it reads a sanitized transcript and reports a boolean.
	// It needs no config and no network, so it branches before either.
	if *decide {
		if err := runCheckpointDecide(os.Stdin); err != nil {
			fmt.Fprintf(os.Stderr, "memgw checkpoint: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if strings.TrimSpace(*configPath) == "" {
		fmt.Fprintf(os.Stderr, "memgw checkpoint: --config (or $%s) is not set\n", adapter.ConfigEnv)
		os.Exit(2)
	}
	cfg, err := adapter.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "memgw checkpoint: %v\n", err)
		os.Exit(2)
	}

	if !adapter.KnownHost(*host) {
		fmt.Fprintf(os.Stderr, "memgw checkpoint: --host must be one of %s\n", strings.Join(adapter.Hosts, ", "))
		os.Exit(2)
	}
	if *capture {
		captureQuiet = *quiet
		// Capture fails loudly here and quietly at the host: the hook fragment
		// discards this command's exit code, so a workstation keeps working
		// while an operator reading the log still sees why nothing was written.
		if err := runCheckpointCapture(context.Background(), os.Stdin, *host, cfg, *timeout); err != nil {
			fmt.Fprintf(os.Stderr, "memgw checkpoint: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if *captureTurn {
		// The capture turn is the same shape of work: loud here, quiet at the
		// host, because a session that cannot author a handover still stops.
		if err := runCheckpointCaptureTurn(context.Background(), os.Stdin, *host, cfg, *timeout); err != nil {
			fmt.Fprintf(os.Stderr, "memgw checkpoint: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if err := runCheckpointSubmit(context.Background(), os.Stdin, *host, cfg, *timeout); err != nil {
		fmt.Fprintf(os.Stderr, "memgw checkpoint: %v\n", err)
		os.Exit(1)
	}
}

// checkpointSubmit is the payload the capture turn writes. provenance is
// required and is folded into the checkpoint payload so it reaches the ledger.
type checkpointSubmit struct {
	Objective      string   `json:"objective"`
	CompletedWork  string   `json:"completed_work"`
	PendingActions string   `json:"pending_actions"`
	Blockers       string   `json:"blockers"`
	NextSafeAction string   `json:"next_safe_action"`
	ModifiedFiles  []string `json:"modified_files"`
	Provenance     string   `json:"provenance"`
}

func runCheckpointSubmit(ctx context.Context, in io.Reader, host string, cfg adapter.Config, timeout time.Duration) error {
	raw, err := io.ReadAll(io.LimitReader(in, 1<<20))
	if err != nil {
		return fmt.Errorf("read checkpoint: %w", err)
	}
	var cp checkpointSubmit
	if err := json.Unmarshal(raw, &cp); err != nil {
		return fmt.Errorf("the checkpoint is not a JSON object: %w", err)
	}
	if cp.Provenance != "capture" && cp.Provenance != "observed" {
		return fmt.Errorf("the checkpoint must mark provenance as \"capture\" or \"observed\", got %q", cp.Provenance)
	}

	// Resolve the work from the working directory. A checkpoint with no work is
	// a note about nothing, and guessing the work would attach this handover to
	// the wrong unit. When the resolution names a project and workspace but no
	// work, the gateway is asked for the work currently open there, creating it
	// if none is: that is the "current work" path that removes the manual uuid.
	cfg, err = cfg.ResolveForCwd(mustCwd())
	if err != nil {
		return err
	}
	if cfg.Work.WorkID == uuid.Nil {
		if cfg.Gateway.BaseURL == "" {
			return fmt.Errorf("the resolved target names no work and no gateway is configured to name one")
		}
		tok, err := adapter.ReadToken(cfg.Gateway.TokenPath)
		if err != nil {
			return err
		}
		ensured, err := adapter.EnsureWork(ctx, cfg, tok, timeout)
		if err != nil {
			return fmt.Errorf("could not name the current work: %w", err)
		}
		cfg.Work = adapter.WorkConfig{WorkID: ensured.WorkID, Title: ensured.Title}
	}

	// The expected revision is what the writer must present to continue this
	// work without losing a race. It comes from the gateway's current view of
	// the work, not from memory: a checkpoint written against a stale revision
	// must lose the race it should lose.
	rev, err := readWorkRevision(ctx, cfg, timeout)
	if err != nil {
		return fmt.Errorf("could not read the work's current revision: %w", err)
	}

	hook := adapter.Hook{
		Host:      host,
		Native:    "memgw_checkpoint",
		Lifecycle: adapter.Checkpoint,
		SessionID: "checkpoint:" + host,
		Cwd:       mustCwd(),
		Checkpoint: &adapter.CheckpointInput{
			WorkID:           cfg.Work.WorkID,
			ExpectedRevision: rev,
			Objective:        cp.Objective,
			CompletedWork:    cp.CompletedWork,
			PendingActions:   cp.PendingActions,
			Blockers:         cp.Blockers,
			NextSafeAction:   cp.NextSafeAction,
			ModifiedFiles:    cp.ModifiedFiles,
			Provenance:       cp.Provenance,
		},
	}

	// Straight to the gateway, for the reason spelled out in memgw_capture.go:
	// an expected revision cannot survive sitting in a queue.
	cfg.Collector = adapter.CollectorConfig{}

	opts := adapter.Options{Host: host, Config: cfg, Timeout: timeout}
	// The token is read only when a gateway is configured, exactly as the
	// adapter command does; the credential never passes through this process's
	// logs or arguments.
	if cfg.Gateway.BaseURL != "" {
		tok, err := adapter.ReadToken(cfg.Gateway.TokenPath)
		if err != nil {
			return err
		}
		opts.Token = tok
	}
	out, err := adapter.SubmitHook(ctx, hook, opts)
	if err != nil {
		out.Error = err.Error()
		_ = adapter.Print(os.Stdout, out)
		return err
	}
	_ = adapter.Print(os.Stdout, out)
	if out.Result == nil || !out.Result.Accepted {
		return fmt.Errorf("the gateway did not accept the checkpoint")
	}
	return nil
}

// readWorkRevision fetches the work's current revision from the gateway's
// bootstrap. It is the only read the checkpoint writer needs, and it reuses the
// same endpoint the bootstrap injection already exercises.
func readWorkRevision(ctx context.Context, cfg adapter.Config, timeout time.Duration) (int64, error) {
	// Reuse the adapter's bootstrap request by reading its output; the revision
	// is in .work.revision. This avoids a second HTTP shape.
	bootCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// A minimal, self-contained request rather than reusing runAdapterBootstrap
	// (which writes to os.Stdout and mixes concerns). It reads the token from
	// the configured path and never prints it.
	if cfg.Gateway.BaseURL == "" || cfg.Gateway.TokenPath == "" {
		return 0, fmt.Errorf("reading the work revision needs a configured gateway")
	}
	tok, err := adapter.ReadToken(cfg.Gateway.TokenPath)
	if err != nil {
		return 0, err
	}
	body, _ := json.Marshal(map[string]any{
		"project_id": cfg.ProjectID, "workspace_id": cfg.WorkspaceID, "work_id": cfg.Work.WorkID,
	})
	req, err := http.NewRequestWithContext(bootCtx, http.MethodPost,
		strings.TrimRight(cfg.Gateway.BaseURL, "/")+"/memgw/v1/bootstrap", strings.NewReader(string(body)))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, err
	}
	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("bootstrap returned HTTP %d", resp.StatusCode)
	}
	var boot struct {
		Work *struct {
			Revision int64 `json:"revision"`
		} `json:"work"`
		Checkpoint *latestCheckpoint `json:"checkpoint"`
	}
	if err := json.Unmarshal(raw, &boot); err != nil {
		return 0, fmt.Errorf("bootstrap response is not JSON: %w", err)
	}
	if boot.Work == nil {
		return 0, fmt.Errorf("the gateway reports no work for this target")
	}
	lastCheckpoint = boot.Checkpoint
	return boot.Work.Revision, nil
}

// latestCheckpoint is the checkpoint already recorded for this work, as the
// bootstrap reports it. Capture compares against it so a hook that fires twice
// over an unchanged task list records nothing the second time.
type latestCheckpoint struct {
	Objective      string   `json:"objective"`
	CompletedWork  string   `json:"completed_work"`
	PendingActions string   `json:"pending_actions"`
	Blockers       string   `json:"blockers"`
	NextSafeAction string   `json:"next_safe_action"`
	ModifiedFiles  []string `json:"modified_files"`
}

// lastCheckpoint is what the most recent readWorkRevision saw. It is a package
// variable rather than a second return value because every caller reads the
// revision and only capture reads the checkpoint, and the two are one round
// trip: asking twice would let the work move between them.
var lastCheckpoint *latestCheckpoint

// runCheckpointDecide reads a sanitized transcript and reports whether a
// checkpoint is warranted. It never fabricates content: it only reports a
// boolean and a reason, so the capture turn (a model) is the only thing that
// ever writes the semantic fields.
func runCheckpointDecide(in io.Reader) error {
	raw, err := io.ReadAll(io.LimitReader(in, 16<<20))
	if err != nil {
		return fmt.Errorf("read transcript: %w", err)
	}
	var t struct {
		SessionID string `json:"session_id"`
		Lines     []any  `json:"lines"`
		Bytes     int64  `json:"bytes"`
	}
	if err := json.Unmarshal(raw, &t); err != nil {
		// A transcript that is not the expected shape is not evidence to decide
		// on; refuse rather than guess.
		return fmt.Errorf("the transcript is not the expected shape: %w", err)
	}
	warranted := len(t.Lines) > 0 || t.Bytes > 0
	out := map[string]any{
		"warranted": warranted,
		"reason":    "a capture turn must author the checkpoint; deterministic extraction is not attempted",
	}
	enc := json.NewEncoder(os.Stdout)
	return enc.Encode(out)
}
