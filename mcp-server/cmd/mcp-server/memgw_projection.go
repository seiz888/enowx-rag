package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/enowdev/enowx-rag/pkg/memgw/outbox"
	"github.com/enowdev/enowx-rag/pkg/memgw/pgstore"
	"github.com/enowdev/enowx-rag/pkg/memgw/projection"
	"github.com/enowdev/enowx-rag/pkg/rag"
)

// runMemgwProjection drains the projection outbox into the vector store.
//
// It is a command of its own and not something the gateway does in a
// background goroutine. Two reasons, both operational rather than aesthetic:
// the projection may be stopped, rebuilt or pointed at a different store
// without taking writes offline, and a projection that shared the server's
// process would share its blast radius -- an embedder that hangs would hold
// connections the write path needs.
//
//	memgw projection run      drain until interrupted
//	memgw projection status   report the watermark, the queue and the dead letters
//	memgw projection rebuild  re-queue the projection from the events in the ledger
func runMemgwProjection(args []string) {
	fs := flag.NewFlagSet("memgw projection", flag.ExitOnError)
	qdrantURL := fs.String("qdrant", strings.TrimSpace(os.Getenv("RAG_QDRANT_URL")),
		"Qdrant REST endpoint (default $RAG_QDRANT_URL)")
	embedder := fs.String("embedder", "",
		`which embedder computes the vectors: "voyage" (the approved hosted model, needs RAG_VOYAGE_API_KEY), "tei" for a local model, "fixture" for deterministic vectors with NO semantic content (tests only)`)
	teiURL := fs.String("tei-url", strings.TrimSpace(os.Getenv("RAG_TEI_URL")), "TEI endpoint when --embedder=tei")
	voyageModel := fs.String("voyage-model", strings.TrimSpace(os.Getenv("RAG_VOYAGE_MODEL")),
		"Voyage model when --embedder=voyage (default voyage-4)")
	voyageDim := fs.Int("voyage-dim", 0, "Voyage output dimension when --embedder=voyage (0 = model default)")
	owner := fs.String("owner", defaultOwner(), "lease owner recorded on claimed rows")
	batch := fs.Int("batch", 16, "rows claimed per batch")
	lease := fs.Duration("lease", 30*time.Second, "how long a claimed row is held before another worker may take it")
	maxSensitivity := fs.String("max-sensitivity", "internal",
		"highest sensitivity class that may be embedded (public|internal|confidential|restricted)")
	project := fs.String("project", "", "limit a rebuild to one project id (default: every project)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: enowx-rag memgw projection <run|status|rebuild> [flags]

Drains the projection outbox into the vector store. The ledger is the truth and
the index is a cache of it: this process only ever reads events and writes
points, and it never modifies canonical state.

Environment: MEMGW_DSN (required), MEMGW_SCHEMA (default "memgw"), MEMGW_ENV
(development|test).

Flags:
`)
		fs.PrintDefaults()
	}
	parseArgs(fs, args)

	cmd := fs.Arg(0)
	if cmd != "run" && cmd != "status" && cmd != "rebuild" {
		fs.Usage()
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	pool, err := openMemgwPool(ctx)
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "memgw projection: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	store := outbox.NewStore(pool)
	if cmd == "rebuild" {
		if err := projectionRebuild(context.Background(), store, *project); err != nil {
			fmt.Fprintf(os.Stderr, "memgw projection: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if cmd == "status" {
		if err := projectionStatus(context.Background(), store, pool); err != nil {
			fmt.Fprintf(os.Stderr, "memgw projection: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if strings.TrimSpace(*qdrantURL) == "" {
		fmt.Fprintln(os.Stderr, "memgw projection: --qdrant (or RAG_QDRANT_URL) is not set; the worker never guesses a store")
		os.Exit(2)
	}
	embed, err := chooseEmbedder(*embedder, *teiURL, *voyageModel, *voyageDim)
	if err != nil {
		fmt.Fprintf(os.Stderr, "memgw projection: %v\n", err)
		os.Exit(2)
	}

	runCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	provider, err := rag.NewQdrantProvider(runCtx, *qdrantURL, strings.TrimSpace(os.Getenv("RAG_QDRANT_API_KEY")), embed)
	if err != nil {
		fmt.Fprintf(os.Stderr, "memgw projection: %v\n", err)
		os.Exit(1)
	}
	defer provider.Close()

	applier := projection.New(
		projection.NewPgEvents(pool),
		store,
		projection.NewQdrantIndex(provider),
		projection.Config{MaxSensitivity: *maxSensitivity},
	)
	cfg := outbox.DefaultWorkerConfig(outbox.ProjectionQdrant, *owner)
	cfg.BatchSize = *batch
	cfg.Lease = *lease

	fmt.Printf("memgw projection: draining into %s as %s (embedder %s, ceiling %s)\n",
		*qdrantURL, *owner, embedderName(embed, *embedder), *maxSensitivity)
	if err := outbox.NewWorker(store, applier, cfg).Run(runCtx); err != nil && runCtx.Err() == nil {
		fmt.Fprintf(os.Stderr, "memgw projection: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("memgw projection: stopped")
}

// chooseEmbedder refuses to pick for the operator.
//
// The default would either be the fixture -- which would silently build a
// collection with no semantic content and look exactly like a real one -- or
// TEI, which would silently start sending content to whatever is at that URL.
// Neither is a decision this command should make on somebody's behalf.
func chooseEmbedder(kind, teiURL, voyageModel string, voyageDim int) (rag.EmbeddingClient, error) {
	switch kind {
	case "fixture":
		return projection.NewFixtureEmbedder(384), nil
	case "voyage":
		// The same approved embedding path the RAG service already uses. The
		// key is read from the environment (RAG_VOYAGE_API_KEY), never from a
		// flag, so it cannot appear in a command line or the process list.
		key := strings.TrimSpace(os.Getenv("RAG_VOYAGE_API_KEY"))
		if key == "" {
			return nil, fmt.Errorf("--embedder=voyage needs RAG_VOYAGE_API_KEY in the environment")
		}
		if voyageModel == "" {
			voyageModel = "voyage-4"
		}
		return rag.NewVoyageEmbeddingClient(key, voyageModel, voyageDim), nil
	case "tei":
		if strings.TrimSpace(teiURL) == "" {
			return nil, fmt.Errorf("--embedder=tei needs --tei-url (or RAG_TEI_URL)")
		}
		return rag.NewTEIEmbeddingClient(teiURL), nil
	case "":
		return nil, fmt.Errorf("--embedder is required: \"voyage\" uses the approved hosted model, \"tei\" a local model, \"fixture\" deterministic vectors with no semantic meaning (tests only)")
	default:
		return nil, fmt.Errorf("unknown embedder %q", kind)
	}
}

// embedderName reports what will actually compute the vectors, so the startup
// line is evidence rather than an echo of the flag.
func embedderName(e rag.EmbeddingClient, fallback string) string {
	if n, ok := e.(rag.ModelNamer); ok {
		return n.ModelName()
	}
	return fallback
}

func projectionStatus(ctx context.Context, store *outbox.Store, pool *pgstore.Pool) error {
	wm, err := store.Watermark(ctx, outbox.ProjectionQdrant)
	if err != nil {
		return err
	}
	rows, err := pool.Pgx().Query(ctx, `
		SELECT projection, state, count(*), max(attempts)
		FROM projection_outbox GROUP BY projection, state ORDER BY projection, state`)
	if err != nil {
		return err
	}
	defer rows.Close()
	fmt.Printf("target: %s schema=%s\n", pgstore.RedactDSN(os.Getenv("MEMGW_DSN")), pool.Schema())
	fmt.Printf("qdrant watermark: applied through seq %d\n\n", wm)
	fmt.Printf("%-10s %-12s %8s %10s\n", "PROJECTION", "STATE", "ROWS", "MAX TRIES")
	empty := true
	for rows.Next() {
		var p, state string
		var n, attempts int
		if err := rows.Scan(&p, &state, &n, &attempts); err != nil {
			return err
		}
		empty = false
		fmt.Printf("%-10s %-12s %8d %10d\n", p, state, n, attempts)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if empty {
		fmt.Println("the outbox is empty")
	}

	// Dead letters are listed rather than counted: they are the rows that
	// stopped being retried, and "5 dead letters" is not something an operator
	// can act on.
	dead, err := store.DeadLetters(ctx, outbox.ProjectionQdrant)
	if err != nil {
		return err
	}
	if len(dead) > 0 {
		fmt.Printf("\n%d dead letter(s):\n", len(dead))
		for _, it := range dead {
			fmt.Printf("  outbox %d  event %s  seq %d  attempts %d\n", it.OutboxID, it.EventID, it.EventSeq, it.Attempts)
		}
	}
	return nil
}

func defaultOwner() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	return fmt.Sprintf("%s/%d", host, os.Getpid())
}

// projectionRebuild re-queues the projection from the ledger.
//
// It queues work; it does not do it. Nothing is written to the vector store
// here, and the worker that picks the rows up consults the tombstone journal
// before rendering anything, so a replay cannot resurrect a deleted document.
func projectionRebuild(ctx context.Context, store *outbox.Store, project string) error {
	scope := uuid.Nil
	if strings.TrimSpace(project) != "" {
		id, err := uuid.Parse(strings.TrimSpace(project))
		if err != nil {
			return fmt.Errorf("--project is not a uuid: %w", err)
		}
		scope = id
	}
	r, err := store.Rebuild(ctx, outbox.ProjectionQdrant, scope)
	if err != nil {
		return err
	}
	where := "every project"
	if scope != uuid.Nil {
		where = scope.String()
	}
	fmt.Printf("rebuild scope    %s\n", where)
	fmt.Printf("rows re-queued   %d\n", r.Requeued)
	fmt.Printf("rows restored    %d (events that had no outbox row at all)\n", r.Restored)
	fmt.Printf("watermark        %d (unchanged: it records how far the projection has been applied at least once)\n", r.Watermark)
	fmt.Println("nothing has been written to the vector store; run \"memgw projection run\" to drain the queue")
	return nil
}
