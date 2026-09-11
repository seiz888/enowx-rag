//go:build windows || linux

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/enowdev/enowx-rag/pkg/memgw/collector"
)

// The collector command line.
//
// The two platform differences -- where the spool lives and what the local
// endpoint is called -- come from collectorDefaults, so that everything else
// here is one implementation rather than two that drift.
func runMemgwCollector(args []string) {
	def := collectorDefaults()
	fs := flag.NewFlagSet("memgw collector", flag.ExitOnError)
	dir := fs.String("dir", def.dir, "directory holding the spool and the key")
	pipe := fs.String(def.endpointFlag, collector.DefaultEndpoint, def.endpointUsage)
	gatewayURL := fs.String("gateway", "http://127.0.0.1:7777", "gateway base URL")
	tokenFile := fs.String("token-file", "", "file holding this host's memgw credential (required to run)")
	maxRows := fs.Int64("max-rows", 0, "queue limit; 0 uses the built-in default")
	reserve := fs.Int64("reserve", 0, "rows inside the limit reserved for checkpoints and tombstones")
	drain := fs.Duration("drain", 0, "how long shutdown waits for in-flight work")
	seq := fs.Int64("seq", 0, "which held row a held command acts on")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: enowx-rag memgw collector [run|stats|held|release|discard|purge] [flags]

  run      listen on the local endpoint and forward queued events to the gateway
  stats    print the queue's counts and exit
  held     list the quarantined and dead rows (metadata only, never the payload)
  release  --seq N  put a held row back in the queue with its attempts reset
  discard  --seq N  stop retrying a held row, keeping it and its key on disk
  purge    --seq N  delete a held row, freeing its idempotency key

The credential is read from --token-file and never from a flag or the
environment: a command line is readable by every process on this machine.
`)
		fs.PrintDefaults()
	}
	cmd := "run"
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	_ = fs.Parse(args)

	cfg := collector.Config{
		SpoolPath:  filepath.Join(*dir, "spool.db"),
		KeyPath:    filepath.Join(*dir, "spool.key"),
		Endpoint:   *pipe,
		GatewayURL: *gatewayURL,
		TokenFile:  *tokenFile,
		Spool: collector.SpoolConfig{
			MaxRows:     *maxRows,
			ReserveRows: *reserve,
		},
		DrainWindow: *drain,
		Logger:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}

	switch cmd {
	case "held":
		if err := collectorHeld(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "memgw collector: %v\n", err)
			os.Exit(1)
		}
	case "release", "discard", "purge":
		if *seq <= 0 {
			fmt.Fprintf(os.Stderr, "memgw collector: %s needs --seq, and the number comes from the held listing\n", cmd)
			os.Exit(2)
		}
		if err := collectorHold(cfg, cmd, *seq); err != nil {
			fmt.Fprintf(os.Stderr, "memgw collector: %v\n", err)
			os.Exit(1)
		}
	case "stats":
		if err := collectorStats(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "memgw collector: %v\n", err)
			os.Exit(1)
		}
	case "run":
		if cfg.TokenFile == "" {
			fmt.Fprintln(os.Stderr, "memgw collector: --token-file is required; the collector will not run without a credential")
			os.Exit(2)
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := collector.Run(ctx, cfg); err != nil {
			fmt.Fprintf(os.Stderr, "memgw collector: %v\n", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "memgw collector: unknown command %q\n", cmd)
		fs.Usage()
		os.Exit(2)
	}
}

// collectorStats opens the spool read-only-ish and prints the counts. It is a
// separate process from the running collector, which SQLite in WAL mode
// permits, so an operator can ask "is anything stuck" without stopping the
// thing they are asking about.
func collectorStats(cfg collector.Config) error {
	key, err := collector.LoadOrCreateKey(cfg.KeyPath)
	if err != nil {
		return err
	}
	s, err := collector.OpenSpool(cfg.SpoolPath, key, cfg.Spool)
	if err != nil {
		return err
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := s.Stats(ctx)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(st)
}

// collectorHeld prints what stopped moving. It runs beside a live collector for
// the same reason stats does.
func collectorHeld(cfg collector.Config) error {
	s, err := openSpool(cfg)
	if err != nil {
		return err
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := s.Holds(ctx, 200)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if rows == nil {
		rows = []collector.Held{}
	}
	return enc.Encode(rows)
}

// collectorHold applies one operator decision to one row.
//
// Stop the collector first. These write to the same rows the forwarder claims
// from, and while SQLite will serialise the writes, releasing a row underneath
// a running forwarder means an operator and a retry loop are both deciding what
// happens to the same event.
func collectorHold(cfg collector.Config, cmd string, seq int64) error {
	s, err := openSpool(cfg)
	if err != nil {
		return err
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	switch cmd {
	case "release":
		err = s.Release(ctx, seq)
	case "discard":
		err = s.Discard(ctx, seq)
	case "purge":
		err = s.Purge(ctx, seq)
	}
	if err != nil {
		return err
	}
	fmt.Printf("%s: row %d\n", cmd, seq)
	return nil
}

func openSpool(cfg collector.Config) (*collector.Spool, error) {
	key, err := collector.LoadOrCreateKey(cfg.KeyPath)
	if err != nil {
		return nil, err
	}
	return collector.OpenSpool(cfg.SpoolPath, key, cfg.Spool)
}
