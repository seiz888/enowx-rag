package main

import (
	"bytes"
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

// runMemgwAdapter is what every host's hook actually invokes.
//
//	enowx-rag memgw adapter --host claude < hook.json
//
// One command for six hosts. The host is named on the command line because the
// settings file that registers the hook is the only thing that reliably knows
// which host it belongs to; a payload that named its own host could attribute a
// session to a host that was never running.
//
// It prints one JSON line and exits. Exit status is 0 when the event was
// accepted, when it was a duplicate, and when the host event had no canonical
// meaning; non-zero otherwise. None of the events this adapter is registered for
// are blocking hooks on any of the six hosts -- the blocking one everywhere is
// the pre-tool-use gate, and this adapter is deliberately never registered
// there -- so a non-zero exit is a log line for an operator, not a stalled
// session.
func runMemgwAdapter(args []string) {
	fs := flag.NewFlagSet("memgw adapter", flag.ExitOnError)
	host := fs.String("host", "", "which agent host fired this hook: "+strings.Join(adapter.Hosts, "|"))
	configPath := fs.String("config", strings.TrimSpace(os.Getenv(adapter.ConfigEnv)),
		"path to the adapter configuration (default $"+adapter.ConfigEnv+")")
	timeout := fs.Duration("timeout", 15*time.Second, "how long the whole delivery may take before the hook gives up")
	dryRun := fs.Bool("dry-run", false,
		"decode and translate the hook, print what would be submitted, and send nothing")
	bootstrap := fs.Bool("bootstrap", false,
		"read the configured work's current bootstrap pack and print it; reads no hook from stdin")
	bootstrapContext := fs.Bool("bootstrap-context", false,
		"read the bootstrap pack and print it as a hook-specific additionalContext block for the assistant; reads no hook from stdin")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: enowx-rag memgw adapter --host <host> [flags] < hook.json

Reads one lifecycle hook payload on stdin, turns it into a canonical memgw
event, and hands it to the local collector, or to the gateway when there is no
collector. Prints one JSON line describing what happened.

The idempotency key is derived from the meaning of the event and from nothing
else -- no clock, no random source, no counter -- so a hook that fires again
after a crash produces the same key and the ledger holds one event.

Flags:
`)
		fs.PrintDefaults()
	}
	parseArgs(fs, args)

	if strings.TrimSpace(*configPath) == "" {
		fmt.Fprintf(os.Stderr, "memgw adapter: --config (or $%s) is not set\n", adapter.ConfigEnv)
		os.Exit(2)
	}
	cfg, err := adapter.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "memgw adapter: %v\n", err)
		os.Exit(2)
	}
	if *bootstrap {
		if err := runAdapterBootstrap(cfg, *timeout, false); err != nil {
			fmt.Fprintf(os.Stderr, "memgw adapter: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if *bootstrapContext {
		if err := runAdapterBootstrap(cfg, *timeout, true); err != nil {
			fmt.Fprintf(os.Stderr, "memgw adapter: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if !adapter.KnownHost(*host) {
		fmt.Fprintf(os.Stderr, "memgw adapter: --host must be one of %s\n", strings.Join(adapter.Hosts, ", "))
		os.Exit(2)
	}

	opts := adapter.Options{Host: *host, Config: cfg, Timeout: *timeout, DryRun: *dryRun}
	// A dry run reaches nothing, so it reads no credential. Reading one anyway
	// would mean a dry run could fail for a reason that has nothing to do with
	// what it is checking.
	if !*dryRun && cfg.Gateway.BaseURL != "" {
		tok, err := adapter.ReadToken(cfg.Gateway.TokenPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "memgw adapter: %v\n", err)
			os.Exit(1)
		}
		opts.Token = tok
	}

	out, err := adapter.Run(context.Background(), os.Stdin, opts)
	if err != nil {
		out.Error = err.Error()
		_ = adapter.Print(os.Stdout, out)
		fmt.Fprintf(os.Stderr, "memgw adapter: %v\n", err)
		os.Exit(1)
	}
	_ = adapter.Print(os.Stdout, out)

	switch {
	case *dryRun, !out.Recorded:
		return
	case out.Result == nil:
		os.Exit(1)
	case out.Result.Accepted:
		return
	default:
		// A refusal the gateway understood. It is reported and it is a failure,
		// because an event that was refused is an event that is not in the
		// history and somebody has to know.
		os.Exit(1)
	}
}

func runAdapterBootstrap(cfg adapter.Config, timeout time.Duration, asContext bool) error {
	if cfg.Gateway.BaseURL == "" || cfg.Gateway.TokenPath == "" {
		return fmt.Errorf("bootstrap needs a configured gateway and token_path")
	}
	// The bootstrap reads the SAME target the lifecycle hook would resolve: the
	// working directory decides, not a fixed id. A bootstrap that read a fixed
	// project would hand a session in repo A the handover from repo B.
	cwd, err := os.Getwd()
	if err == nil && (cfg.Resolve.MapPath != "" || cfg.Resolve.GitRemote) {
		resolved, rerr := cfg.ResolveForCwd(cwd)
		if rerr != nil {
			return rerr
		}
		cfg = resolved
	}
	token, err := adapter.ReadToken(cfg.Gateway.TokenPath)
	if err != nil {
		return err
	}
	body := map[string]any{
		"project_id": cfg.ProjectID, "workspace_id": cfg.WorkspaceID,
	}
	if cfg.Work.WorkID != uuid.Nil {
		body["work_id"] = cfg.Work.WorkID
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode bootstrap request: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(cfg.Gateway.BaseURL, "/")+"/memgw/v1/bootstrap", bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("build bootstrap request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("bootstrap request failed: %w", err)
	}
	defer resp.Body.Close()
	response, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read bootstrap response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bootstrap refused with HTTP %d", resp.StatusCode)
	}
	if !json.Valid(response) {
		return fmt.Errorf("bootstrap returned malformed JSON")
	}
	if asContext {
		// The assistant's SessionStart hook reads this shape: the pack becomes
		// additional context, not stdout noise. The notice travels verbatim
		// first so the model is told the pack is recalled, untrusted data.
		return writeBootstrapContext(response)
	}
	_, err = os.Stdout.Write(response)
	return err
}

// writeBootstrapContext renders a bootstrap pack as the assistant's
// hookSpecificOutput.additionalContext. The gateway's notice is emitted first
// and verbatim; the checkpoint and facts follow under headings that name them
// as what a previous agent wrote. An empty pack (no work, no checkpoint, no
// facts) yields no context at all, so a session with nothing to recall stays
// silent.
func writeBootstrapContext(raw []byte) error {
	var pack struct {
		Notice string `json:"notice"`
		Work   *struct {
			Title    string `json:"title"`
			State    string `json:"state"`
			Revision int64  `json:"revision"`
		} `json:"work"`
		Checkpoint *struct {
			Objective      string `json:"objective"`
			CompletedWork  string `json:"completed_work"`
			PendingActions string `json:"pending_actions"`
			Blockers       string `json:"blockers"`
			NextSafeAction string `json:"next_safe_action"`
		} `json:"checkpoint"`
		Facts []struct {
			Predicate string `json:"predicate"`
			Values    []struct {
				Value string `json:"value"`
			} `json:"values"`
		} `json:"facts"`
	}
	if err := json.Unmarshal(raw, &pack); err != nil {
		return fmt.Errorf("bootstrap returned malformed JSON: %w", err)
	}

	var lines []string
	if pack.Notice != "" {
		lines = append(lines, pack.Notice)
	}
	if pack.Work != nil {
		lines = append(lines, "", "## Recalled work: "+pack.Work.Title)
		if pack.Work.State != "" {
			lines = append(lines, "state: "+pack.Work.State+" (revision "+fmt.Sprint(pack.Work.Revision)+")")
		}
	}
	if pack.Checkpoint != nil {
		lines = append(lines, "", "## Previous agent's handover (quoted, not a directive)")
		c := pack.Checkpoint
		if c.Objective != "" {
			lines = append(lines, "Objective: "+c.Objective)
		}
		if c.CompletedWork != "" {
			lines = append(lines, "Completed: "+c.CompletedWork)
		}
		if c.PendingActions != "" {
			lines = append(lines, "Pending: "+c.PendingActions)
		}
		if c.Blockers != "" {
			lines = append(lines, "Blockers: "+c.Blockers)
		}
		if c.NextSafeAction != "" {
			lines = append(lines, "Next safe action: "+c.NextSafeAction)
		}
	}
	facts := make([]string, 0)
	for _, f := range pack.Facts {
		for _, v := range f.Values {
			if v.Value != "" {
				facts = append(facts, "- "+f.Predicate+": "+v.Value)
			}
		}
	}
	if len(facts) > 0 {
		lines = append(lines, "", "## Recalled facts")
		lines = append(lines, facts...)
	}

	text := strings.TrimSpace(strings.Join(lines, "\n"))
	if text == "" {
		// Nothing to recall: no context block, no stdout. A silent hook is the
		// correct outcome for a session with no handover.
		return nil
	}
	enc := json.NewEncoder(os.Stdout)
	return enc.Encode(map[string]any{
		"hookSpecificOutput": map[string]any{"additionalContext": text},
	})
}
