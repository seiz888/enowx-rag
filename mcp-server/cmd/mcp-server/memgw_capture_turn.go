package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/enowdev/enowx-rag/pkg/memgw/adapter"
)

// Private capture turn.
//
// The task-list capture (memgw_capture.go) only fires when the host exposes a
// task tool, and the headless Claude CLI exposes none. A session that never
// touches a todo tool would otherwise leave no handover at all. This mode
// closes that gap the only honest way: it asks a model to author the handover
// from the session's own transcript, as a bounded, isolated, private turn.
//
// The transcript is NEVER sent to RAG, Voyage, or any log. It is read locally,
// reduced to a sanitized projection -- user prompts and the assistant's final
// prose, with tool inputs/results stripped because that is where credentials
// live -- and that projection alone is handed to a child `claude` invocation
// whose hooks, plugins, LSP and MCP are all disabled. The child is asked for
// one thing: the canonical checkpoint JSON, against a strict schema, with no
// tools and no session persistence. Its output is validated and submitted with
// provenance "capture".
//
// The child is a separate process and a separate turn; it never re-enters the
// parent's Stop hook. Recursion is additionally guarded by an environment
// marker (MEMGW_CAPTURE_TURN) and by --bare + --no-session-persistence, so a
// capture turn that itself produced a Stop hook would be refused before it
// could spawn another capture turn.
//
// Failure is always visible but never fatal to the session: this command exits
// non-zero with a reason on stderr, and the host hook discards the exit code,
// so a session whose capture turn could not run still stops normally.

// captureTurnRecursionGuardEnv marks the child process. The child's own hook
// registration (if any) keys off this to refuse to run another capture turn.
const captureTurnRecursionGuardEnv = "MEMGW_CAPTURE_TURN_CHILD"

// captureTurnModel is the model the child turn uses. It is an alias, so it
// tracks the operator's current model without pinning a dated name.
const captureTurnDefaultModel = "sonnet"

// captureTurnMaxChars is the hard ceiling on the sanitized projection. The
// projection is what leaves this process; everything else is read and dropped.
const captureTurnMaxChars = 24000

// captureTurnMaxBudgetUSD caps what the child turn may spend. It is a guard
// against a runaway authoring turn, not a cost target.
const captureTurnMaxBudgetUSD = "0.50"

// captureTurnFirstBytes is how much transcript must exist before the first
// capture turn is worth a model call. Below this the session is too thin for a
// meaningful handover and the turn is skipped.
const captureTurnFirstBytes = 2000

// captureTurnRearmBytes is how much new transcript must accrue before a second
// turn re-runs. It prevents a model call on every Stop of a long session.
const captureTurnRearmBytes = 4000

// captureTurnMaxRuns caps the number of capture turns one session may spend.
const captureTurnMaxRuns = 3

// captureTurnGate is the per-session gate state: how many turns have run and
// the transcript size the last one saw. It holds no transcript content.
type captureTurnGate struct {
	Runs   int   `json:"runs"`
	Offset int64 `json:"offset"`
}

func captureTurnGatePath(sessionID string) string {
	root := os.Getenv("MEMGW_ADAPTER_STATE")
	if root == "" {
		root = filepath.Join(os.TempDir(), "memgw-capture")
	}
	_ = os.MkdirAll(root, 0o700)
	safe := make([]rune, 0, 64)
	for _, r := range sessionID {
		if len(safe) == 64 {
			break
		}
		if r == '-' || r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			safe = append(safe, r)
		}
	}
	return filepath.Join(root, "turn-"+string(safe)+".json")
}

func loadCaptureTurnGate(path string) captureTurnGate {
	var g captureTurnGate
	raw, err := os.ReadFile(path)
	if err != nil {
		return g
	}
	_ = json.Unmarshal(raw, &g)
	return g
}

func saveCaptureTurnGate(path string, g captureTurnGate) {
	raw, err := json.Marshal(g)
	if err != nil {
		return
	}
	_ = os.WriteFile(path, raw, 0o600)
}

