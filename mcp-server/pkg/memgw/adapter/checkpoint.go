package adapter

import (
	"sort"
	"strings"
)

// The read side of a handover.
//
// The rest of this package writes: it turns a host's hook into an event and
// hands it to a sink. Nothing in it reads, because a lifecycle hook has nothing
// to do with what the ledger already knows. A handover does: the agent picking
// the work up has to be told what the agent that put it down had done, what was
// left, what was in the way and what is safe to do next.
//
// The read half already exists: POST /memgw/v1/bootstrap returns the latest
// checkpoint verbatim, and the two host fragments render it. What is missing is
// the write half being automatic. This file is that: it turns the task list a
// host already keeps -- Claude Code's TodoWrite, OMP's todo list -- into the
// checkpoint fields, so a handover is recorded because the agent worked, not
// because it remembered to write a note.

// TodoItem is one entry of a host's task list.
//
// Claude Code's TodoWrite and OMP's todo list agree on this much -- a line of
// text and a state -- and this deliberately keeps only that. A richer mapping
// would have to be re-derived every time either host changed its tool schema,
// and the checkpoint fields it feeds are free text anyway.
type TodoItem struct {
	Content string `json:"content"`
	Status  string `json:"status"`
}

// blockedMarkers are how a todo says it is stuck.
//
// A task list has no blocked state, so a blocker can only be recognised from
// the words the agent wrote. The vocabulary is small and English because the
// text is written by the model, not by the user, and the alternative -- asking
// the model to remember to file a blocker separately -- is exactly the manual
// step this whole path exists to remove.
var blockedMarkers = []string{"blocked", "blocker", "waiting on", "waiting for", "cannot ", "can't ", "stuck"}

// CheckpointFromTodos turns a task list into a handover note.
//
// The mapping is fixed and boring on purpose:
//
//   - completed  -> completed_work
//   - in_progress and pending -> pending_actions
//   - the in_progress item, or else the first pending one -> next_safe_action
//   - any item whose text carries a blocked marker -> blockers
//
// next_safe_action is required by the contract and must never be empty, so a
// list with nothing left to do says so in words rather than failing to record
// the checkpoint that proves the work is finished.
func CheckpointFromTodos(objective string, todos []TodoItem, files []string) CheckpointInput {
	var done, pending, blockers []string
	next := ""
	// The first pending item that is not itself a blocker. A blocked item is
	// the one thing that is certainly not safe to do next, so proposing it as
	// the next action would hand the following agent straight into the wall
	// the last one stopped at.
	firstFree := ""
	for _, t := range todos {
		line := strings.TrimSpace(t.Content)
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		blocked := false
		for _, m := range blockedMarkers {
			if strings.Contains(lower, m) {
				blockers = append(blockers, line)
				blocked = true
				break
			}
		}
		switch strings.ToLower(strings.TrimSpace(t.Status)) {
		case "completed":
			done = append(done, line)
		case "in_progress":
			pending = append(pending, line)
			if next == "" && !blocked {
				next = line
			}
			if firstFree == "" && !blocked {
				firstFree = line
			}
		default:
			pending = append(pending, line)
			if firstFree == "" && !blocked {
				firstFree = line
			}
		}
	}
	if next == "" {
		next = firstFree
	}
	if next == "" && len(pending) > 0 {
		next = pending[0]
	}
	if next == "" {
		next = "nothing pending; the task list is fully completed"
	}

	sort.Strings(files)
	return CheckpointInput{
		Objective:      clampField(objective),
		CompletedWork:  clampField(bullets(done)),
		PendingActions: clampField(bullets(pending)),
		Blockers:       clampField(bullets(blockers)),
		NextSafeAction: clampField(next),
		ModifiedFiles:  clampFiles(files),
	}
}

func bullets(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return "- " + strings.Join(lines, "\n- ")
}

// clampField keeps a field inside the contract's bound.
//
// Truncation is marked. A silently shortened handover reads as a complete one
// and the reader acts on it as such.
func clampField(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxTextFieldBytes {
		return s
	}
	const mark = "\n[truncated]"
	return strings.ToValidUTF8(s[:maxTextFieldBytes-len(mark)], "") + mark
}

func clampFiles(files []string) []string {
	if files == nil {
		return []string{}
	}
	if len(files) > maxModifiedFiles {
		return files[:maxModifiedFiles]
	}
	return files
}
