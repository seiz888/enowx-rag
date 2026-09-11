package graphify

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
)

// ToolVersion and IndexSchemaVersion identify what produced a generation.
//
// They are separate on purpose. The schema version changes when the shape of
// the index changes and every older generation must be rebuilt; the tool
// version changes when the walk changes and older generations are still
// readable. A reader that cannot tell those apart has to rebuild for every
// release.
const (
	ToolVersion        = "memgw-graphify/1"
	IndexSchemaVersion = "1.0.0"
)

// Manifest is what a generation says about itself.
//
// It is written last, inside the generation directory, and then again as the
// published pointer. A generation directory with no manifest is a build that
// did not finish, and is ignored rather than repaired.
type Manifest struct {
	// Generation is the directory name and the identity of this build.
	Generation string `json:"generation"`

	ProjectID   uuid.UUID `json:"project_id"`
	WorkspaceID uuid.UUID `json:"workspace_id"`
	RepoPath    string    `json:"repo_path"`

	// Commit is the HEAD the index was built from, empty outside a git
	// checkout. DirtyDigest covers everything the working tree has that the
	// commit does not; it is "clean" when there is nothing.
	Commit      string `json:"commit"`
	DirtyDigest string `json:"dirty_digest"`

	ToolVersion   string `json:"tool_version"`
	SchemaVersion string `json:"schema_version"`

	GeneratedAt time.Time `json:"generated_at"`
	BuildMillis int64     `json:"build_millis"`

	FileCount   int64  `json:"file_count"`
	ByteCount   int64  `json:"byte_count"`
	IndexDigest string `json:"index_digest"`

	// ExcludedCount is how many paths the secret-path rules kept out. It is
	// reported rather than listed: the count is what tells an operator the
	// rules are doing something, and the list would be a list of the names of
	// this machine's secret files.
	ExcludedCount int64 `json:"excluded_count"`

	// Stale marks a generation that was already known to be behind the working
	// tree when it was published. It is published anyway -- a stale index is
	// more useful than no index, as long as it says so.
	Stale       bool   `json:"stale,omitempty"`
	StaleReason string `json:"stale_reason,omitempty"`
}

func writeManifest(path string, m Manifest) error {
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return writeFileAtomic(path, body)
}

func readManifest(path string) (Manifest, error) {
	var m Manifest
	body, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return m, fmt.Errorf("graphify: %s is not a manifest: %w", path, err)
	}
	return m, nil
}

// writeFileAtomic writes through a temporary file in the same directory and
// renames it into place. The rename is what makes a reader see the whole file
// or none of it; writing in place would let a reader see half a manifest and
// conclude the generation is corrupt.
func writeFileAtomic(path string, body []byte) error {
	tmp, err := os.CreateTemp(dirOf(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