// captureTurnWarranted decides whether a capture turn is worth a model call
// for this session now. It never reads transcript content.
func captureTurnWarranted(gate captureTurnGate, transcriptSize int64) (bool, captureTurnGate) {
	if gate.Runs >= captureTurnMaxRuns {
		return false, gate
	}
	need := int64(captureTurnFirstBytes)
	if gate.Runs > 0 {
		need = captureTurnRearmBytes
	}
	if transcriptSize-gate.Offset < need {
		return false, gate
	}
	gate.Runs++
	gate.Offset = transcriptSize
	return true, gate
}

// captureTurnSchema is the strict JSON schema the child must satisfy. Every
// field is required so a partial answer is refused, not guessed around.
const captureTurnSchema = `{"type":"object","additionalProperties":false,"required":["objective","completed_work","pending_actions","blockers","next_safe_action","modified_files"],"properties":{"objective":{"type":"string"},"completed_work":{"type":"string"},"pending_actions":{"type":"string"},"blockers":{"type":"string"},"next_safe_action":{"type":"string"},"modified_files":{"type":"array","items":{"type":"string"}}}}`

// captureTurnPrompt is the single instruction the child turn receives. It
// deliberately asks for the semantic fields the ledger contract requires and
// nothing else, and forbids quoting transcript content verbatim.
const captureTurnPrompt = `You are authoring a structured handover note for the next agent working in this repository. Read the sanitized session summary below (user prompts and the assistant's own final prose; tool inputs and outputs were stripped for safety). Produce ONLY the JSON object described, nothing else.

State:
- "objective": one line, what this session was trying to do.
- "completed_work": a short markdown-bulleted list of what was finished.
- "pending_actions": a short markdown-bulleted list of what remains.
- "blockers": any obstacle that stopped progress (empty string if none).
- "next_safe_action": one line, the single safest next step for a fresh agent.
- "modified_files": array of repository paths this session changed (empty array if none).

Rules:
- Never reproduce a credential, token, key, password, or secret of any kind. If the summary contains one, do not repeat it; describe the work without it.
- Keep every field under 2000 characters.
- If the summary is too thin to be sure, say what you can and leave uncertain fields as empty strings rather than inventing.
- Do not add commentary outside the JSON.

SANITIZED SESSION SUMMARY:
%s`

// captureTurnSubmit is the child's strict-schema output, validated before it is
// handed to the checkpoint submit path.
type captureTurnSubmit struct {
	Objective      string   `json:"objective"`
	CompletedWork  string   `json:"completed_work"`
	PendingActions string   `json:"pending_actions"`
	Blockers       string   `json:"blockers"`
	NextSafeAction string   `json:"next_safe_action"`
	ModifiedFiles  []string `json:"modified_files"`
}

