package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// Automatic capture.
//
// `checkpoint --submit` records a handover a model authored. It is correct and
// it is not enough: it happens only when something asks the model to write one,
// and an agent that has to remember to write a note is the failure this whole
// path exists to remove.
//
// Capture takes the other route. It reads what the host already records without
// being asked -- the task list the agent keeps while it works -- and turns it
// into the checkpoint fields. Nothing is invented: every line submitted was
// written by the agent itself, in its own tool call, for its own reasons. The
// only thing this adds is the mapping, and the mapping is fixed (see
// adapter.CheckpointFromTodos) so two runs over the same list produce the same
// checkpoint and therefore the same idempotency key.
//
// It is wired to three host events:
//
//	UserPromptSubmit  the session's first prompt becomes the objective and is
//	                  remembered; nothing is submitted.
//	PostToolUse       a task-list write submits a checkpoint from the new list.
//	                  Claude Code has shipped two task tools: TodoWrite, which
//	                  carries the whole list in one call, and the TaskCreate /
//	                  TaskUpdate / TaskList trio, which carries one change at a
//	                  time. Both are read, because which one a build exposes is
//	                  not this code's decision -- 2.1.267 offers the trio and no
//	                  TodoWrite at all, and a capture keyed on TodoWrite alone
//	                  would sit there recording nothing.
//	Stop/SessionEnd/PreCompact
//	                  re-submit the last remembered list, so a session that
//	                  ends without touching its todos still leaves a handover.
//	                  A re-submission of an unchanged list derives the same key
//	                  and is answered `duplicate`, not written twice.
//
// provenance is "observed": these fields were read from what the agent did, not
// authored in a turn asked for them. A reader must be able to tell those apart.
const captureProvenance = "observed"

// captureState is what one session remembers between hooks. It holds no
// transcript and no tool output: an objective line and the task list, which are
// exactly the two things the checkpoint is built from.
type captureState struct {
	Objective string             `json:"objective"`
	Todos     []adapter.TodoItem `json:"todos"`
	// Tasks is the incremental list, kept by id because TaskUpdate names a
	// task by id and nothing else. Order is creation order, which is the order
	// the agent wrote them in and therefore the order they read best in.
	Tasks []captureTask `json:"tasks,omitempty"`
	// Sent is the digest of the last checkpoint this session got accepted. It
	// is the local half of the duplicate defence: the gateway's copy is
	// authoritative but reaches the ledger through a queue, so a hook that
	// fires twice in quick succession can read a bootstrap that does not yet
	// show the first write. Remembering what was sent closes that window
	// without weakening the check that follows it.
	Sent string `json:"sent_digest,omitempty"`
}

// captureTask is one entry of the incremental task list.
type captureTask struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Status  string `json:"status"`
}

// claudeCaptureHook is the part of a Claude Code hook payload capture reads.
// Unknown fields are ignored on purpose: this is a foreign format and a new
// field in it must not stop a checkpoint being recorded.
type claudeCaptureHook struct {
	SessionID string `json:"session_id"`
	Event     string `json:"hook_event_name"`
	Cwd       string `json:"cwd"`
	Prompt    string `json:"prompt"`
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		Todos []adapter.TodoItem `json:"todos"`
		// The task trio's fields. Subject is the task's text; TaskUpdate names
		// the task by id and may carry a new status, a new subject, or both.
		TaskID  string `json:"taskId"`
		Subject string `json:"subject"`
		Status  string `json:"status"`
	} `json:"tool_input"`
	ToolResponse struct {
		Task *struct {
			ID      string `json:"id"`
			Subject string `json:"subject"`
		} `json:"task"`
		Tasks []struct {
			ID      string `json:"id"`
			Subject string `json:"subject"`
			Status  string `json:"status"`
		} `json:"tasks"`
		StatusChange *struct {
			To string `json:"to"`
		} `json:"statusChange"`
	} `json:"tool_response"`
}

// secretish matches the shapes a credential takes when it is pasted into a
// prompt. The objective is the one field capture takes from user text, and a
// ledger is a place a secret must not reach; a long opaque run is replaced
// rather than stored, because a redacted objective is still a usable objective.
var secretish = regexp.MustCompile(`(?i)(sk-[A-Za-z0-9_\-]{12,}|gh[pousr]_[A-Za-z0-9]{16,}|eyJ[A-Za-z0-9_\-]{20,}|(?:AKIA|ASIA)[A-Z0-9]{16}|xox[baprs]-[A-Za-z0-9\-]{10,}|[A-Za-z0-9+/]{40,}={0,2}|\b[0-9a-f]{32,}\b)`)

