// Tests for the adapter.
//
// Nothing in this file is taken from a real session. Every session id,
// objective and path is written here, so a fixture can never become a route by
// which real content reaches a queue, a ledger or a vector store.
package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/enowdev/enowx-rag/pkg/memgw/collector"
)

var (
	fixProject   = uuid.MustParse("7627423a-fd6e-459d-87c0-be32cd47c8cb")
	fixWorkspace = uuid.MustParse("20bc010b-9676-4d03-a6de-9812bf195f2f")
	fixWork      = uuid.MustParse("2f3f1f9a-1f0c-4a3e-9a7d-0d5a6b7c8d9e")
)

func testConfig() Config {
	return Config{
		ProjectID:   fixProject,
		WorkspaceID: fixWorkspace,
		WriterEpoch: 1,
		Sensitivity: "internal",
	}
}

func mustDecode(t *testing.T, host string, v any) Hook {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	h, err := Decode(host, raw)
	if err != nil {
		t.Fatalf("decode %s: %v", host, err)
	}
	return h
}

// ---------------------------------------------------------------------------
// The idempotency key
// ---------------------------------------------------------------------------

func TestTheSameMomentDerivesTheSameKeyTwice(t *testing.T) {
	// The whole subsystem rests on this. A hook fires, the machine dies, the
	// host restarts and fires it again: if the key moved, the ledger now holds
	// the same moment twice and nothing downstream can tell.
	h := Hook{Host: "claude", Lifecycle: SessionStart, SessionID: "s-fixture-1"}
	if Key(h) != Key(h) {
		t.Fatal("two derivations of one hook produced different keys")
	}
}

func TestTheKeyReadsNoClockAndNoRandomness(t *testing.T) {
	// Derived a hundred times with real time passing in between. Any clock,
	// counter or random source anywhere in the derivation shows up here.
	h := Hook{Host: "omp", Lifecycle: SessionCompact, SessionID: "s-fixture-2",
		Discriminator: map[string]string{"trigger": "auto"}}
	want := Key(h)
	for i := 0; i < 100; i++ {
		time.Sleep(time.Microsecond)
		if got := Key(h); got != want {
			t.Fatalf("derivation %d drifted: %s != %s", i, got, want)
		}
	}
}

func TestTheKeyDoesNotDependOnMapOrder(t *testing.T) {
	// Go randomises map iteration per process, so a derivation that hashed the
	// discriminator map directly would produce a different key in every
	// process -- a duplicate on every restart, and one that no single-process
	// test would ever catch.
	a := Hook{Host: "omp", Lifecycle: SessionCompact, SessionID: "s-fixture-3",
		Discriminator: map[string]string{"a": "1", "b": "2", "c": "3", "d": "4", "e": "5"}}
	b := Hook{Host: "omp", Lifecycle: SessionCompact, SessionID: "s-fixture-3",
		Discriminator: map[string]string{"e": "5", "d": "4", "c": "3", "b": "2", "a": "1"}}
	if Key(a) != Key(b) {
		t.Fatal("the key depends on the order the discriminator happened to be built in")
	}
}

func TestDifferentMomentsDeriveDifferentKeys(t *testing.T) {
	base := Hook{Host: "claude", Lifecycle: SessionStart, SessionID: "s-fixture-4"}
	otherHost := base
	otherHost.Host = "droid"
	otherLife := base
	otherLife.Lifecycle = SessionEnd
	otherSession := base
	otherSession.SessionID = "s-fixture-5"
	withDisc := base
	withDisc.Discriminator = map[string]string{"occurrence": "2"}

	seen := map[string]string{}
	for name, h := range map[string]Hook{
		"base": base, "other host": otherHost, "other lifecycle": otherLife,
		"other session": otherSession, "with discriminator": withDisc,
	} {
		k := Key(h)
		if prev, dup := seen[k]; dup {
			t.Fatalf("%q and %q derive the same key", name, prev)
		}
		seen[k] = name
	}
}

func TestTheKeyFitsTheContractBound(t *testing.T) {
	// 128 bytes is the frozen bound. The readable prefix carries the host name,
	// so the longest host in the list is the one that has to fit.
	for _, host := range Hosts {
		h := Hook{Host: host, Lifecycle: SessionCompact, SessionID: strings.Repeat("s", 128)}
		if n := len(Key(h)); n > 128 {
			t.Fatalf("host %s derives a %d-byte key, over the contract's 128", host, n)
		}
	}
}

