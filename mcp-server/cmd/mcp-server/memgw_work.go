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

// runMemgwWork is the explicit work-lifecycle command: complete or abandon the
// work the working directory resolves to, or report which work that is.
//
// It exists because completion must be deliberate, never inferred. A checkpoint
// whose next_safe_action reads "done" is a model's prose, not a state change;
// promoting prose into a terminal state would silently close work the writer
// only meant to describe as nearly finished. This command is the operator's
// explicit "this unit of work is over", and the next session in the same repo
// then starts a fresh work automatically, because the ensure-work path never
// selects a terminal one.
//
//	enowx-rag memgw work complete [--reason <class>]
//	enowx-rag memgw work abandon  [--reason <class>]
//	enowx-rag memgw work current
//
// complete and abandon submit work.completed / work.abandoned against the
// work's current revision, so a transition that lost a race with another writer
// fails rather than silently closing the wrong state. current only reads.
func runMemgwWork(args []string) {
	fs := flag.NewFlagSet("memgw work", flag.ExitOnError)
	host := fs.String("host", "claude", "which host acts: "+strings.Join(adapter.Hosts, "|"))
	configPath := fs.String("config", strings.TrimSpace(os.Getenv(adapter.ConfigEnv)),
		"path to the adapter configuration (default $"+adapter.ConfigEnv+")")
	reason := fs.String("reason", "lifecycle", "reason_class to stamp on the transition")
	timeout := fs.Duration("timeout", 15*time.Second, "how long the whole delivery may take")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: enowx-rag memgw work <complete|abandon|current> [flags]

  complete   mark the current work completed (terminal)
  abandon    mark the current work abandoned  (terminal)
  current    print the work the cwd resolves to (work_id, state, revision)
`)
		fs.PrintDefaults()
	}
	parseArgs(fs, args)

	if fs.NArg() < 1 {
		fs.Usage()
		os.Exit(2)
	}
	action := fs.Arg(0)
	if action != "complete" && action != "abandon" && action != "current" {
		fmt.Fprintf(os.Stderr, "memgw work: unknown action %q\n", action)
		fs.Usage()
		os.Exit(2)
	}

	if strings.TrimSpace(*configPath) == "" {
		fmt.Fprintf(os.Stderr, "memgw work: --config (or $%s) is not set\n", adapter.ConfigEnv)
		os.Exit(2)
	}
	cfg, err := adapter.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "memgw work: %v\n", err)
		os.Exit(2)
	}
	cfg, err = cfg.ResolveForCwd(mustCwd())
	if err != nil {
		fmt.Fprintf(os.Stderr, "memgw work: %v\n", err)
		os.Exit(2)
	}

	if err := runWorkAction(context.Background(), action, *host, *reason, cfg, *timeout); err != nil {
		fmt.Fprintf(os.Stderr, "memgw work: %v\n", err)
		os.Exit(1)
	}
}

// runWorkAction performs one work-lifecycle action against the resolved target.
func runWorkAction(ctx context.Context, action, host, reason string, cfg adapter.Config, timeout time.Duration) error {
	if cfg.Gateway.BaseURL == "" || cfg.Gateway.TokenPath == "" {
		return fmt.Errorf("a work transition needs a configured gateway")
	}
	tok, err := adapter.ReadToken(cfg.Gateway.TokenPath)
	if err != nil {
		return err
	}

	ensured, err := adapter.EnsureWork(ctx, cfg, tok, timeout)
	if err != nil {
		return fmt.Errorf("could not name the current work: %w", err)
	}

	if action == "current" {
		enc := json.NewEncoder(os.Stdout)
		return enc.Encode(ensured)
	}

	// A transition advances the work aggregate, so it must carry the revision
	// the writer saw; the ensure response's revision is that view.
	eventType := "work.completed"
	if action == "abandon" {
		eventType = "work.abandoned"
	}
	rev := ensured.Revision
	workID := ensured.WorkID

	// Submit the transition as a raw event through the gateway. The adapter's
	// Translate has no work.* lifecycle, so this is built here in the same
	// envelope the gateway expects.
	sub := map[string]any{
		"event_id":          uuid.NewSHA1(uuid.NameSpaceOID, []byte(eventType+"\x00"+workID.String()+"\x00"+cfg.WorkspaceID.String())).String(),
		"idempotency_key":   "work." + action + "." + workID.String(),
		"project_id":        cfg.ProjectID.String(),
		"workspace_id":      cfg.WorkspaceID.String(),
		"work_id":           workID.String(),
		"session_id":        "work:" + action + ":" + workID.String(),
		"branch_id":         uuid.NewSHA1(uuid.Nil, []byte("work\x00"+workID.String())).String(),
		"type":              eventType,
		"payload":           map[string]any{"title": ensured.Title, "reason_class": reason},
		"expected_revision": rev,
		"sensitivity_class": cfg.Sensitivity,
		"policy_version":    "1.0.0",
		"schema_version":    "1.0.0",
		"occurred_at":       time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00"),
		"writer_epoch":      cfg.WriterEpoch,
	}

	body, err := json.Marshal(sub)
	if err != nil {
		return err
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		strings.TrimRight(cfg.Gateway.BaseURL, "/")+"/memgw/v1/events", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return fmt.Errorf("the gateway refused the transition: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	fmt.Fprintf(os.Stdout, "%s %s (state now terminal, revision %d)\n", action, workID, rev+1)
	return nil
}
