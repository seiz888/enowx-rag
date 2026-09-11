package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"flag"

	"github.com/enowdev/enowx-rag/pkg/memgw/shadow"
)

// runMemgwShadow runs a frozen evaluation dataset and writes a report.
//
// The retrieval lane runs only against a retriever the operator named. There is
// one to name today and it is offline: BM25 over a directory of documents,
// which needs no provider, no credential and no corpus export. It is a floor,
// not the dense retriever the gateway will use, and it is reported under its
// own name so nobody can read the number as more than it is. With no
// --retriever the lane is blocked with the reason the operator gives, which is
// an honest report rather than a missing one.
func runMemgwShadow(args []string) {
	fs := flag.NewFlagSet("memgw shadow", flag.ExitOnError)
	datasetPath := fs.String("dataset", "", "frozen dataset to run")
	reportPath := fs.String("report", "", "where to write the report (default: stdout summary only)")
	blocked := fs.String("retrieval-blocked", "", "why the retrieval lane cannot run in this environment")
	retriever := fs.String("retriever", "", `retriever for the retrieval lane: "lexical" (offline BM25) or empty`)
	corpus := fs.String("corpus", "", "directory the lexical retriever indexes (required with --retriever lexical)")
	timeout := fs.Duration("timeout", 10*time.Minute, "overall deadline")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: enowx-rag memgw shadow run --dataset FILE [flags]

  run     evaluate a frozen dataset and report each lane separately
  freeze  stamp a draft dataset with its digest, making it and its thresholds
          immutable; a threshold chosen after seeing a result is not a threshold

  --dataset FILE            the frozen dataset (refused if edited since freezing)
  --report FILE             where to write the report
  --retriever NAME          "lexical" runs an offline BM25 baseline
  --corpus DIR              what the lexical retriever indexes
  --retrieval-blocked TEXT  why the retrieval lane cannot run here
  --timeout D               overall deadline (default 10m)

Shadow evaluation observes; it gates nothing. Correctness is exact and runs
anywhere. Retrieval is a measurement that needs a provider and a corpus, and a
lane that cannot run is reported as blocked -- never as zero and never as a
pass.

Exit codes: 0 pass, 5 fail, 6 incomplete (a lane could not run), 2 usage,
1 any other failure.
`)
	}
	if len(args) == 0 || len(args[0]) == 0 || args[0][0] == '-' {
		fs.Usage()
		os.Exit(2)
	}
	cmd, rest := args[0], args[1:]
	_ = fs.Parse(rest)
	if cmd == "freeze" {
		shadowFreeze(*datasetPath, *reportPath)
		return
	}
	if cmd != "run" {
		fmt.Fprintf(os.Stderr, "memgw shadow: unknown command %q\n", cmd)
		fs.Usage()
		os.Exit(2)
	}
	if strings.TrimSpace(*datasetPath) == "" {
		fmt.Fprintln(os.Stderr, "memgw shadow: --dataset is required")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	ds, err := shadow.LoadDataset(*datasetPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "memgw shadow: %v\n", err)
		os.Exit(1)
	}
	run := shadow.Runner{Dataset: ds, RetrievalBlocked: *blocked}
	switch strings.TrimSpace(*retriever) {
	case "":
	case "lexical":
		if strings.TrimSpace(*corpus) == "" {
			fmt.Fprintln(os.Stderr, "memgw shadow: --retriever lexical needs --corpus")
			os.Exit(2)
		}
		lex, err := shadow.NewLexicalRetriever(*corpus)
		if err != nil {
			fmt.Fprintf(os.Stderr, "memgw shadow: %v\n", err)
			os.Exit(1)
		}
		run.Retriever = lex
	default:
		fmt.Fprintf(os.Stderr, "memgw shadow: unknown retriever %q\n", *retriever)
		os.Exit(2)
	}
	rep, err := run.Run(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "memgw shadow: %v\n", err)
		os.Exit(1)
	}
	if strings.TrimSpace(*reportPath) != "" {
		body, err := json.MarshalIndent(rep, "", "  ")
		if err == nil {
			err = os.WriteFile(*reportPath, append(body, '\n'), 0o600)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "memgw shadow: writing the report: %v\n", err)
			os.Exit(1)
		}
	}
	printShadow(rep, *reportPath)

	switch rep.Verdict {
	case shadow.Pass:
		return
	case shadow.Fail:
		os.Exit(5)
	default:
		os.Exit(6)
	}
}

func printShadow(rep shadow.Report, reportPath string) {
	fmt.Printf("dataset       %s\n", rep.Dataset)
	fmt.Printf("digest        %s\n", rep.DatasetDigest)
	fmt.Printf("correctness   %s  %d/%d matched\n", rep.Correctness.Status, rep.Correctness.Matched, rep.Correctness.Total)
	for _, f := range rep.Correctness.Failures {
		fmt.Printf("  %s: want %s, got %s\n", f.CaseID, f.Want, f.Got)
	}
	fmt.Printf("retrieval     %s", rep.RetrievalLane.Status)
	if rep.RetrievalLane.Retriever != "" {
		fmt.Printf("  via %s", rep.RetrievalLane.Retriever)
	}
	if rep.RetrievalLane.RecallAtK != nil {
		fmt.Printf("  recall@%d %.3f over %d cases in %dms",
			rep.RetrievalLane.K, *rep.RetrievalLane.RecallAtK, rep.RetrievalLane.Total, *rep.RetrievalLane.LatencyMillis)
	}
	fmt.Println()
	if rep.RetrievalLane.Blocked != "" {
		fmt.Printf("  blocked: %s\n", rep.RetrievalLane.Blocked)
	}
	fmt.Printf("verdict       %s\n", rep.Verdict)
	fmt.Printf("              %s\n", rep.Note)
	if reportPath != "" {
		fmt.Printf("written       %s\n", reportPath)
	}
}

// shadowFreeze stamps a draft. --report names the output, because a freeze that
// overwrote its input in place would destroy the draft an author was editing.
func shadowFreeze(draft, out string) {
	if strings.TrimSpace(draft) == "" || strings.TrimSpace(out) == "" {
		fmt.Fprintln(os.Stderr, "memgw shadow freeze: --dataset (the draft) and --report (the frozen output) are both required")
		os.Exit(2)
	}
	d, err := shadow.LoadDraft(draft)
	if err != nil {
		fmt.Fprintf(os.Stderr, "memgw shadow: %v\n", err)
		os.Exit(1)
	}
	d.Freeze()
	if err := d.Write(out); err != nil {
		fmt.Fprintf(os.Stderr, "memgw shadow: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("frozen        %s\n", out)
	fmt.Printf("cases         %d\n", len(d.Cases))
	fmt.Printf("digest        %s\n", d.Digest)
}
