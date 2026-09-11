package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/enowdev/enowx-rag/pkg/memgw/gateway"
)

// The gateway, served on its own.
//
// `enowx-rag --serve` mounts the gateway inside the whole enowx-rag service,
// which also builds a vector store and an embedder. That is right for the
// deployment where both exist. It is wrong for a workstation that wants the
// ledger and nothing else: there, a missing Qdrant URL or embedding key stops
// the process before a single memgw route is mounted, and the memory gateway
// becomes unavailable because of a dependency it does not use.
//
// So this serves exactly the routes in pkg/memgw/gateway and nothing more. It
// shares openMemgwGateway with the full service, which means production is
// verified here in the same way and by the same code: same assertion, same
// transport rule, same refusal.
func runMemgwServe(args []string) {
	fs := flag.NewFlagSet("memgw serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:7791", "listen address; loopback by default because the gateway is a local service")
	_ = fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	handler, closePool, err := openMemgwGateway(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "memgw: %v\n", err)
		os.Exit(1)
	}
	defer closePool()
	if handler == nil {
		fmt.Fprintln(os.Stderr, "memgw: MEMGW_DSN is not set; there is no ledger to serve")
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.Handle(gateway.MountPath+"/", http.StripPrefix(gateway.MountPath, handler))

	srv := &http.Server{
		Addr:    *addr,
		Handler: mux,
		// Every route answers in one response, so a write deadline is safe
		// here in a way it is not for the streaming /mcp transport.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	fmt.Fprintf(os.Stderr, "memgw gateway listening on http://%s%s (per-principal credential)\n", *addr, gateway.MountPath)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "memgw: %v\n", err)
		os.Exit(1)
	}
}