// runCheckpointCaptureTurn reads a host hook payload, projects the transcript,
// runs an isolated child model turn, validates its output, and submits it with
// provenance "capture".
func runCheckpointCaptureTurn(ctx context.Context, in io.Reader, host string, cfg adapter.Config, timeout time.Duration) error {
	if os.Getenv(captureTurnRecursionGuardEnv) != "" {
		return fmt.Errorf("capture turn refused: this process is already a capture turn")
	}

	raw, err := io.ReadAll(io.LimitReader(in, 4<<20))
	if err != nil {
		return fmt.Errorf("read hook payload: %w", err)
	}
	var h struct {
		SessionID      string `json:"session_id"`
		HookEventName  string `json:"hook_event_name"`
		Cwd            string `json:"cwd"`
		TranscriptPath string `json:"transcript_path"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return fmt.Errorf("the hook payload is not a JSON object: %w", err)
	}
	if strings.TrimSpace(h.TranscriptPath) == "" {
		return fmt.Errorf("the hook payload names no transcript_path")
	}

	// Gate before any model spend: a tiny session, or a session that has
	// already been captured with no new work, does not warrant another turn.
	fi, err := os.Stat(h.TranscriptPath)
	if err != nil {
		return fmt.Errorf("stat transcript: %w", err)
	}
	gatePath := captureTurnGatePath(h.SessionID)
	gate := loadCaptureTurnGate(gatePath)
	warranted, newGate := captureTurnWarranted(gate, fi.Size())
	if !warranted {
		return printCapture(map[string]any{
			"captured": false,
			"reason":   "capture turn not warranted (session too thin or already captured)",
		})
	}

	projection, err := sanitizeTranscript(h.TranscriptPath)
	if err != nil {
		return err
	}
	if strings.TrimSpace(projection) == "" {
		return fmt.Errorf("the transcript reduced to an empty projection; nothing to author from")
	}

	// Resolve the work before spending the model call: an unmapped cwd must not
	// consume a turn that would land nowhere.
	cwd := h.Cwd
	if strings.TrimSpace(cwd) == "" {
		cwd = mustCwd()
	}
	if _, err := cfg.ResolveForCwd(cwd); err != nil {
		return err
	}

	checkpoint, err := runCaptureTurnChild(ctx, projection, timeout)
	if err != nil {
		return err
	}

	// Validate the child's answer against the contract before submitting it.
	if err := validateCaptureTurn(*checkpoint); err != nil {
		return err
	}

	if err := submitCaptureTurn(ctx, host, cfg, cwd, h.SessionID, *checkpoint, timeout); err != nil {
		return err
	}
	saveCaptureTurnGate(gatePath, newGate)
	return nil
}

// sanitizeTranscript reads a Claude transcript JSONL and returns a bounded
// projection: user prompt strings and the assistant's final text prose only.
// thinking, tool_use and tool_result blocks are dropped because those are
// where pasted credentials live. Tool names and file edits are not retained.
func sanitizeTranscript(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open transcript: %w", err)
	}
	defer f.Close()

	var b strings.Builder
	sc := bufio.NewScanner(f)
	// A transcript line is a single JSON object; 1 MiB is far above any real one.
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec struct {
			Type    string `json:"type"`
			Message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		switch rec.Type {
		case "user":
			projectUser(&b, rec.Message.Content)
		case "assistant":
			projectAssistant(&b, rec.Message.Content)
		}
		if b.Len() >= captureTurnMaxChars {
			break
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("read transcript: %w", err)
	}
	return b.String(), nil
}

// projectUser keeps a user prompt string. A tool_result content block is
// dropped: it is the output of a tool and the likeliest home of a secret.
func projectUser(b *strings.Builder, content json.RawMessage) {
	if len(content) == 0 {
		return
	}
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		if s = strings.TrimSpace(s); s != "" {
			appendProjected(b, "user", s)
		}
	}
}

// projectAssistant keeps only text blocks (the assistant's own prose). thinking
// and tool_use blocks are dropped: thinking is noise for a handover, and
// tool_use input is exactly where a credential pasted into a tool call sits.
func projectAssistant(b *strings.Builder, content json.RawMessage) {
	if len(content) == 0 {
		return
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &blocks); err != nil {
		return
	}
	for _, blk := range blocks {
		if blk.Type != "text" {
			continue
		}
		if t := strings.TrimSpace(blk.Text); t != "" {
			appendProjected(b, "assistant", t)
		}
	}
}

func appendProjected(b *strings.Builder, role, text string) {
	if b.Len() > 0 {
		b.WriteByte('\n')
	}
	fmt.Fprintf(b, "[%s] %s", role, text)
}

// secretProjection matches credential shapes in the projection as a last line
// of defence. It is broader than secretish because the projection goes to a
// model, and a model will happily echo a token it was shown.
var secretProjection = regexp.MustCompile(`(?i)(sk-[A-Za-z0-9_\-]{12,}|gh[pousr]_[A-Za-z0-9]{16,}|eyJ[A-Za-z0-9_\-]{20,}|(?:AKIA|ASIA)[A-Z0-9]{16}|xox[baprs]-[A-Za-z0-9\-]{10,}|-----BEGIN [A-Z ]*PRIVATE KEY-----|Bearer [A-Za-z0-9_\-\.]{20,}|[A-Za-z0-9+/]{40,}={0,2}|\b[0-9a-f]{32,}\b)`)

// redactProjection removes credential shapes from the projection before it is
// handed to the child model. Redaction is a safety net on top of dropping tool
// blocks: a user may paste a key straight into a prompt.
func redactProjection(s string) string {
	return secretProjection.ReplaceAllString(s, "[redacted]")
}

// runCaptureTurnChild runs the isolated child turn and returns its parsed,
// schema-validated checkpoint. The child has no hooks, plugins, LSP or MCP, no
// session persistence, no tools, and a hard budget; it sees only the sanitized
// projection on stdin.
func runCaptureTurnChild(ctx context.Context, projection string, timeout time.Duration) (*captureTurnSubmit, error) {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return nil, fmt.Errorf("claude CLI not found on PATH; cannot run the capture turn: %w", err)
	}
	model := os.Getenv("MEMGW_CAPTURE_MODEL")
	if strings.TrimSpace(model) == "" {
		model = captureTurnDefaultModel
	}

	childCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(childCtx, bin,
		"-p",
		"--no-session-persistence",
		"--output-format", "json",
		"--json-schema", captureTurnSchema,
		"--model", model,
		"--max-budget-usd", captureTurnMaxBudgetUSD,
		"--permission-mode", "bypassPermissions",
	)
	// The projection goes on stdin, never on the command line, so it cannot
	// land in a process listing.
	cmd.Stdin = strings.NewReader(fmt.Sprintf(captureTurnPrompt, redactProjection(projection)))
	cmd.Env = append(os.Environ(),
		captureTurnRecursionGuardEnv+"=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
	)
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	if err := cmd.Run(); err != nil {
		if childCtx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("capture turn timed out after %s", timeout)
		}
		return nil, fmt.Errorf("capture turn failed: %v: %s", err, strings.TrimSpace(errBuf.String()))
	}

	// --output-format json wraps the result. structured_output is the
	// schema-validated object; when it is null (budget/error), result may carry
	// the JSON as a string. terminal_reason tells us whether the turn actually
	// completed, so a budget-exhausted or errored turn is refused rather than
	// parsed as a valid answer.
	var wrapper struct {
		StructuredOutput *captureTurnSubmit `json:"structured_output"`
		Result           string             `json:"result"`
		IsError          bool               `json:"is_error"`
		TerminalReason   string             `json:"terminal_reason"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(outBuf.String())), &wrapper); err != nil {
		return nil, fmt.Errorf("capture turn output is not the expected JSON: %w", err)
	}
	if wrapper.TerminalReason == "budget_exhausted" {
		return nil, fmt.Errorf("capture turn exhausted its budget (%s USD) before completing", captureTurnMaxBudgetUSD)
	}
	if wrapper.TerminalReason == "api_error" || wrapper.IsError {
		return nil, fmt.Errorf("capture turn reported an API error: %s", strings.TrimSpace(wrapper.Result))
	}

	var out captureTurnSubmit
	if wrapper.StructuredOutput != nil {
		out = *wrapper.StructuredOutput
	} else if strings.TrimSpace(wrapper.Result) != "" {
		// result is a JSON string of the structured object when the wrapper
		// does not expose structured_output directly.
		if err := json.Unmarshal([]byte(wrapper.Result), &out); err != nil {
			return nil, fmt.Errorf("capture turn result is not the expected JSON: %w", err)
		}
	} else {
		return nil, fmt.Errorf("capture turn produced no structured output")
	}
	return &out, nil
}

