package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/enowdev/enowx-rag/pkg/memgw/collector"
)

// The adapter against a real collector.
//
// Not a stand-in for the protocol: the actual collector.Serve loop, over a real
// connection, writing into a real encrypted spool on disk. What it proves is
// that the two halves agree -- that a submission this package builds is one the
// collector accepts, and that the event id the collector derives is the one the
// gateway sink would have derived from the same key.
//
// The transport is net.Pipe rather than a named pipe, so this runs on every
// platform. The named pipe itself has its own tests in the collector package;
// what is being tested here is the conversation, not the socket.
func serveOne(t *testing.T, s *collector.Spool) net.Conn {
	t.Helper()
	client, server := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = collector.Serve(ctx, s, server, slog.New(slog.DiscardHandler))
		_ = server.Close()
	}()
	t.Cleanup(func() {
		cancel()
		_ = client.Close()
		<-done
	})
	return client
}

func openSpool(t *testing.T) *collector.Spool {
	t.Helper()
	key := bytes.Repeat([]byte{0x2b}, collector.KeyLen)
	s, err := collector.OpenSpool(filepath.Join(t.TempDir(), "spool.db"), key, collector.SpoolConfig{})
	if err != nil {
		t.Fatalf("open the spool: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestTheCollectorAcceptsWhatTheAdapterBuilds(t *testing.T) {
	s := openSpool(t)
	conn := serveOne(t, s)
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	h := Hook{Host: "claude", Lifecycle: SessionStart, SessionID: "s-fixture-30", ReasonClass: "startup"}
	sub, ok, err := Translate(h, testConfig())
	if err != nil || !ok {
		t.Fatalf("translate: %v", err)
	}
	res, err := submitOverPipe(conn, sub)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if !res.Accepted || !res.Durable {
		t.Fatalf("the collector did not report the event as durable: %+v", res)
	}
	// The id the collector derived and the id the gateway sink would derive are
	// the same id, so an event that took one path once and the other after a
	// configuration change is still one event.
	if res.EventID != collector.EventID(sub.IdempotencyKey).String() {
		t.Fatalf("the collector derived %s, the gateway path would derive %s",
			res.EventID, collector.EventID(sub.IdempotencyKey))
	}

	st, err := s.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Pending != 1 {
		t.Fatalf("the spool holds %d pending rows", st.Pending)
	}
}

func TestASecondFireOfTheSameHookIsRecognisedAsTheSameEvent(t *testing.T) {
	// The crash case, spelled out: the hook fires, the event reaches the spool,
	// the process dies before the host recorded that it succeeded, the host
	// restarts and fires the hook again.
	s := openSpool(t)
	h := Hook{Host: "claude", Lifecycle: SessionCompact, SessionID: "s-fixture-31",
		Discriminator: map[string]string{"trigger": "auto"}}

	var ids []string
	for i := 0; i < 2; i++ {
		// A new connection each time, as a new process would have.
		conn := serveOne(t, s)
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		sub, _, err := Translate(h, testConfig())
		if err != nil {
			t.Fatal(err)
		}
		res, err := submitOverPipe(conn, sub)
		if err != nil {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
		if !res.Accepted {
			t.Fatalf("attempt %d was refused: %+v", i+1, res)
		}
		if i == 1 && !res.Duplicate {
			t.Fatalf("the second fire was not recognised as a duplicate: %+v", res)
		}
		ids = append(ids, res.EventID)
	}
	if ids[0] != ids[1] {
		t.Fatalf("two fires of one hook produced two event ids: %s and %s", ids[0], ids[1])
	}
	st, _ := s.Stats(context.Background())
	if st.Pending != 1 {
		t.Fatalf("two fires of one hook left %d rows in the spool", st.Pending)
	}
}

func TestTheAdapterSendsNoEventIDOverThePipe(t *testing.T) {
	// The collector refuses a client-chosen event id, because an id the client
	// chose is an id that changes when the client restarts. This asserts the
	// adapter does not send one rather than relying on the refusal.
	client, server := net.Pipe()
	defer client.Close()
	done := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 1<<16)
		n, _ := server.Read(buf)
		_, _ = server.Write([]byte(`{"ok":true,"event_id":"x","durable":true}` + "\n"))
		done <- append([]byte(nil), buf[:n]...)
		_ = server.Close()
	}()
	_ = client.SetDeadline(time.Now().Add(10 * time.Second))

	sub, _, _ := Translate(Hook{Host: "omp", Lifecycle: SessionEnd, SessionID: "s-fixture-32"}, testConfig())
	if _, err := submitOverPipe(client, sub); err != nil && err != io.EOF {
		t.Fatalf("submit: %v", err)
	}
	var req struct {
		Op             string          `json:"op"`
		IdempotencyKey string          `json:"idempotency_key"`
		Event          json.RawMessage `json:"event"`
	}
	if err := json.Unmarshal(<-done, &req); err != nil {
		t.Fatal(err)
	}
	if req.Op != "submit" || req.IdempotencyKey != sub.IdempotencyKey {
		t.Fatalf("the request did not carry the key: %+v", req)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(req.Event, &fields); err != nil {
		t.Fatal(err)
	}
	if _, present := fields["event_id"]; present {
		t.Fatal("the adapter sent an event_id the collector would have refused")
	}
	if _, present := fields["principal_id"]; present {
		t.Fatal("the adapter named its own principal")
	}
}
