package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enowdev/enowx-rag/pkg/memgw/adapter"
)

// writeTranscript writes a temporary transcript JSONL and returns its path.
func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "transcript.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return p
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func TestSanitizeTranscriptKeepsProseDropsTools(t *testing.T) {
	user := mustJSON(t, map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": "Deploy the service",
		},
	})
	toolUse := mustJSON(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role": "assistant",
			"content": []map[string]any{
				{"type": "tool_use", "name": "Bash", "input": map[string]any{"command": "curl -H 'Authorization: Bearer SECRET_TOKEN_VALUE_12345' https://x"}},
			},
		},
	})
	prose := mustJSON(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role": "assistant",
			"content": []map[string]any{
				{"type": "text", "text": "Deployed successfully."},
			},
		},
	})
	toolResult := mustJSON(t, map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": []map[string]any{{"type": "tool_result", "tool_use_id": "x", "content": "password=hunter2"}},
		},
	})

	p := writeTranscript(t, user, toolUse, prose, toolResult)
	got, err := sanitizeTranscript(p)
	if err != nil {
		t.Fatalf("sanitizeTranscript: %v", err)
	}

	if !strings.Contains(got, "Deploy the service") {
		t.Errorf("projection missing user prompt: %q", got)
	}
	if !strings.Contains(got, "Deployed successfully.") {
		t.Errorf("projection missing assistant prose: %q", got)
	}
	if strings.Contains(got, "SECRET_TOKEN_VALUE_12345") {
		t.Errorf("projection leaked tool_use input: %q", got)
	}
	if strings.Contains(got, "hunter2") {
		t.Errorf("projection leaked tool_result content: %q", got)
	}
}

func TestSanitizeTranscriptMissingFile(t *testing.T) {
	if _, err := sanitizeTranscript(filepath.Join(t.TempDir(), "nope.jsonl")); err == nil {
		t.Fatal("expected error for missing transcript")
	}
}

func TestSanitizeTranscriptMalformedLines(t *testing.T) {
	// A malformed line must be skipped, not fail the whole read.
	p := writeTranscript(t, "{not json", `{"type":"user","message":{"role":"user","content":"ok"}}`)
	got, err := sanitizeTranscript(p)
	if err != nil {
		t.Fatalf("sanitizeTranscript: %v", err)
	}
	if !strings.Contains(got, "ok") {
		t.Errorf("projection should keep the valid line: %q", got)
	}
}

func TestRedactProjectionCoversSecretShapes(t *testing.T) {
	cases := []string{
		"sk-abcdefghijklmno",
		"ghp_0123456789abcdef",
		"AKIAIOSFODNN7EXAMPLE",
		"xoxb-123456789012-abcdefghijkl",
		"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9",
		"-----BEGIN RSA PRIVATE KEY-----",
		"Bearer abcdefghijklmnopqrstuv",
		"0123456789abcdef0123456789abcdef",
	}
	for _, secret := range cases {
		in := "do the thing with " + secret
		got := redactProjection(in)
		if strings.Contains(got, secret) {
			t.Errorf("redactProjection(%q) leaked %q", in, secret)
		}
		if !strings.Contains(got, "[redacted]") {
			t.Errorf("redactProjection(%q) did not mark redaction: %q", in, got)
		}
	}
}

func TestRedactProjectionLeavesOrdinaryText(t *testing.T) {
	in := "fix the flaky test in pkg/core/service.go"
	got := redactProjection(in)
	if got != in {
		t.Errorf("ordinary prose was altered: %q", got)
	}
}

func TestValidateCaptureTurn(t *testing.T) {
	good := captureTurnSubmit{
		Objective: "x", NextSafeAction: "y",
		CompletedWork: "a", PendingActions: "b", Blockers: "", ModifiedFiles: []string{"f.go"},
	}
	if err := validateCaptureTurn(good); err != nil {
		t.Fatalf("valid checkpoint refused: %v", err)
	}

	noObjective := good
	noObjective.Objective = "  "
	if err := validateCaptureTurn(noObjective); err == nil {
		t.Error("missing objective was accepted")
	}

	noNext := good
	noNext.NextSafeAction = ""
	if err := validateCaptureTurn(noNext); err == nil {
		t.Error("missing next_safe_action was accepted")
	}

	tooLong := good
	tooLong.Objective = strings.Repeat("a", 4097)
	if err := validateCaptureTurn(tooLong); err == nil {
		t.Error("over-length field was accepted")
	}

	tooManyFiles := good
	tooManyFiles.ModifiedFiles = make([]string, 201)
	if err := validateCaptureTurn(tooManyFiles); err == nil {
		t.Error("over-many modified_files was accepted")
	}
}

func TestRecursionGuardRefusesChild(t *testing.T) {
	t.Setenv(captureTurnRecursionGuardEnv, "1")
	defer os.Unsetenv(captureTurnRecursionGuardEnv)

	err := runCheckpointCaptureTurn(nil, strings.NewReader("{}"), "claude", adapter.Config{}, 0)
	if err == nil || !strings.Contains(err.Error(), "already a capture turn") {
		t.Fatalf("expected recursion refusal, got %v", err)
	}
}

func TestCaptureTurnRequiresTranscriptPath(t *testing.T) {
	// No transcript_path in the hook payload -> refused before any model call.
	in := `{"session_id":"s","hook_event_name":"Stop","cwd":"D:\\PROJECTS\\enowx-rag"}`
	err := runCheckpointCaptureTurn(nil, strings.NewReader(in), "claude", adapter.Config{}, 0)
	if err == nil || !strings.Contains(err.Error(), "transcript_path") {
		t.Fatalf("expected transcript_path refusal, got %v", err)
	}
}

func TestCaptureTurnWarranted(t *testing.T) {
	// First run needs captureTurnFirstBytes.
	ok, g := captureTurnWarranted(captureTurnGate{}, 100)
	if ok {
		t.Fatal("tiny transcript was warranted")
	}
	if g.Runs != 0 {
		t.Fatalf("gate mutated on refusal: %+v", g)
	}

	ok, g = captureTurnWarranted(captureTurnGate{}, captureTurnFirstBytes)
	if !ok {
		t.Fatal("threshold transcript was not warranted")
	}
	if g.Runs != 1 || g.Offset != captureTurnFirstBytes {
		t.Fatalf("gate not advanced: %+v", g)
	}

	// Second run needs captureTurnRearmBytes of NEW content.
	ok, _ = captureTurnWarranted(g, captureTurnFirstBytes+10)
	if ok {
		t.Fatal("second turn ran without enough new content")
	}

	ok, g = captureTurnWarranted(g, captureTurnFirstBytes+captureTurnRearmBytes)
	if !ok || g.Runs != 2 {
		t.Fatalf("second turn with enough content not warranted: %+v", g)
	}

	// Cap at captureTurnMaxRuns.
	g.Runs = captureTurnMaxRuns
	ok, _ = captureTurnWarranted(g, 1<<30)
	if ok {
		t.Fatal("run cap was exceeded")
	}
}

func TestCaptureTurnGatePathSanitizesSessionID(t *testing.T) {
	p := captureTurnGatePath("../../etc/passwd")
	base := filepath.Base(p)
	if strings.Contains(base, "..") || strings.Contains(base, "/") || strings.Contains(base, "\\") {
		t.Fatalf("unsafe gate basename: %q", base)
	}
	if base != "turn-etcpasswd.json" {
		t.Fatalf("unexpected sanitized basename: %q", base)
	}
}
