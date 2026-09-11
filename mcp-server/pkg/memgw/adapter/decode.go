package adapter

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Decode turns whatever a host put on stdin into a Hook.
//
// Two families. Hosts that invoke a command with their own JSON -- Claude Code,
// Droid and Codex -- are decoded from the shape they actually emit. Hosts whose
// fragment is code in this repository -- OMP, OpenCode and Hermes -- send the
// canonical shape, because a translation we wrote is one we can hold to a
// schema instead of to a decoder full of alternates.
//
// The host is named by the caller and never read from the payload. A payload
// that could choose its own host could attribute a session to a host that was
// not running, and the caller is the settings file, which is the thing that
// actually knows.
func Decode(host string, raw []byte) (Hook, error) {
	if !KnownHost(host) {
		return Hook{}, fmt.Errorf("adapter: unknown host %q; this build decodes %s",
			host, strings.Join(Hosts, ", "))
	}
	var h Hook
	var err error
	switch host {
	case "omp", "opencode", "hermes":
		h, err = DecodeCanonical(raw)
		if err != nil {
			return Hook{}, err
		}
		if h.Host != "" && h.Host != host {
			// The fragment declared a host other than the one the command was
			// invoked for. That is a mis-installation -- an OMP plugin wired
			// into OpenCode's config -- and it is better found here than after
			// a week of sessions attributed to the wrong host.
			return Hook{}, fmt.Errorf("adapter: invoked for host %q but the payload declares %q", host, h.Host)
		}
	case "claude", "droid":
		h, err = decodeClaudeShaped(raw)
		if err != nil {
			return Hook{}, err
		}
	case "codex":
		h, err = decodeCodex(raw)
		if err != nil {
			return Hook{}, err
		}
	}
	h.Host = host
	if h.Lifecycle == SessionBranch {
		// A branch's reason lands in a bounded column, so it is normalised here
		// rather than passed through. See branchReasonClass.
		h.ReasonClass = branchReasonClass(h.ReasonClass)
	}
	if err := h.Validate(); err != nil {
		return Hook{}, err
	}
	return h, nil
}

// claudeHook is the object Claude Code writes to a hook's stdin. Droid's hook
// system is shaped after it and is decoded by the same function; where Droid
// diverges, the divergence shows up as an unrecognised event name and is
// reported rather than guessed.
//
// Unknown fields are allowed here, unlike the canonical shape: this struct
// describes somebody else's format, and a host that adds a field in a patch
// release must not break every hook on the machine.
type claudeHook struct {
	SessionID     string `json:"session_id"`
	HookEventName string `json:"hook_event_name"`
	Cwd           string `json:"cwd"`
	Source        string `json:"source"`  // SessionStart: startup|resume|clear|compact
	Trigger       string `json:"trigger"` // PreCompact: manual|auto
	Reason        string `json:"reason"`  // SessionEnd: clear|logout|prompt_input_exit|other
}

func decodeClaudeShaped(raw []byte) (Hook, error) {
	var c claudeHook
	if err := json.Unmarshal(raw, &c); err != nil {
		return Hook{}, fmt.Errorf("adapter: the hook payload is not a JSON object: %w", err)
	}
	h := Hook{
		Native:    c.HookEventName,
		SessionID: c.SessionID,
		Cwd:       c.Cwd,
	}
	switch c.HookEventName {
	case "SessionStart":
		switch c.Source {
		case "resume", "compact":
			// The same session id, carrying state it already had. No parent is
			// set: the parent of a resume is a *different* session, and naming
			// the session itself would make the ledger refuse it as a resume of
			// something that does not exist yet.
			h.Lifecycle = SessionResume
			h.ReasonClass = reasonClass(c.Source)
		default:
			h.Lifecycle = SessionStart
			h.ReasonClass = reasonClass(c.Source)
		}
	case "PreCompact":
		h.Lifecycle = SessionCompact
		h.ReasonClass = reasonClass(c.Trigger)
		// The trigger is the only value Claude Code offers that differs
		// between two compactions, and it usually does not. See Key for what
		// that costs.
		h.Discriminator = discriminator("trigger", c.Trigger)
	case "SessionEnd":
		h.Lifecycle = SessionEnd
		h.ReasonClass = reasonClass(c.Reason)
	case "":
		return Hook{}, fmt.Errorf("adapter: the hook payload names no event")
	default:
		// Stop is deliberately not a session end. It fires when the assistant
		// finishes a turn, which happens many times in one session, and
		// recording each as a session ending would make the ledger claim
		// dozens of sessions where there was one.
		h.Lifecycle = Ignored
	}
	return h, nil
}