func runCheckpointCapture(ctx context.Context, in io.Reader, host string, cfg adapter.Config, timeout time.Duration) error {
	raw, err := io.ReadAll(io.LimitReader(in, 4<<20))
	if err != nil {
		return fmt.Errorf("read hook payload: %w", err)
	}
	var h claudeCaptureHook
	if err := json.Unmarshal(raw, &h); err != nil {
		return fmt.Errorf("the hook payload is not a JSON object: %w", err)
	}
	if strings.TrimSpace(h.SessionID) == "" {
		return fmt.Errorf("the hook payload names no session")
	}

	path := capturePath(h.SessionID)
	state := loadCaptureState(path)

	switch h.Event {
	case "UserPromptSubmit":
		// Only the first prompt of a session becomes the objective. A later
		// prompt refines the work; it does not restate what the session is for,
		// and letting it overwrite would make the objective drift to whatever
		// was asked last.
		if state.Objective == "" && strings.TrimSpace(h.Prompt) != "" {
			state.Objective = objectiveFromPrompt(h.Prompt)
			saveCaptureState(path, state)
		}
		return printCapture(map[string]any{"captured": false, "reason": "objective remembered"})
	case "PostToolUse":
		if !applyTaskTool(&state, h) {
			return printCapture(map[string]any{"captured": false, "reason": "not a task-list write"})
		}
		saveCaptureState(path, state)
	case "Stop", "SessionEnd", "PreCompact":
		if len(currentList(state)) == 0 {
			return printCapture(map[string]any{"captured": false, "reason": "no task list was recorded in this session"})
		}
	default:
		return printCapture(map[string]any{"captured": false, "reason": "event carries no checkpoint"})
	}

	objective := state.Objective
	if objective == "" {
		objective = cfg.Work.Title
	}
	if objective == "" {
		objective = "work in " + filepath.Base(cwdOr(h.Cwd))
	}

	cp := adapter.CheckpointFromTodos(objective, currentList(state), modifiedFiles(cwdOr(h.Cwd)))
	cp.Provenance = captureProvenance

	digest := checkpointDigest(cp)
	if digest == state.Sent {
		return printCapture(map[string]any{"captured": false, "reason": "this session already recorded exactly this checkpoint"})
	}
	if err := submitCaptured(ctx, host, cfg, h.SessionID, cwdOr(h.Cwd), cp, timeout); err != nil {
		return err
	}
	state.Sent = digest
	saveCaptureState(path, state)
	return nil
}