func TestTwoDifferentCheckpointsAreTwoEvents(t *testing.T) {
	mk := func(objective string) Hook {
		return Hook{Host: "omp", Lifecycle: Checkpoint, SessionID: "s-fixture-6",
			Checkpoint: &CheckpointInput{
				WorkID: fixWork, ExpectedRevision: 3,
				Objective: objective, NextSafeAction: "run the suite",
			}}
	}
	if Key(mk("wire the adapter")) == Key(mk("wire the coordinator")) {
		t.Fatal("two different handover notes derive one key")
	}
	if Key(mk("wire the adapter")) != Key(mk("wire the adapter")) {
		t.Fatal("one handover note derives two keys")
	}
}

func TestAReorderedFileListIsADifferentCheckpoint(t *testing.T) {
	// The safe direction: a second event rather than a silent merge of two
	// notes that were not the same.
	mk := func(files ...string) Hook {
		return Hook{Host: "omp", Lifecycle: Checkpoint, SessionID: "s-fixture-7",
			Checkpoint: &CheckpointInput{WorkID: fixWork, ExpectedRevision: 1,
				Objective: "o", NextSafeAction: "n", ModifiedFiles: files}}
	}
	if Key(mk("a.go", "b.go")) == Key(mk("b.go", "a.go")) {
		t.Fatal("the file list is not part of the checkpoint's identity")
	}
}

// ---------------------------------------------------------------------------
// Decoding the hosts
// ---------------------------------------------------------------------------

func TestClaudeStopIsNotASessionEnding(t *testing.T) {
	// Stop fires when the assistant finishes a turn, which happens many times
	// in one session. Recording each as a session ending would make the ledger
	// claim dozens of sessions where there was one.
	h := mustDecode(t, "claude", map[string]any{
		"session_id": "s-fixture-8", "hook_event_name": "Stop", "cwd": "D:\\fixture",
	})
	if h.Lifecycle != Ignored {
		t.Fatalf("Stop became %q", h.Lifecycle)
	}
}

func TestClaudeSessionStartSourcesAreDistinguished(t *testing.T) {
	for source, want := range map[string]Lifecycle{
		"startup": SessionStart, "clear": SessionStart,
		"resume": SessionResume, "compact": SessionResume,
	} {
		h := mustDecode(t, "claude", map[string]any{
			"session_id": "s-fixture-9", "hook_event_name": "SessionStart", "source": source,
		})
		if h.Lifecycle != want {
			t.Fatalf("source %q became %q, want %q", source, h.Lifecycle, want)
		}
		if h.Lifecycle == SessionResume && h.ParentSessionID != "" {
			// A resume whose parent is itself would be refused by the ledger as
			// a resume of a session that does not exist yet.
			t.Fatalf("source %q named itself as its own parent", source)
		}
	}
}

func TestClaudePreCompactBecomesACompaction(t *testing.T) {
	h := mustDecode(t, "claude", map[string]any{
		"session_id": "s-fixture-10", "hook_event_name": "PreCompact", "trigger": "auto",
	})
	if h.Lifecycle != SessionCompact {
		t.Fatalf("PreCompact became %q", h.Lifecycle)
	}
	if h.Discriminator["trigger"] != "auto" {
		t.Fatalf("the trigger was not carried into the key: %v", h.Discriminator)
	}
}

func TestAnUnrecognisedHostEventIsIgnoredAndNotGuessed(t *testing.T) {
	h := mustDecode(t, "droid", map[string]any{
		"session_id": "s-fixture-11", "hook_event_name": "SomethingAddedInAPatchRelease",
	})
	if h.Lifecycle != Ignored {
		t.Fatalf("an unknown host event was guessed into %q", h.Lifecycle)
	}
	if h.Native != "SomethingAddedInAPatchRelease" {
		t.Fatalf("the host's own name for the event was lost: %q", h.Native)
	}
}

