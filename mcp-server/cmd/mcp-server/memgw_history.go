package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/enowdev/enowx-rag/pkg/memgw/migrate"
	"github.com/enowdev/enowx-rag/pkg/memgw/pgstore"
)

// runMemgwHistory plans and applies the pre-gateway chunk map.
//
// It is deliberately two commands. "plan" reads and writes a file and touches
// no database; "apply" takes that file and does exactly what it says, refusing
// a plan whose digest no longer matches its contents. An operator can therefore
// produce the plan on one day, have somebody read it, and run it on another,
// knowing that what runs is what was read.
//
// The name is "history" and not "migrate" because the schema runner already
// owns that word, and because this moves no history: it records where each
// pre-gateway chunk came from so a tombstone can find it. Nothing here creates
// a fact.
func runMemgwHistory(args []string) {
	fs := flag.NewFlagSet("memgw history", flag.ExitOnError)
	source := fs.String("source", "", "newline-delimited JSON export of pre-gateway chunks")
	out := fs.String("out", "", "where to write the plan (default: stdout summary only)")
	planPath := fs.String("plan", "", "plan file to apply")
	projectID := fs.String("project-id", "", "the ledger project the corpus belongs to; without it an id is derived from the rag project name and no tombstone issued against the real project will reach these rows")
	ragProject := fs.String("rag-project", "", "read the existing mapping for this rag project from the database before planning")
	timeout := fs.Duration("timeout", 10*time.Minute, "overall deadline")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: enowx-rag memgw history <plan|apply> [flags]

  plan    classify an export and write a plan; reads nothing, writes no rows
  apply   write the mapping rows a plan calls for, and nothing else
  replay  re-apply the deletion journal to the mapping, so a row imported or
          restored after a tombstone was ordered is not readable again. Run this
          after every apply and after every restore, before anything queries the
          corpus.

  --source FILE      NDJSON export of pre-gateway chunks (plan)
  --out FILE         where to write the plan (plan)
  --project-id UUID  the ledger project the corpus belongs to (strongly advised:
                     without it the project id is derived from the rag project
                     name, and a tombstone issued against the real project will
                     not reach these rows)
  --rag-project NAME read the existing mapping from MEMGW_DSN before planning
  --plan FILE        the plan to apply (apply)
  --timeout D        overall deadline (default 10m)

Exit codes: 0 done, 2 usage, 3 the target is not provably non-production,
4 the plan is unbalanced or has been edited, 1 any other failure.

Environment: MEMGW_DSN, MEMGW_SCHEMA, MEMGW_ENV -- needed by apply, and by
plan only when --rag-project is given.
`)
	}
	// The verb comes first and the flags after it, so they have to be split
	// before parsing: Go's flag package stops at the first non-flag argument.
	if len(args) == 0 || len(args[0]) == 0 || args[0][0] == '-' {
		fs.Usage()
		os.Exit(2)
	}
	cmd, rest := args[0], args[1:]
	_ = fs.Parse(rest)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	var err error
	switch cmd {
	case "plan":
		err = historyPlan(ctx, *source, *out, *ragProject, *projectID)
	case "apply":
		err = historyApply(ctx, *planPath)
	case "replay":
		err = historyReplay(ctx, *projectID)
	default:
		fmt.Fprintf(os.Stderr, "memgw history: unknown command %q\n", cmd)
		fs.Usage()
		os.Exit(2)
	}
	if err == nil {
		return
	}
	switch {
	case errors.Is(err, pgstore.ErrUnsafeTarget):
		fmt.Fprintf(os.Stderr, "memgw history: refusing this target: %v\n", err)
		os.Exit(3)
	case errors.Is(err, errPlanUnusable):
		fmt.Fprintf(os.Stderr, "memgw history: %v\n", err)
		os.Exit(4)
	default:
		fmt.Fprintf(os.Stderr, "memgw history: %v\n", err)
		os.Exit(1)
	}
}

// errPlanUnusable marks the failures that are about the plan itself rather than
// about the machine it ran on, so a caller can tell "this plan is wrong" from
// "the database was unreachable".
var errPlanUnusable = errors.New("the plan cannot be used")

func historyPlan(ctx context.Context, source, out, ragProject, projectID string) error {
	if strings.TrimSpace(source) == "" {
		return errors.New("--source is required; the planner never guesses a corpus")
	}
	var project uuid.UUID
	if strings.TrimSpace(projectID) != "" {
		id, err := uuid.Parse(strings.TrimSpace(projectID))
		if err != nil {
			return fmt.Errorf("--project-id is not a uuid: %w", err)
		}
		project = id
	}
	known := map[string]migrate.Known{}
	if strings.TrimSpace(ragProject) != "" {
		target, closePool, err := openHistoryTarget(ctx)
		if err != nil {
			return err
		}
		defer closePool()
		known, err = target.Known(ctx, ragProject)
		if err != nil {
			return err
		}
	}

	p, err := migrate.Planner{Source: migrate.NewFileSource(source), Known: known, ProjectID: project}.Plan(ctx)
	if err != nil {
		return err
	}
	if !p.Balanced() {
		return fmt.Errorf("%w: the counts do not account for every record", errPlanUnusable)
	}
	if strings.TrimSpace(out) != "" {
		if err := p.Write(out); err != nil {
			return err
		}
	}
	printPlan(p, len(known), out)
	return nil
}

func printPlan(p migrate.Plan, known int, out string) {
	fmt.Printf("source        %s\n", p.Source)
	fmt.Printf("plan version  %s\n", p.PlanVersion)
	fmt.Printf("namespace     %s\n", p.Namespace)
	fmt.Printf("project       %s (%s)\n", p.ProjectID, p.ProjectIDSource)
	fmt.Printf("known rows    %d\n", known)
	fmt.Printf("total         %d\n", p.Total)
	for _, d := range []migrate.Decision{migrate.Imported, migrate.Quarantined, migrate.Rejected, migrate.Skipped} {
		fmt.Printf("  %-12s %d\n", d, p.Counts[d])
	}
	fmt.Printf("digest        %s\n", p.Digest)

	// Held records are grouped by reason rather than listed one by one: the
	// list an operator acts on is "forty chunks matched a credential pattern",
	// and the ids for that are in the plan file.
	held := map[string]int{}
	var candidates int
	for _, e := range p.Entries {
		if e.Decision == migrate.Quarantined || e.Decision == migrate.Rejected {
			held[e.Reason]++
		}
		if e.Candidate {
			candidates++
		}
	}
	if len(held) > 0 {
		reasons := make([]string, 0, len(held))
		for r := range held {
			reasons = append(reasons, r)
		}
		sort.Strings(reasons)
		fmt.Println("held back")
		for _, r := range reasons {
			fmt.Printf("  %4d  %s\n", held[r], r)
		}
	}
	if candidates > 0 {
		fmt.Printf("candidates    %d records read like durable claims and are listed for review; this command promotes nothing\n", candidates)
	}
	if out != "" {
		fmt.Printf("written       %s\n", out)
	}
}

func historyApply(ctx context.Context, planPath string) error {
	if strings.TrimSpace(planPath) == "" {
		return errors.New("--plan is required")
	}
	p, err := migrate.ReadPlan(planPath)
	if err != nil {
		return fmt.Errorf("%w: %v", errPlanUnusable, err)
	}
	if !p.Balanced() {
		return fmt.Errorf("%w: the counts do not account for every record", errPlanUnusable)
	}

	target, closePool, err := openHistoryTarget(ctx)
	if err != nil {
		return err
	}
	defer closePool()

	a, err := target.Apply(ctx, p)
	if err != nil {
		return err
	}
	fmt.Printf("plan          %s\n", planPath)
	fmt.Printf("digest        %s\n", p.Digest)
	fmt.Printf("inserted      %d\n", a.Inserted)
	fmt.Printf("already there %d\n", a.AlreadyPresent)
	fmt.Printf("not an import %d\n", a.Skipped)
	if len(a.Conflicted) > 0 {
		// Nothing was overwritten. These are named in full because each one is
		// a disagreement between the plan and the table that a person has to
		// settle before the next run.
		fmt.Printf("conflicted    %d (nothing was overwritten)\n", len(a.Conflicted))
		for _, c := range a.Conflicted {
			fmt.Printf("  %s\n", c)
		}
	}
	return nil
}

// historyReplay repairs the mapping from the deletion journal.
//
// Without --project-id it covers every project, which is what a restore wants:
// the operator repairing a freshly restored database does not yet know which
// projects were behind.
func historyReplay(ctx context.Context, projectID string) error {
	var project uuid.UUID
	if strings.TrimSpace(projectID) != "" {
		id, err := uuid.Parse(strings.TrimSpace(projectID))
		if err != nil {
			return fmt.Errorf("--project-id is not a uuid: %w", err)
		}
		project = id
	}
	target, closePool, err := openHistoryTarget(ctx)
	if err != nil {
		return err
	}
	defer closePool()

	r, err := target.Replay(ctx, project)
	if err != nil {
		return err
	}
	scope := "every project"
	if project != uuid.Nil {
		scope = project.String()
	}
	fmt.Printf("scope         %s\n", scope)
	fmt.Printf("tombstones    %d\n", r.Tombstones)
	fmt.Printf("newly marked  %d\n", r.Marked)
	if r.Marked == 0 {
		fmt.Println("nothing was readable that a tombstone had already reached")
	}
	return nil
}

// openHistoryTarget opens the pool and refuses a production target. The refusal
// lives in NewPGTarget, which is the only way to build a target at all.
func openHistoryTarget(ctx context.Context) (*migrate.PGTarget, func(), error) {
	pool, err := openMemgwPool(ctx)
	if err != nil {
		return nil, nil, err
	}
	target, err := migrate.NewPGTarget(ctx, pool)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	return target, pool.Close, nil
}