// submitCaptured writes the checkpoint, retrying a lost CAS race.
//
// The expected revision is read from the gateway immediately before the write,
// so a race is narrow but real: two hosts checkpointing the same work at the
// same moment. Losing that race is not an error to report, it is a revision to
// re-read -- but only a bounded number of times, because a conflict that never
// clears is a different problem and must surface rather than spin.
func submitCaptured(ctx context.Context, host string, cfg adapter.Config, sessionID, cwd string, cp adapter.CheckpointInput, timeout time.Duration) error {
	resolved, err := cfg.ResolveForCwd(cwd)
	if err != nil {
		return err
	}
	if resolved.Work.WorkID == uuid.Nil {
		return fmt.Errorf("the resolved target names no work; a checkpoint must name the work it belongs to")
	}

	// A checkpoint goes straight to the gateway, never through the collector.
	//
	// The collector is right for a lifecycle event: it commits to disk and
	// forwards later, so a hook returns durable even with the gateway down. A
	// checkpoint cannot use that. It carries an expected revision read moments
	// earlier, and a queue holds it for an interval nobody controls -- two
	// checkpoints queued between two forwards present the same revision, and
	// the second is refused as stale_revision and quarantined. That was
	// observed on this workstation: one canary's task-tool calls put a row in
	// the claude spool that could never succeed, because by the time it was
	// forwarded the revision it named was already behind.
	//
	// So the compare-and-set is performed against the ledger synchronously or
	// not at all. Losing the gateway costs a checkpoint, which the next one
	// supersedes anyway; queueing it costs a quarantined row and an operator.
	resolved.Collector = adapter.CollectorConfig{}

	opts := adapter.Options{Host: host, Config: resolved, Timeout: timeout}
	if resolved.Gateway.BaseURL != "" {
		tok, err := adapter.ReadToken(resolved.Gateway.TokenPath)
		if err != nil {
			return err
		}
		opts.Token = tok
	}

	var last error
	for attempt := 0; attempt < 3; attempt++ {
		rev, err := readWorkRevision(ctx, resolved, timeout)
		if err != nil {
			return fmt.Errorf("could not read the work's current revision: %w", err)
		}
		// A checkpoint whose content is already the latest one is not written
		// again. The expected revision is part of the idempotency fingerprint,
		// so a re-fired hook after a successful write derives a *different*
		// key and would otherwise be recorded as a second, identical
		// checkpoint. Comparing the content is what makes a retry a no-op.
		if same := lastCheckpoint; same != nil && sameCheckpoint(*same, cp) {
			return printCapture(map[string]any{
				"captured": false,
				"reason":   "the latest checkpoint already says this",
			})
		}

		cp.WorkID = resolved.Work.WorkID
		cp.ExpectedRevision = rev

		hook := adapter.Hook{
			Host:      host,
			Native:    "memgw_capture",
			Lifecycle: adapter.Checkpoint,
			SessionID: sessionID,
			Cwd:       cwd,
			// A copy per attempt: the expected revision is part of the
			// idempotency fingerprint, so a retry at a new revision is a
			// different event and must not reuse the previous one's key.
			Checkpoint: &cp,
		}
		out, err := adapter.SubmitHook(ctx, hook, opts)
		if err != nil {
			out.Error = err.Error()
			_ = adapter.Print(os.Stdout, out)
			return err
		}
		if out.Result != nil && out.Result.Accepted {
			_ = adapter.Print(os.Stdout, out)
			return nil
		}
		class := ""
		if out.Result != nil {
			class = out.Result.Class
		}
		if class != "cas_conflict" && class != "stale_revision" {
			_ = adapter.Print(os.Stdout, out)
			return fmt.Errorf("the gateway refused the checkpoint: %s", class)
		}
		last = fmt.Errorf("the checkpoint lost a revision race (%s)", class)
	}
	return last
}

// applyTaskTool folds one task-tool call into the session's list. It reports
// false for a tool call that says nothing about the list.
//
// TaskList is trusted over the incremental state whenever it appears: it is the
// host's own answer to "what is the list", so a state that drifted -- a create
// this process never saw, a session that started mid-work -- is corrected the
// next time the agent looks at its tasks.
func applyTaskTool(state *captureState, h claudeCaptureHook) bool {
	switch h.ToolName {
	case "TodoWrite":
		if len(h.ToolInput.Todos) == 0 {
			return false
		}
		state.Todos = h.ToolInput.Todos
		return true
	case "TaskCreate":
		id, subject := "", h.ToolInput.Subject
		if t := h.ToolResponse.Task; t != nil {
			id = t.ID
			if subject == "" {
				subject = t.Subject
			}
		}
		if subject == "" {
			return false
		}
		state.Tasks = append(state.Tasks, captureTask{ID: id, Content: subject, Status: "pending"})
		return true
	case "TaskUpdate":
		status := h.ToolInput.Status
		if status == "" && h.ToolResponse.StatusChange != nil {
			status = h.ToolResponse.StatusChange.To
		}
		id := h.ToolInput.TaskID
		for i := range state.Tasks {
			if state.Tasks[i].ID != id {
				continue
			}
			if status == "deleted" {
				state.Tasks = append(state.Tasks[:i], state.Tasks[i+1:]...)
				return true
			}
			if status != "" {
				state.Tasks[i].Status = status
			}
			if h.ToolInput.Subject != "" {
				state.Tasks[i].Content = h.ToolInput.Subject
			}
			return true
		}
		return false
	case "TaskList":
		if len(h.ToolResponse.Tasks) == 0 {
			return false
		}
		tasks := make([]captureTask, 0, len(h.ToolResponse.Tasks))
		for _, t := range h.ToolResponse.Tasks {
			tasks = append(tasks, captureTask{ID: t.ID, Content: t.Subject, Status: t.Status})
		}
		state.Tasks = tasks
		return true
	default:
		return false
	}
}

