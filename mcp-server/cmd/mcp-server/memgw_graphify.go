package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/enowdev/enowx-rag/pkg/memgw/graphify"
)

// runMemgwGraphify drives the repository code index.
//
//	enowx-rag memgw graphify rebuild --repo D:\PROJECTS\x --index D:\memgw-graphify\x
//	enowx-rag memgw graphify status  --repo D:\PROJECTS\x --index D:\memgw-graphify\x
//
// Two verbs, because there are two questions: build one, and is the one I have
// still true. Exit status is what a scheduled task reads: 0 when the index is
// usable, 3 when it is published but behind, and non-zero-but-not-3 when
// something is actually wrong. A rebuild that could not take the lock exits 4,
// which is a normal outcome for a task that fires while somebody is already
// rebuilding, not a failure to alert on.
func runMemgwGraphify(args []string) {
	fs := flag.NewFlagSet("memgw graphify", flag.ExitOnError)
	repo := fs.String("repo", ".", "repository to index")
	index := fs.String("index", "", "directory holding the index; must be outside the repository")
	project := fs.String("project", "", "project id this repository belongs to (uuid)")
	workspace := fs.String("workspace", "", "workspace id for this checkout (uuid)")
	keep := fs.Int("keep", 2, "how many generations to keep")
	timeout := fs.Duration("timeout", 15*time.Minute, "how long a rebuild may take")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: enowx-rag memgw graphify [rebuild|status] [flags]

  rebuild  build one generation and publish it atomically
  status   report whether the published generation still describes the repository

The index is a projection: it is rebuildable, it is never authority, and an
answer read out of it must be confirmed against live source before it is acted
on. Nothing here sends source anywhere.

Exit status: 0 usable, 3 published but stale, 4 another rebuild holds the lock.

Flags:
`)
		fs.PrintDefaults()
	}
	cmd := "status"
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	parseArgs(fs, args)

	if *index == "" {
		fmt.Fprintln(os.Stderr, "memgw graphify: --index is required; an index inside the repository would change the tree it describes")
		os.Exit(2)
	}
	cfg := graphify.Config{RepoPath: *repo, IndexDir: *index, Keep: *keep}
	if *project != "" {
		id, err := uuid.Parse(*project)
		if err != nil {
			fmt.Fprintf(os.Stderr, "memgw graphify: --project is not a uuid: %v\n", err)
			os.Exit(2)
		}
		cfg.ProjectID = id
	}
	if *workspace != "" {
		id, err := uuid.Parse(*workspace)
		if err != nil {
			fmt.Fprintf(os.Stderr, "memgw graphify: --workspace is not a uuid: %v\n", err)
			os.Exit(2)
		}
		cfg.WorkspaceID = id
	}

	c, err := graphify.New(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "memgw graphify: %v\n", err)
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")

	switch cmd {
	case "rebuild":
		m, err := c.Rebuild(ctx)
		if errors.Is(err, graphify.ErrLocked) {
			fmt.Fprintln(os.Stderr, "memgw graphify: another rebuild is running; nothing was done")
			os.Exit(4)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "memgw graphify: %v\n", err)
			os.Exit(1)
		}
		_ = enc.Encode(m)
		if m.Stale {
			os.Exit(3)
		}
	case "status":
		st, err := c.Validate(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "memgw graphify: %v\n", err)
			os.Exit(1)
		}
		_ = enc.Encode(st)
		switch {
		case !st.Published:
			os.Exit(1)
		case !st.Fresh:
			os.Exit(3)
		}
	default:
		fmt.Fprintf(os.Stderr, "memgw graphify: unknown command %q\n", cmd)
		fs.Usage()
		os.Exit(2)
	}
}
