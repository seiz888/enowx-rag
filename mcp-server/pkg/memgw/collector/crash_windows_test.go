//go:build windows

package collector

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Killing a real process in the middle of a real enqueue.
//
// Everything else in this package can be tested by calling functions. This one
// cannot: the claim is that an acceptance survives the process dying without
// warning, and a Go test that closes a database politely has not tested that.
// So the test starts a second copy of the test binary, lets it enqueue as fast
// as it can, terminates it hard (TerminateProcess -- no signal, no defer, no
// flush), and then opens what is left on disk.
//
// The assertion is the promise the collector makes: every key the child was
// told was durable is on the disk and decrypts. Anything the child did not get
// an answer for may or may not be there, and that is correct -- the caller was
// never told it was safe.

const crashHelperEnv = "MEMGW_COLLECTOR_CRASH_HELPER"

// TestCrashHelperEnqueuesUntilKilled is not a test. It is the child process,
// selected by the environment variable, and it exits only when killed.
func TestCrashHelperEnqueuesUntilKilled(t *testing.T) {
	dir := os.Getenv(crashHelperEnv)
	if dir == "" {
		t.Skip("not the crash helper child")
	}
	s, err := OpenSpool(filepath.Join(dir, "spool.db"), testKey(), SpoolConfig{MaxRows: 100000})
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper: open spool:", err)
		os.Exit(3)
	}
	out := bufio.NewWriterSize(os.Stdout, 64)
	for i := 0; ; i++ {
		key := fmt.Sprintf("crash:%06d", i)
		if _, err := s.Enqueue(context.Background(), event(key, "checkpoint.written", strings.Repeat("x", 512))); err != nil {
			fmt.Fprintln(os.Stderr, "helper: enqueue:", err)
			os.Exit(4)
		}
		// Reported only after the enqueue returned, so every key the parent
		// reads is a key the child was told was durable.
		fmt.Fprintln(out, key)
		out.Flush()
	}
}

func TestAnAcceptedEventSurvivesTheProcessBeingKilled(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a process")
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=TestCrashHelperEnqueuesUntilKilled", "-test.v")
	cmd.Env = append(os.Environ(), crashHelperEnv+"="+dir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	accepted := []string{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if strings.HasPrefix(line, "crash:") {
				mu.Lock()
				accepted = append(accepted, line)
				mu.Unlock()
			}
		}
	}()

	// Wait until the child is well into the work, then kill it mid-flight. The
	// wait is on observed progress rather than a fixed sleep: a sleep that was
	// too short would kill a process that had not started writing, and the test
	// would pass having proved nothing.
	deadline := time.Now().Add(30 * time.Second)
	for {
		mu.Lock()
		n := len(accepted)
		mu.Unlock()
		if n >= 40 {
			break
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatalf("the child enqueued only %d events; it never got going", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Kill on Windows is TerminateProcess: no cleanup runs in the child, which
	// is the point.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	<-done

	mu.Lock()
	confirmed := append([]string(nil), accepted...)
	mu.Unlock()
	if len(confirmed) == 0 {
		t.Fatal("the child reported no accepted events")
	}

	// Now read what the killed process left behind.
	s, err := OpenSpool(filepath.Join(dir, "spool.db"), testKey(), SpoolConfig{})
	if err != nil {
		t.Fatalf("the spool could not be opened after the kill: %v", err)
	}
	defer s.Close()

	var integrity string
	if err := s.db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatal(err)
	}
	if integrity != "ok" {
		t.Fatalf("the database is damaged after the kill: %s", integrity)
	}

	present := map[string]bool{}
	rows, err := s.db.Query(`SELECT idempotency_key, event_id, nonce, ciphertext FROM spool`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var key, eventID string
		var nonce, ct []byte
		if err := rows.Scan(&key, &eventID, &nonce, &ct); err != nil {
			t.Fatal(err)
		}
		// Every surviving row must still decrypt and still parse. A torn write
		// would show up here as a row that exists and cannot be used, which is
		// worse than a row that is not there.
		body, err := s.env.open(aadFor(eventID, key), nonce, ct)
		if err != nil {
			t.Fatalf("row %s survived the kill but does not decrypt: %v", key, err)
		}
		var parsed map[string]any
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Fatalf("row %s decrypts to something that is not JSON: %v", key, err)
		}
		if eventID != EventID(key).String() {
			t.Fatalf("row %s carries event id %s, which is not the one its key derives", key, eventID)
		}
		present[key] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	// The promise: everything the child was told was durable is here.
	missing := []string{}
	for _, key := range confirmed {
		if !present[key] {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("%d of %d events that were accepted before the kill are not on disk (first: %s)",
			len(missing), len(confirmed), missing[0])
	}
	t.Logf("killed mid-enqueue: %d acknowledged events, %d rows on disk, all decrypt",
		len(confirmed), len(present))
}