// currentList is the session's task list in the shape the checkpoint mapping
// takes. TodoWrite wins when a session used it, because a build that has it
// carries the whole list in one call and there is nothing to merge.
func currentList(state captureState) []adapter.TodoItem {
	if len(state.Todos) > 0 {
		return state.Todos
	}
	out := make([]adapter.TodoItem, 0, len(state.Tasks))
	for _, t := range state.Tasks {
		out = append(out, adapter.TodoItem{Content: t.Content, Status: t.Status})
	}
	return out
}

// checkpointDigest identifies a checkpoint by its content alone -- not by its
// work id or expected revision, which change between attempts at recording the
// same thing.
func checkpointDigest(cp adapter.CheckpointInput) string {
	h := sha256.New()
	for _, f := range []string{cp.Objective, cp.CompletedWork, cp.PendingActions, cp.Blockers, cp.NextSafeAction} {
		fmt.Fprintf(h, "%d:%s", len(f), f)
	}
	for _, f := range cp.ModifiedFiles {
		fmt.Fprintf(h, "%d:%s", len(f), f)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// sameCheckpoint reports whether a checkpoint would say exactly what the
// recorded one already says. Files are compared in order because
// CheckpointFromTodos sorts them, so equal content always compares equal.
func sameCheckpoint(a latestCheckpoint, b adapter.CheckpointInput) bool {
	if a.Objective != b.Objective || a.CompletedWork != b.CompletedWork ||
		a.PendingActions != b.PendingActions || a.Blockers != b.Blockers ||
		a.NextSafeAction != b.NextSafeAction || len(a.ModifiedFiles) != len(b.ModifiedFiles) {
		return false
	}
	for i := range a.ModifiedFiles {
		if a.ModifiedFiles[i] != b.ModifiedFiles[i] {
			return false
		}
	}
	return true
}

// objectiveFromPrompt keeps the first line, bounded and redacted.
func objectiveFromPrompt(prompt string) string {
	line := strings.TrimSpace(prompt)
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	if len(line) > 500 {
		line = strings.ToValidUTF8(line[:500], "") + "…"
	}
	return secretish.ReplaceAllString(line, "[redacted]")
}

// modifiedFiles is the working tree's own answer, not a guess.
//
// It is `git status --porcelain` because that is what "the files this session
// changed" means to the person reading the handover. A directory that is not a
// repository yields nothing, which is reported as an empty list rather than as
// a failure: a checkpoint is still worth recording without it.
func modifiedFiles(cwd string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", cwd, "status", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var files []string
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) < 4 {
			continue
		}
		name := strings.TrimSpace(line[3:])
		// A rename reads "old -> new"; the new path is the one that exists.
		if i := strings.Index(name, " -> "); i >= 0 {
			name = name[i+4:]
		}
		files = append(files, strings.Trim(name, `"`))
	}
	return files
}

func cwdOr(cwd string) string {
	if strings.TrimSpace(cwd) != "" {
		return cwd
	}
	return mustCwd()
}

func capturePath(sessionID string) string {
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
	return filepath.Join(root, string(safe)+".json")
}

func loadCaptureState(path string) captureState {
	var s captureState
	raw, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	_ = json.Unmarshal(raw, &s)
	return s
}

func saveCaptureState(path string, s captureState) {
	raw, err := json.Marshal(s)
	if err != nil {
		return
	}
	_ = os.WriteFile(path, raw, 0o600)
}

// captureQuiet silences the no-op lines.
//
// Claude Code folds a UserPromptSubmit hook's stdout into the model's context,
// so a hook that says "nothing to record" on every prompt would put that
// sentence in front of the model all day. Silence is only ever applied to the
// outcomes that record nothing; a checkpoint that was written is always
// printed.
var captureQuiet bool

// printCapture reports a hook that produced no checkpoint. It is a success:
// most hooks are not checkpoint moments, and a host that treated them as
// failures would fill its log with them.
func printCapture(v map[string]any) error {
	if captureQuiet {
		return nil
	}
	return json.NewEncoder(os.Stdout).Encode(v)
}
