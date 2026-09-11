package collector

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The forwarder tests run against a stub gateway rather than the real one,
// because what is under test here is what the collector does with each kind of
// answer -- including answers a healthy gateway never gives, like a connection
// dropped after the commit. The real gateway is exercised end to end in the
// gateway package and in the collector's process-level tests.

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// stubGateway records what it was sent and answers however the test says.
type stubGateway struct {
	mu        sync.Mutex
	submits   int32
	receipts  int32
	committed map[string]string // idempotency key -> receipt state
	handler   func(g *stubGateway, w http.ResponseWriter, r *http.Request)
	srv       *httptest.Server
}

func newStub(t *testing.T, h func(g *stubGateway, w http.ResponseWriter, r *http.Request)) *stubGateway {
	g := &stubGateway{committed: map[string]string{}, handler: h}
	mux := http.NewServeMux()
	mux.HandleFunc("/memgw/v1/events", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&g.submits, 1)
		g.handler(g, w, r)
	})
	mux.HandleFunc("/memgw/v1/receipts/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&g.receipts, 1)
		key := strings.TrimPrefix(r.URL.Path, "/memgw/v1/receipts/")
		g.mu.Lock()
		state, ok := g.committed[key]
		g.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"state": state, "seq": 1})
	})
	g.srv = httptest.NewServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

func (g *stubGateway) record(r *http.Request, state string) {
	var body struct {
		IdempotencyKey string `json:"idempotency_key"`
	}
	raw, _ := io.ReadAll(r.Body)
	json.Unmarshal(raw, &body)
	g.mu.Lock()
	g.committed[body.IdempotencyKey] = state
	g.mu.Unlock()
}