func TestCodexPostCompactIsNotRecordedTwice(t *testing.T) {
	// pre-compact already records the compaction. Recording post-compact too
	// would double every compaction in the history.
	pre := mustDecode(t, "codex", map[string]any{"event": "pre-compact", "session_id": "s-fixture-12"})
	post := mustDecode(t, "codex", map[string]any{"event": "post-compact", "session_id": "s-fixture-12"})
	if pre.Lifecycle != SessionCompact {
		t.Fatalf("pre-compact became %q", pre.Lifecycle)
	}
	if post.Lifecycle != Ignored {
		t.Fatalf("post-compact became %q", post.Lifecycle)
	}
}

func TestCodexAcceptsTheSpellingsItsPayloadMightUse(t *testing.T) {
	// The Codex shape was read out of the binary and has never been observed
	// firing, so the decoder accepts the plausible spellings and refuses what
	// it cannot place rather than emitting events with an empty session id.
	h := mustDecode(t, "codex", map[string]any{"event_name": "session_start", "sessionId": "s-fixture-13"})
	if h.Lifecycle != SessionStart || h.SessionID != "s-fixture-13" {
		t.Fatalf("the alternate spellings were not read: %+v", h)
	}
	if _, err := Decode("codex", []byte(`{"session_id":"s-fixture-14"}`)); err == nil {
		t.Fatal("a codex payload naming no event was accepted")
	}
}

func TestAnUnknownHostIsRefused(t *testing.T) {
	if _, err := Decode("cursor", []byte(`{}`)); err == nil {
		t.Fatal("an unknown host was accepted")
	}
}

func TestAMisinstalledFragmentIsCaught(t *testing.T) {
	// An OMP plugin wired into OpenCode's configuration. Better found at the
	// first hook than after a week of sessions attributed to the wrong host.
	raw := []byte(`{"host":"omp","event":"session_start","session_id":"s-fixture-15"}`)
	if _, err := Decode("opencode", raw); err == nil {
		t.Fatal("a payload declaring a different host was accepted")
	}
}

func TestTheCanonicalShapeRefusesUnknownFields(t *testing.T) {
	raw := []byte(`{"event":"session_start","session_id":"s-fixture-16","sesion_id":"typo"}`)
	if _, err := Decode("omp", raw); err == nil {
		t.Fatal("a canonical payload with an unknown field was accepted")
	}
}

func TestABranchWithoutADivergencePointIsRefused(t *testing.T) {
	raw := []byte(`{"event":"session_branch","session_id":"s-fixture-17","parent_session_id":"s-fixture-16"}`)
	if _, err := Decode("omp", raw); err == nil {
		t.Fatal("a branch with no branch point was accepted")
	}
}

func TestAnOversizedSessionIDIsRefusedBeforeItReachesAQueue(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"event": "session_start", "session_id": strings.Repeat("x", 129),
	})
	if _, err := Decode("omp", raw); err == nil {
		t.Fatal("a session id over the contract's bound was accepted")
	}
}

func TestACheckpointIsNeverInvented(t *testing.T) {
	// A session end carrying a checkpoint would mean the adapter had decided
	// what the session achieved. It is refused.
	raw := []byte(`{"event":"session_end","session_id":"s-fixture-18","checkpoint":{"work_id":"` +
		fixWork.String() + `","expected_revision":1,"objective":"o","next_safe_action":"n"}}`)
	if _, err := Decode("omp", raw); err == nil {
		t.Fatal("a session end carrying a checkpoint was accepted")
	}
}

func TestACheckpointMustNameItsWorkAndItsNextStep(t *testing.T) {
	for name, ck := range map[string]map[string]any{
		"no work":             {"expected_revision": 1, "objective": "o", "next_safe_action": "n"},
		"no objective":        {"work_id": fixWork.String(), "expected_revision": 1, "next_safe_action": "n"},
		"no next safe action": {"work_id": fixWork.String(), "expected_revision": 1, "objective": "o"},
	} {
		raw, _ := json.Marshal(map[string]any{
			"event": "checkpoint", "session_id": "s-fixture-19", "checkpoint": ck,
		})
		if _, err := Decode("omp", raw); err == nil {
			t.Fatalf("a checkpoint with %s was accepted", name)
		}
	}
}

// ---------------------------------------------------------------------------
// Translation
// ---------------------------------------------------------------------------