// validateCaptureTurn enforces the ledger contract on the child's answer. A
// field that is missing or absurd is refused rather than committed.
func validateCaptureTurn(cp captureTurnSubmit) error {
	if strings.TrimSpace(cp.Objective) == "" {
		return fmt.Errorf("capture turn produced no objective")
	}
	if strings.TrimSpace(cp.NextSafeAction) == "" {
		return fmt.Errorf("capture turn produced no next_safe_action")
	}
	for name, v := range map[string]string{
		"objective": cp.Objective, "completed_work": cp.CompletedWork,
		"pending_actions": cp.PendingActions, "blockers": cp.Blockers,
		"next_safe_action": cp.NextSafeAction,
	} {
		if len(v) > 4096 {
			return fmt.Errorf("capture turn field %q exceeds the ledger bound", name)
		}
	}
	if len(cp.ModifiedFiles) > 200 {
		return fmt.Errorf("capture turn produced too many modified_files")
	}
	return nil
}

// submitCaptureTurn submits the authored checkpoint with provenance "capture".
// It mirrors runCheckpointSubmit but resolves from the caller's cwd and uses the
// session id of the source session so the checkpoint joins the right branch.
func submitCaptureTurn(ctx context.Context, host string, cfg adapter.Config, cwd, sessionID string, cp captureTurnSubmit, timeout time.Duration) error {
	resolved, err := cfg.ResolveForCwd(cwd)
	if err != nil {
		return err
	}
	if resolved.Work.WorkID == uuid.Nil {
		if resolved.Gateway.BaseURL == "" {
			return fmt.Errorf("the resolved target names no work and no gateway is configured to name one")
		}
		tok, err := adapter.ReadToken(resolved.Gateway.TokenPath)
		if err != nil {
			return err
		}
		ensured, err := adapter.EnsureWork(ctx, resolved, tok, timeout)
		if err != nil {
			return fmt.Errorf("could not name the current work: %w", err)
		}
		resolved.Work = adapter.WorkConfig{WorkID: ensured.WorkID, Title: ensured.Title}
	}

	rev, err := readWorkRevision(ctx, resolved, timeout)
	if err != nil {
		return fmt.Errorf("could not read the work's current revision: %w", err)
	}
	if lc := lastCheckpoint; lc != nil && sameCheckpoint(*lc, adapter.CheckpointInput{
		Objective: cp.Objective, CompletedWork: cp.CompletedWork,
		PendingActions: cp.PendingActions, Blockers: cp.Blockers,
		NextSafeAction: cp.NextSafeAction, ModifiedFiles: cp.ModifiedFiles,
	}) {
		return printCapture(map[string]any{
			"captured": false,
			"reason":   "the latest checkpoint already says this",
		})
	}

	hook := adapter.Hook{
		Host:      host,
		Native:    "memgw_capture_turn",
		Lifecycle: adapter.Checkpoint,
		SessionID: sessionID,
		Cwd:       cwd,
		Checkpoint: &adapter.CheckpointInput{
			WorkID:           resolved.Work.WorkID,
			ExpectedRevision: rev,
			Objective:        cp.Objective,
			CompletedWork:    cp.CompletedWork,
			PendingActions:   cp.PendingActions,
			Blockers:         cp.Blockers,
			NextSafeAction:   cp.NextSafeAction,
			ModifiedFiles:    cp.ModifiedFiles,
			Provenance:       "capture",
		},
	}

	resolved.Collector = adapter.CollectorConfig{}
	opts := adapter.Options{Host: host, Config: resolved, Timeout: timeout}
	if resolved.Gateway.BaseURL != "" {
		tok, err := adapter.ReadToken(resolved.Gateway.TokenPath)
		if err != nil {
			return err
		}
		opts.Token = tok
	}
	out, err := adapter.SubmitHook(ctx, hook, opts)
	if err != nil {
		out.Error = err.Error()
		_ = adapter.Print(os.Stdout, out)
		return err
	}
	_ = adapter.Print(os.Stdout, out)
	if out.Result == nil || !out.Result.Accepted {
		return fmt.Errorf("the gateway did not accept the checkpoint")
	}
	return nil
}

