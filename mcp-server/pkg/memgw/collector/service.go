package collector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

// Config is everything the collector process needs.
//
// There is no field for the credential value. The token is read from a file
// whose path is given here, and never from a command line, because a command
// line is readable by every process on the machine and ends up in the parent
// shell's history. An environment variable would be better than a command line
// and still worse than a file with an ACL, so the file is the only way in.
type Config struct {
	SpoolPath string
	KeyPath   string
	// Endpoint is the local address adapters connect to: a named pipe on
	// Windows, a unix socket path on Linux. Both are local-only and both carry
	// an access control the operating system enforces, which is why neither is
	// a loopback TCP port.
	Endpoint    string
	GatewayURL  string
	TokenFile   string
	Spool       SpoolConfig
	Logger      *slog.Logger
	DrainWindow time.Duration // how long shutdown waits for in-flight work
}

func (c Config) withDefaults() Config {
	if c.Endpoint == "" {
		c.Endpoint = DefaultEndpoint
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.DrainWindow <= 0 {
		// Short on purpose. Shutdown does not need to deliver the queue -- the
		// queue is durable and will be delivered on the next start. It needs to
		// stop accepting, finish answering whoever is mid-request, and get out.
		c.DrainWindow = 5 * time.Second
	}
	return c
}

// ReadToken loads the gateway credential from a file.
//
// It refuses a file that is readable by more than its owner where that can be
// determined, and it trims whitespace, because a token pasted into a file
// almost always arrives with a trailing newline and a collector that fails on
// that is a collector that gets debugged at midnight.
func ReadToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("collector: read the credential file: %w", err)
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", fmt.Errorf("collector: the credential file %s is empty", path)
	}
	return token, nil
}

// Run starts the collector and returns when the context ends.
//
// Startup order is deliberate and is the supervised part: the key, then the
// spool, then recovery of anything left in flight, and only then the pipe. A
// collector that started listening before it could write to disk would accept
// events it cannot keep, which is the one promise it exists to make.
func Run(ctx context.Context, cfg Config) error {
	cfg = cfg.withDefaults()
	log := cfg.Logger

	key, err := LoadOrCreateKey(cfg.KeyPath)
	if err != nil {
		return err
	}
	spool, err := OpenSpool(cfg.SpoolPath, key, cfg.Spool)
	if err != nil {
		return err
	}
	defer spool.Close()
	// The key is not needed after the envelope is built. Wiping it is not a
	// defence against a determined memory reader -- Go moves memory -- but it
	// shortens the window in which a crash dump written by Windows contains it.
	for i := range key {
		key[i] = 0
	}

	recovered, err := spool.Recover(ctx)
	if err != nil {
		return err
	}
	if recovered > 0 {
		log.Info("collector recovered rows left in flight by a previous run", "rows", recovered)
	}

	token, err := ReadToken(cfg.TokenFile)
	if err != nil {
		return err
	}
	fwd, err := NewForwarder(spool, ForwarderConfig{
		BaseURL: cfg.GatewayURL, Token: token, Logger: log,
	})
	if err != nil {
		return err
	}
	token = ""

	listener, err := Listen(cfg.Endpoint)
	if err != nil {
		return err
	}

	runCtx, stop := context.WithCancel(ctx)
	defer stop()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := fwd.Run(runCtx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("collector forwarder stopped", "error", err)
		}
	}()

	accepted := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(accepted)
		var conns sync.WaitGroup
		for {
			conn, err := listener.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					conns.Wait()
					return
				}
				if runCtx.Err() != nil {
					conns.Wait()
					return
				}
				log.Error("collector accept", "error", err)
				continue
			}
			conns.Add(1)
			go func() {
				defer conns.Done()
				defer conn.Close()
				if err := Serve(runCtx, spool, conn, log); err != nil &&
					!errors.Is(err, context.Canceled) {
					log.Debug("collector connection ended", "error", err)
				}
			}()
		}
	}()

	log.Info("collector listening",
		"endpoint", listener.Name(), "spool", cfg.SpoolPath, "gateway", cfg.GatewayURL,
		"max_rows", spool.Config().MaxRows, "reserve", spool.Config().ReserveRows)

	<-ctx.Done()

	// Bounded shutdown. Stop accepting first, so nothing new arrives while the
	// queue is being left in a consistent state, then give in-flight work a
	// fixed window and stop regardless. Anything unfinished is still on disk.
	if err := listener.Close(); err != nil {
		log.Error("collector close listener", "error", err)
	}
	stop()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(cfg.DrainWindow):
		log.Warn("collector shutdown window expired with work still in flight; "+
			"the queue is durable and will be resumed on the next start",
			"window", cfg.DrainWindow)
	}
	return nil
}