func TestThePayloadIsDeterministicEvenThoughTheEnvelopeIsNot(t *testing.T) {
	// occurred_at is read from the clock, which is fine: it is not part of any
	// derived identifier. The payload must not be, because the gateway rejects
	// a re-submission whose payload digest moved.
	h := Hook{Host: "omp", Lifecycle: SessionStart, SessionID: "s-fixture-20", ReasonClass: "startup"}
	a, ok, err := Translate(h, testConfig())
	if err != nil || !ok {
		t.Fatalf("translate: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	b, _, err := Translate(h, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Payload, b.Payload) {
		t.Fatalf("the payload moved between two translations: %s vs %s", a.Payload, b.Payload)
	}
	if a.IdempotencyKey != b.IdempotencyKey {
		t.Fatal("the key moved between two translations")
	}
	if a.OccurredAt == b.OccurredAt {
		t.Skip("the clock did not advance; nothing is proven about occurred_at here")
	}
}

func TestEveryEventInASessionLandsOnOneBranch(t *testing.T) {
	// The branch is derived, so a stateless hook fired twice an hour apart with
	// a reboot in between does not open a second branch.
	start := Hook{Host: "omp", Lifecycle: SessionStart, SessionID: "s-fixture-21"}
	end := Hook{Host: "omp", Lifecycle: SessionEnd, SessionID: "s-fixture-21"}
	a, _, _ := Translate(start, testConfig())
	b, _, _ := Translate(end, testConfig())
	if a.BranchID != b.BranchID {
		t.Fatalf("two events in one session landed on two branches: %s and %s", a.BranchID, b.BranchID)
	}
	other := Hook{Host: "omp", Lifecycle: SessionStart, SessionID: "s-fixture-22"}
	c, _, _ := Translate(other, testConfig())
	if c.BranchID == a.BranchID {
		t.Fatal("two sessions share one branch")
	}
	if BranchID("claude", "s-fixture-21") == BranchID("omp", "s-fixture-21") {
		t.Fatal("two hosts using the same session id share a branch")
	}
}

func TestABranchNamesTheBranchItLeft(t *testing.T) {
	seq := int64(41)
	h := Hook{Host: "omp", Lifecycle: SessionBranch, SessionID: "s-fixture-23",
		ParentSessionID: "s-fixture-21", BranchPointSeq: &seq}
	sub, ok, err := Translate(h, testConfig())
	if err != nil || !ok {
		t.Fatalf("translate: %v", err)
	}
	var p map[string]any
	if err := json.Unmarshal(sub.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p["parent_branch_id"] != BranchID("omp", "s-fixture-21").String() {
		t.Fatalf("the parent branch was not derived from the parent session: %v", p)
	}
	if p["branch_point_seq"].(float64) != 41 {
		t.Fatalf("the branch point was lost: %v", p)
	}
}

func TestACheckpointCarriesItsWorkAndItsExpectedRevision(t *testing.T) {
	h := Hook{Host: "omp", Lifecycle: Checkpoint, SessionID: "s-fixture-24",
		Checkpoint: &CheckpointInput{WorkID: fixWork, ExpectedRevision: 7,
			Objective: "finish section E", NextSafeAction: "run the adapter suite"}}
	sub, _, err := Translate(h, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if sub.WorkID == nil || *sub.WorkID != fixWork {
		t.Fatalf("the checkpoint lost its work: %v", sub.WorkID)
	}
	if sub.ExpectedRevision == nil || *sub.ExpectedRevision != 7 {
		t.Fatalf("the checkpoint lost its expected revision: %v", sub.ExpectedRevision)
	}
	// A nil file list would encode as null and a normalising client would
	// re-send it as [], which the ledger would refuse as a payload mismatch.
	if !strings.Contains(string(sub.Payload), `"modified_files":[]`) {
		t.Fatalf("the empty file list did not encode as an empty array: %s", sub.Payload)
	}
}

func TestACheckpointStampsItsProvenance(t *testing.T) {
	h := Hook{Host: "claude", Lifecycle: Checkpoint, SessionID: "s-fixture-provenance",
		Checkpoint: &CheckpointInput{WorkID: fixWork, ExpectedRevision: 3,
			Objective: "ship the handoff", NextSafeAction: "open OMP in the same repo",
			Provenance: "capture"}}
	sub, _, err := Translate(h, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sub.Payload), `"provenance":"capture"`) {
		t.Fatalf("the checkpoint did not stamp its provenance: %s", sub.Payload)
	}
}

func TestACheckpointWithoutProvenanceIsStillAccepted(t *testing.T) {
	// A host that predates the provenance mark must not break: provenance is
	// optional and only added to the payload when present.
	h := Hook{Host: "omp", Lifecycle: Checkpoint, SessionID: "s-fixture-noprov",
		Checkpoint: &CheckpointInput{WorkID: fixWork, ExpectedRevision: 1,
			Objective: "o", NextSafeAction: "n"}}
	sub, _, err := Translate(h, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sub.Payload), "provenance") {
		t.Fatalf("a checkpoint without provenance unexpectedly carried it: %s", sub.Payload)
	}
}

func TestAnIgnoredHookProducesNoSubmission(t *testing.T) {
	_, ok, err := Translate(Hook{Host: "claude", Lifecycle: Ignored}, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("an ignored hook produced a submission")
	}
}

func TestTheSubmissionNeverNamesItsOwnPrincipalOrEventID(t *testing.T) {
	// The identity a write is attributed to is the one the credential proved,
	// and the event id is derived from the key by whatever accepts it.
	h := Hook{Host: "omp", Lifecycle: SessionStart, SessionID: "s-fixture-25"}
	sub, _, _ := Translate(h, testConfig())
	raw, _ := json.Marshal(sub)
	var fields map[string]any
	_ = json.Unmarshal(raw, &fields)
	for _, forbidden := range []string{"principal_id", "event_id"} {
		if _, present := fields[forbidden]; present {
			t.Fatalf("the submission carries %s", forbidden)
		}
	}
}

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "adapter.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAConfigurationMayNotHoldTheToken(t *testing.T) {
	// The shape refuses it: there is no field for a token, so a configuration
	// that tries to carry one fails to parse rather than working and quietly
	// putting a credential in a file people paste into issues.
	p := writeConfig(t, `{"project_id":"`+fixProject.String()+`","workspace_id":"`+fixWorkspace.String()+
		`","writer_epoch":1,"gateway":{"base_url":"http://127.0.0.1:1","token":"memgw_secret"}}`)
	if _, err := LoadConfig(p); err == nil {
		t.Fatal("a configuration carrying a token was accepted")
	}
}

