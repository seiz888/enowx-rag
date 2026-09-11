package adapter

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// Resolution is how a hook's working directory names where the event lands.
//
// The fixed ProjectID/WorkspaceID/WorkID on Config answer "this workstation is
// the canary" once, for every hook. That is right for a single-purpose pilot and
// wrong for ordinary work: the same adapter runs inside any repo on the machine,
// and a fixed id would file every session under one project no matter where it
// happened. Resolution replaces the fixed ids with a mapping the operator owns.
type Resolution struct {
	// MapPath names a JSON file that maps cwd prefixes to a triple. Longest
	// prefix wins, so a monorepo root can sit above per-service entries.
	MapPath string `json:"map_path,omitempty"`

	// GitRemote, when true, falls back to the git remote for the project when
	// the cwd matches no mapped prefix: the remote is normalised and resolved
	// through the gateway's project-by-repo route. It never carries the token.
	GitRemote bool `json:"git_remote,omitempty"`

	// DefaultProjectSlug is used only when a git remote resolves to a project
	// that does not exist yet and the gateway creates it.
	DefaultProjectSlug string `json:"default_project_slug,omitempty"`

	// HostID is this workstation's host identity, required to derive a
	// workspace from a root-path digest when GitRemote is used.
	HostID uuid.UUID `json:"host_id,omitempty"`
}

// WorkMap is the on-disk shape of a resolution map.
type WorkMap struct {
	Entries []WorkMapEntry `json:"entries"`
}

// WorkMapEntry maps one cwd prefix to one target. The prefix is matched
// longest-first and case-insensitively on Windows, case-sensitively elsewhere.
type WorkMapEntry struct {
	Prefix      string    `json:"prefix"`
	ProjectID   uuid.UUID `json:"project_id"`
	WorkspaceID uuid.UUID `json:"workspace_id"`
	WorkID      uuid.UUID `json:"work_id"`
}

// Resolved is the answer for one hook: where its events land.
type Resolved struct {
	ProjectID   uuid.UUID
	WorkspaceID uuid.UUID
	WorkID      uuid.UUID // uuid.Nil when no work is in scope
}

// loadWorkMap reads and validates the resolution map. A map that exists but is
// malformed is an error, not an empty result: silently treating a broken map as
// "no mapping" would file every session under the git-remote fallback (or
// nowhere) and the operator would never see why.
func loadWorkMap(path string) (*WorkMap, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &WorkMap{}, nil
		}
		return nil, fmt.Errorf("resolve: the work map could not be read from %s: %w", path, err)
	}
	var m WorkMap
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("resolve: %s is not a usable work map: %w", path, err)
	}
	for i, e := range m.Entries {
		if strings.TrimSpace(e.Prefix) == "" {
			return nil, fmt.Errorf("resolve: work map entry %d has no prefix", i)
		}
		if e.ProjectID == uuid.Nil || e.WorkspaceID == uuid.Nil {
			return nil, fmt.Errorf("resolve: work map entry %q must name a project_id and a workspace_id", e.Prefix)
		}
	}
	return &m, nil
}

// resolveCwd maps a working directory to a target via the work map. It returns
// ok=false when no entry matches. Longest prefix wins, so a more specific entry
// overrides a broader one without either being rewritten.
func (m *WorkMap) resolveCwd(cwd string) (WorkMapEntry, bool) {
	cwd = filepath.Clean(cwd)
	var best WorkMapEntry
	bestLen := -1
	found := false
	for _, e := range m.Entries {
		p := filepath.Clean(e.Prefix)
		if pathMatchesPrefix(cwd, p) && len(p) > bestLen {
			best, bestLen, found = e, len(p), true
		}
	}
	return best, found
}

// ResolveTarget resolves a working directory to a target. The work map is
// consulted first; the git-remote fallback is used only when the map has no
// entry and GitRemote is enabled. A resolution that cannot name a project is an
// error: filing an event under the wrong project is worse than not filing it.
//
// The work dimension is deliberately conservative. A work id comes only from an
// explicit map entry; a git-remote fallback resolves project and workspace but
// leaves WorkID nil, because "the active work in this project" is not something
// the gateway can currently answer over HTTP and guessing it would attach a
// session to the wrong unit of work.
func ResolveTarget(cwd string, r Resolution) (Resolved, error) {
	if r.MapPath != "" {
		m, err := loadWorkMap(r.MapPath)
		if err != nil {
			return Resolved{}, err
		}
		if e, ok := m.resolveCwd(cwd); ok {
			return Resolved{ProjectID: e.ProjectID, WorkspaceID: e.WorkspaceID, WorkID: e.WorkID}, nil
		}
	}
	if r.GitRemote {
		// Resolving a git remote needs the gateway, and the gateway needs the
		// token. That is a network call inside a hook, so it is done only by the
		// caller that already holds the token (Run), not here. A missing
		// resolution path is reported as a clear error rather than guessed.
		return Resolved{}, fmt.Errorf("resolve: no work-map entry matches %q and git-remote resolution is not yet wired in this build", cwd)
	}
	return Resolved{}, fmt.Errorf("resolve: no work-map entry matches %q", cwd)
}

// pathsEqual reports whether child is equal to or beneath parent.
func pathMatchesPrefix(child, parent string) bool {
	if child == parent {
		return true
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// sortedPrefixes returns the map prefixes longest-first, for diagnostics.
func (m *WorkMap) sortedPrefixes() []string {
	out := make([]string, 0, len(m.Entries))
	for _, e := range m.Entries {
		out = append(out, e.Prefix)
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}