func newForwarder(t *testing.T, s *Spool, base string) *Forwarder {
	t.Helper()
	f, err := NewForwarder(s, ForwarderConfig{
		BaseURL: base, Token: "memgw_test.token", Logger: quiet(),
		Client: &http.Client{Timeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestALostResponseIsReconciledRatherThanWrittenTwice(t *testing.T) {
	// The scenario: the gateway commits, and the answer never arrives. The
	// collector cannot tell that from a write that failed, and the wrong move --
	// re-sending blindly and treating a second 201 as normal -- is exactly how a
	// ledger acquires duplicate events under different ids.
	var first atomic.Bool
	g := newStub(t, func(g *stubGateway, w http.ResponseWriter, r *http.Request) {
		if first.CompareAndSwap(false, true) {
			g.record(r, "committed")
			// Commit, then sever the connection before answering.
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("the test server cannot hijack; this test cannot simulate a lost response")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
			return
		}
		t.Error("the collector re-sent an event whose receipt it could have read")
	})

	s := newSpool(t, SpoolConfig{MaxAttempts: 5, BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond})
	if _, err := s.Enqueue(t.Context(), event("agent:cp:1", "checkpoint.written", 1)); err != nil {
		t.Fatal(err)
	}
	f := newForwarder(t, s, g.srv.URL)

	// First pass: the send fails at the transport, the row goes back to pending
	// with one attempt on it.
	if _, err := f.Once(t.Context()); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Stats(t.Context())
	if st.Pending != 1 {
		t.Fatalf("after a lost response the row is not pending: %+v", st)
	}
	time.Sleep(3 * time.Millisecond)

	// Second pass: because the row has an attempt on it, the forwarder asks for
	// the receipt first and settles from it.
	if _, err := f.Once(t.Context()); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Stats(t.Context())
	if st.Sent != 1 || st.Pending != 0 {
		t.Fatalf("the reconciled row did not settle: %+v", st)
	}
	if got := atomic.LoadInt32(&g.receipts); got != 1 {
		t.Fatalf("the receipt was looked up %d times, want exactly 1", got)
	}
	if got := atomic.LoadInt32(&g.submits); got != 1 {
		t.Fatalf("the event was submitted %d times; the reconciliation did not prevent the re-send", got)
	}

	var state string
	if err := s.db.QueryRow(`SELECT receipt_state FROM spool WHERE seq = 1`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "committed" {
		t.Fatalf("the settled row records receipt state %q", state)
	}
}

func TestARefusalIsNotRetriedForever(t *testing.T) {
	g := newStub(t, func(g *stubGateway, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		// The message deliberately quotes the payload, which is what a real
		// server refusing a policy violation tends to do.
		json.NewEncoder(w).Encode(map[string]string{
			"class": "policy_rejected", "message": "rejected: " + canary,
		})
	})
	s := newSpool(t, SpoolConfig{MaxAttempts: 5, BaseBackoff: time.Millisecond})
	if _, err := s.Enqueue(t.Context(), event("a", "work.planned", canary)); err != nil {
		t.Fatal(err)
	}
	f := newForwarder(t, s, g.srv.URL)
	if _, err := f.Once(t.Context()); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Stats(t.Context())
	if st.Quarantined != 1 {
		t.Fatalf("a classified refusal was not quarantined: %+v", st)
	}
	if atomic.LoadInt32(&g.submits) != 1 {
		t.Fatal("the refusal was retried")
	}
	// And the server's message, which quoted the payload, is not on the disk.
	var class, reason string
	if err := s.db.QueryRow(`SELECT fail_class, fail_reason FROM spool WHERE seq = 1`).Scan(&class, &reason); err != nil {
		t.Fatal(err)
	}
	if class != "policy_rejected" {
		t.Fatalf("the stored class is %q", class)
	}
	if strings.Contains(reason, canary) {
		t.Fatalf("the server's message reached the disk: %q", reason)
	}
}

func TestAnUnreachableGatewayIsRetriedAndThenDeadLettered(t *testing.T) {
	g := newStub(t, func(g *stubGateway, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	s := newSpool(t, SpoolConfig{MaxAttempts: 3, BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond})
	if _, err := s.Enqueue(t.Context(), event("a", "work.planned", 1)); err != nil {
		t.Fatal(err)
	}
	f := newForwarder(t, s, g.srv.URL)
	for i := 0; i < 3; i++ {
		if _, err := f.Once(t.Context()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * time.Millisecond)
	}
	st, _ := s.Stats(t.Context())
	if st.Dead != 1 {
		t.Fatalf("a permanently failing row did not end in the dead letter state: %+v", st)
	}
	if got := atomic.LoadInt32(&g.submits); got != 3 {
		t.Fatalf("the row was attempted %d times, want the configured 3", got)
	}
}

func TestAnOfflineHostDrainsWhenTheGatewayComesBack(t *testing.T) {
	// The whole reason the collector exists: the agent kept working while the
	// gateway was down, and nothing was lost.
	var up atomic.Bool
	g := newStub(t, func(g *stubGateway, w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"class": "unknown_commit_status"})
			return
		}
		g.record(r, "committed")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"state": "committed", "seq": 1})
	})

	s := newSpool(t, SpoolConfig{MaxAttempts: 50, BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond})
	for i := 0; i < 5; i++ {
		if _, err := s.Enqueue(t.Context(), event(fmt.Sprintf("offline:%d", i), "work.planned", i)); err != nil {
			t.Fatal(err)
		}
	}
	f := newForwarder(t, s, g.srv.URL)

	// Offline: every attempt fails, and every event is still queued.
	for i := 0; i < 5; i++ {
		if _, err := f.Once(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	st, _ := s.Stats(t.Context())
	if st.Pending != 5 || st.Sent != 0 {
		t.Fatalf("events were lost while the gateway was down: %+v", st)
	}

	// Back up. Every queued event is delivered, and each is delivered once:
	// reconciliation answers the ones that already have attempts on them.
	up.Store(true)
	time.Sleep(3 * time.Millisecond)
	deadline := time.Now().Add(10 * time.Second)
	for {
		worked, err := f.Once(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		st, _ = s.Stats(t.Context())
		if st.Sent == 5 {
			break
		}
		if !worked {
			time.Sleep(2 * time.Millisecond)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the backlog did not drain: %+v", st)
		}
	}
	if st.Pending != 0 || st.Dead != 0 || st.Quarantined != 0 {
		t.Fatalf("after the drain: %+v", st)
	}
}

func TestASuccessWithAnUnreadableReceiptIsNotTreatedAsDelivered(t *testing.T) {
	// A 201 whose body cannot be parsed is not evidence of a commit this
	// collector can act on. Settling it would leave a row marked delivered on
	// the word of an answer nobody could read.
	g := newStub(t, func(g *stubGateway, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte("not json"))
	})
	s := newSpool(t, SpoolConfig{MaxAttempts: 5, BaseBackoff: time.Millisecond})
	if _, err := s.Enqueue(t.Context(), event("a", "work.planned", 1)); err != nil {
		t.Fatal(err)
	}
	f := newForwarder(t, s, g.srv.URL)
	if _, err := f.Once(t.Context()); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Stats(t.Context())
	if st.Sent != 0 || st.Pending != 1 {
		t.Fatalf("an unreadable receipt was treated as delivery: %+v", st)
	}
}