func TestAGatewayWithoutATokenPathIsRefused(t *testing.T) {
	p := writeConfig(t, `{"project_id":"`+fixProject.String()+`","workspace_id":"`+fixWorkspace.String()+
		`","writer_epoch":1,"gateway":{"base_url":"http://127.0.0.1:1"}}`)
	if _, err := LoadConfig(p); err == nil {
		t.Fatal("a gateway with no token path was accepted")
	}
}

func TestAConfigurationMustNameAWriterEpochAndASink(t *testing.T) {
	base := `"project_id":"` + fixProject.String() + `","workspace_id":"` + fixWorkspace.String() + `"`
	if _, err := LoadConfig(writeConfig(t, `{`+base+`,"collector":{"pipe":"p"}}`)); err == nil {
		t.Fatal("a configuration with no writer epoch was accepted")
	}
	if _, err := LoadConfig(writeConfig(t, `{`+base+`,"writer_epoch":1}`)); err == nil {
		t.Fatal("a configuration naming no sink was accepted")
	}
}

func TestSensitivityDefaultsToInternalAndIsChecked(t *testing.T) {
	base := `"project_id":"` + fixProject.String() + `","workspace_id":"` + fixWorkspace.String() +
		`","writer_epoch":1,"collector":{"pipe":"p"}`
	c, err := LoadConfig(writeConfig(t, `{`+base+`}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Sensitivity != "internal" {
		t.Fatalf("the default sensitivity is %q", c.Sensitivity)
	}
	if _, err := LoadConfig(writeConfig(t, `{`+base+`,"sensitivity_class":"secret"}`)); err == nil {
		t.Fatal("an unknown sensitivity class was accepted")
	}
}

// ---------------------------------------------------------------------------
// The sinks
// ---------------------------------------------------------------------------

func TestTheGatewaySinkDerivesTheEventIDFromTheKey(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Errorf("the credential did not reach the gateway: %q", r.Header.Get("Authorization"))
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"event_id":"` + got["event_id"].(string) + `","state":"committed"}`))
	}))
	defer srv.Close()

	sink, err := NewGatewaySink(srv.URL, "fixture-token", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	h := Hook{Host: "hermes", Lifecycle: SessionStart, SessionID: "s-fixture-26"}
	sub, _, _ := Translate(h, testConfig())
	res, err := sink.Submit(context.Background(), sub)
	if err != nil {
		t.Fatal(err)
	}
	want := collector.EventID(sub.IdempotencyKey).String()
	if got["event_id"] != want {
		t.Fatalf("the gateway was sent event_id %v, want the one derived from the key %s", got["event_id"], want)
	}
	if !res.Accepted || !res.Durable {
		t.Fatalf("a committed event was not reported as durable: %+v", res)
	}
}

