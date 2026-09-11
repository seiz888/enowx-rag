package shadow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// DatasetVersion is the shape of a frozen dataset.
const DatasetVersion = "memgw-shadow-dataset/1"

// Lane is which of the two kinds of evidence a case produces.
type Lane string

const (
	// Correctness cases are exact: one expected decision, no threshold.
	Correctness Lane = "correctness"
	// Retrieval cases are measured and need a corpus and a provider.
	Retrieval Lane = "retrieval"
)

// Case is one frozen question.
type Case struct {
	ID   string `json:"id"`
	Lane Lane   `json:"lane"`

	// Correctness: the record to classify and the decision it must produce.
	Record *CaseRecord `json:"record,omitempty"`
	Expect *CaseExpect `json:"expect,omitempty"`

	// Retrieval: the query and the documents that should come back. The
	// documents are named by id, never by content, so a dataset can be read by
	// somebody who is not allowed to read the corpus.
	Query    string   `json:"query,omitempty"`
	Relevant []string `json:"relevant,omitempty"`
}

// CaseRecord is a correctness case's input. It mirrors the migration record
// rather than importing it, so the dataset format does not move when an
// internal struct does.
type CaseRecord struct {
	ChunkID      string `json:"chunk_id"`
	RAGProject   string `json:"rag_project"`
	DocumentID   string `json:"document_id,omitempty"`
	SourceDigest string `json:"source_digest,omitempty"`
	Sensitivity  string `json:"sensitivity,omitempty"`
	Text         string `json:"text,omitempty"`
}

// CaseExpect is what a correctness case must produce.
type CaseExpect struct {
	Decision  string `json:"decision"`
	Reason    string `json:"reason,omitempty"`
	Candidate bool   `json:"candidate,omitempty"`
}

// Thresholds are frozen with the cases. A retrieval threshold that is nil is a
// measurement nobody has agreed a bar for yet; it is reported and not judged,
// which is better than inventing a bar to have something to pass.
type Thresholds struct {
	// MinCorrectness is the fraction of correctness cases that must match. It
	// defaults to 1: an exact lane with a tolerance is not an exact lane.
	MinCorrectness *float64 `json:"min_correctness,omitempty"`
	MinRecallAtK   *float64 `json:"min_recall_at_k,omitempty"`
	K              int      `json:"k,omitempty"`
}

// Dataset is the frozen evaluation input.
type Dataset struct {
	DatasetVersion string     `json:"dataset_version"`
	Name           string     `json:"name"`
	FrozenAt       time.Time  `json:"frozen_at"`
	Thresholds     Thresholds `json:"thresholds"`
	Cases          []Case     `json:"cases"`
	// Digest covers the cases and the thresholds together. Freezing the cases
	// but not the bar they are judged against would leave the only number that
	// matters open to being chosen after the run.
	Digest string `json:"digest"`
}

// Digest recomputes the dataset digest.
func (d *Dataset) computeDigest() string {
	cases := make([]Case, len(d.Cases))
	copy(cases, d.Cases)
	sort.Slice(cases, func(i, j int) bool { return cases[i].ID < cases[j].ID })
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00", DatasetVersion, d.Name)
	body, _ := json.Marshal(struct {
		Cases      []Case     `json:"cases"`
		Thresholds Thresholds `json:"thresholds"`
	}{cases, d.Thresholds})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// Freeze stamps the digest. It is what turns a draft into a dataset.
func (d *Dataset) Freeze() {
	d.DatasetVersion = DatasetVersion
	if d.FrozenAt.IsZero() {
		d.FrozenAt = time.Now().UTC()
	}
	d.Digest = d.computeDigest()
}

// Write saves a frozen dataset.
func (d *Dataset) Write(path string) error {
	if d.Digest == "" {
		return fmt.Errorf("shadow: refusing to write a dataset that has not been frozen")
	}
	body, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(body, '\n'), 0o600)
}

// LoadDataset reads a dataset and refuses one whose digest no longer matches.
func LoadDataset(path string) (Dataset, error) {
	var d Dataset
	body, err := os.ReadFile(path)
	if err != nil {
		return d, err
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return d, fmt.Errorf("shadow: %s is not a dataset: %w", path, err)
	}
	if d.DatasetVersion != DatasetVersion {
		return Dataset{}, fmt.Errorf("shadow: dataset version %q is not %q", d.DatasetVersion, DatasetVersion)
	}
	if d.computeDigest() != d.Digest {
		return Dataset{}, fmt.Errorf("shadow: the dataset or its thresholds changed after it was frozen; re-freeze it deliberately, and say so, rather than running against an edited one")
	}
	if err := d.validate(); err != nil {
		return Dataset{}, err
	}
	return d, nil
}

func (d *Dataset) validate() error {
	seen := map[string]bool{}
	for i, c := range d.Cases {
		id := strings.TrimSpace(c.ID)
		if id == "" {
			return fmt.Errorf("shadow: case %d has no id", i)
		}
		if seen[id] {
			return fmt.Errorf("shadow: case id %q appears twice", id)
		}
		seen[id] = true
		switch c.Lane {
		case Correctness:
			if c.Record == nil || c.Expect == nil {
				return fmt.Errorf("shadow: correctness case %q needs a record and an expectation", id)
			}
		case Retrieval:
			if strings.TrimSpace(c.Query) == "" || len(c.Relevant) == 0 {
				return fmt.Errorf("shadow: retrieval case %q needs a query and at least one relevant document", id)
			}
		default:
			return fmt.Errorf("shadow: case %q is in lane %q, which is neither %q nor %q", id, c.Lane, Correctness, Retrieval)
		}
	}
	return nil
}

// LoadDraft reads a dataset that has not been frozen yet.
//
// Freezing is a separate, deliberate step with its own command, so that the
// moment a dataset and its thresholds stop being editable is a moment somebody
// chose. A draft is validated the same way a frozen dataset is; what it lacks
// is the digest.
func LoadDraft(path string) (Dataset, error) {
	var d Dataset
	body, err := os.ReadFile(path)
	if err != nil {
		return d, err
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return d, fmt.Errorf("shadow: %s is not a dataset: %w", path, err)
	}
	if err := d.validate(); err != nil {
		return Dataset{}, err
	}
	return d, nil
}