// codexHook is the shape assumed for Codex CLI.
//
// It is assumed rather than observed: the event vocabulary was read out of the
// binary in the Phase 1 inventory and no Codex hook has ever been seen firing
// on this machine. The decoder therefore accepts the two spellings a JSON
// producer is likely to use for each field and refuses anything it cannot
// place, so a wrong assumption surfaces as a refusal with the payload's own
// keys in the message rather than as events with an empty session id.
type codexHook struct {
	Event         string `json:"event"`
	EventName     string `json:"event_name"`
	HookEventName string `json:"hook_event_name"`
	SessionID     string `json:"session_id"`
	SessionIDAlt  string `json:"sessionId"`
	Cwd           string `json:"cwd"`
	Reason        string `json:"reason"`
	Trigger       string `json:"trigger"`
	Source        string `json:"source"`
}

func decodeCodex(raw []byte) (Hook, error) {
	var c codexHook
	if err := json.Unmarshal(raw, &c); err != nil {
		return Hook{}, fmt.Errorf("adapter: the hook payload is not a JSON object: %w", err)
	}
	name := firstNonEmpty(c.Event, c.EventName, c.HookEventName)
	if name == "" {
		return Hook{}, fmt.Errorf("adapter: the codex hook payload names no event")
	}
	h := Hook{
		Native:    name,
		SessionID: firstNonEmpty(c.SessionID, c.SessionIDAlt),
		Cwd:       c.Cwd,
	}
	// Codex spells its hook_event_name in PascalCase ("SessionStart",
	// "SessionEnd"); older builds wrote hyphens ("session-start"). Both are
	// normalised to one kebab token so a release that changed the casing does
	// not silently start dropping every event into Ignored.
	switch strings.ToLower(strings.ReplaceAll(name, "_", "-")) {
	case "sessionstart", "session-start":
		h.Lifecycle = SessionStart
		// SessionStart carries `source` (startup|resume|clear|compact), not
		// `reason`.
		h.ReasonClass = reasonClass(firstNonEmpty(c.Source, c.Reason))
	case "precompact", "pre-compact":
		h.Lifecycle = SessionCompact
		h.ReasonClass = reasonClass(c.Trigger)
		h.Discriminator = discriminator("trigger", c.Trigger)
	case "sessionend", "session-end":
		h.Lifecycle = SessionEnd
		h.ReasonClass = reasonClass(c.Reason)
	default:
		// post-compact, pre-tool-use, permission-request, stop, interrupt and
		// the rest: real events with no canonical meaning. post-compact in
		// particular is not recorded, because the compaction is already
		// recorded by pre-compact and recording both would double every
		// compaction in the history.
		h.Lifecycle = Ignored
	}
	return h, nil
}

// reasonClasses are the classifications this adapter will pass on. Anything
// else becomes "other": reason_class is a bounded enumeration in the contract,
// and letting a host widen it would turn a classification into free text that
// the ledger then stores forever.
var reasonClasses = map[string]bool{
	"startup": true, "resume": true, "clear": true, "compact": true,
	"manual": true, "auto": true, "logout": true, "timeout": true,
	"error": true, "shutdown": true, "branch": true, "rewind": true,
}

// branchReasonClasses is the vocabulary the ledger's branches table accepts.
// It is a different, smaller set than reasonClasses: for session.branched the
// reason is not a note about the event, it is a column with a CHECK constraint
// on it, and a class outside the set is refused.
var branchReasonClasses = map[string]bool{
	"rewind": true, "fork": true, "resume": true, "compaction": true, "initial": true,
}

// branchSynonyms maps what a host is likely to call a branch onto that
// vocabulary. Anything not listed becomes empty, and an empty reason is filled
// in by the ledger as "rewind" -- inventing a class here would put a guess in a
// column that is meant to be a classification.
var branchSynonyms = map[string]string{
	"branch": "rewind", "rewind": "rewind",
	"fork": "fork", "new": "fork",
	"resume":  "resume",
	"compact": "compaction", "compaction": "compaction",
	"initial": "initial",
}

func branchReasonClass(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		return ""
	}
	if branchReasonClasses[v] {
		return v
	}
	return branchSynonyms[v]
}

func reasonClass(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		return ""
	}
	if reasonClasses[v] {
		return v
	}
	return "other"
}

func discriminator(k, v string) map[string]string {
	if v == "" {
		return nil
	}
	return map[string]string{k: v}
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