func TestARefusalIsClassifiedAndCarriesNoServerMessage(t *testing.T) {
	// A gateway message may quote the submission back, and a hook's stderr goes
	// into the host's own logs.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"class":"scope_denied","detail":"the objective was: fixture secret text"}`))
	}))
	defer srv.Close()

	sink, _ := NewGatewaySink(srv.URL, "fixture-token", time.Second)
	sub, _, _ := Translate(Hook{Host: "hermes", Lifecycle: SessionEnd, SessionID: "s-fixture-27"}, testConfig())
	res, err := sink.Submit(context.Background(), sub)
	if err != nil {
		t.Fatalf("a refusal the gateway understood came back as an error: %v", err)
	}
	if res.Accepted || res.Durable {
		t.Fatalf("a refused event was reported as accepted: %+v", res)
	}
	if res.Class != "scope_denied" {
		t.Fatalf("the class was not read: %q", res.Class)
	}
	if strings.Contains(res.Message, "fixture secret text") {
		t.Fatalf("the server's message was carried into the hook's output: %q", res.Message)
	}
}

func TestAnUnreachableGatewayIsUnknownNotRefused(t *testing.T) {
	// "The gateway says no" and "the gateway did not answer" call for opposite
	// responses, so they must not look alike.
	sink, _ := NewGatewaySink("http://127.0.0.1:1", "fixture-token", 200*time.Millisecond)
	sub, _, _ := Translate(Hook{Host: "hermes", Lifecycle: SessionEnd, SessionID: "s-fixture-28"}, testConfig())
	if _, err := sink.Submit(context.Background(), sub); err == nil {
		t.Fatal("an unreachable gateway was reported as an answer")
	}
}

// TestBranchReasonUsesTheBranchVocabulary: the reason on a session.branched is
// not a note, it is a column with a CHECK constraint on it. A fragment that
// sends the word the host uses must come out as a word the ledger accepts, and
// a word with no honest mapping must come out empty so the ledger classifies it
// rather than the adapter guessing.
func TestBranchReasonUsesTheBranchVocabulary(t *testing.T) {
	cases := []struct{ sent, want string }{
		{"branch", "rewind"},
		{"rewind", "rewind"},
		{"fork", "fork"},
		{"new", "fork"},
		{"resume", "resume"},
		{"compact", "compaction"},
		{"COMPACTION", "compaction"},
		{"", ""},
		{"because the user asked", ""},
	}
	for _, c := range cases {
		raw := []byte(`{"host":"omp","event":"session_branch","session_id":"s2",` +
			`"parent_session_id":"s1","branch_point_seq":3,"reason_class":"` + c.sent + `"}`)
		h, err := Decode("omp", raw)
		if err != nil {
			t.Fatalf("reason %q: %v", c.sent, err)
		}
		if h.ReasonClass != c.want {
			t.Fatalf("reason %q became %q, want %q", c.sent, h.ReasonClass, c.want)
		}
		sub, ok, err := Translate(h, testConfig())
		if err != nil || !ok {
			t.Fatalf("translate %q: ok=%v err=%v", c.sent, ok, err)
		}
		var p map[string]any
		if err := json.Unmarshal(sub.Payload, &p); err != nil {
			t.Fatal(err)
		}
		got, present := p["reason_class"]
		if c.want == "" {
			if present {
				t.Fatalf("reason %q put %v in the payload; an unmappable reason must be left to the ledger", c.sent, got)
			}
			continue
		}
		if got != c.want {
			t.Fatalf("payload reason_class = %v, want %q", got, c.want)
		}
	}
}
