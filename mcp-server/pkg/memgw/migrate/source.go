package migrate

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/enowdev/enowx-rag/pkg/rag"
)

// Record is one pre-gateway chunk as a source hands it over.
//
// Text is present because classification has to look at it -- a credential is
// found by reading, not by reading about -- and it is never copied anywhere. It
// exists for the duration of one classify call.
type Record struct {
	ChunkID      string
	RAGProject   string
	DocumentID   string
	SourceDigest string
	Sensitivity  string
	Text         string
}

// Source hands records to the planner one at a time.
//
// Streaming rather than returning a slice: the corpus this is aimed at is tens
// of thousands of chunks, and a planner that had to hold all of them would be a
// planner that cannot be run on the machine the corpus is on.
type Source interface {
	Each(ctx context.Context, fn func(Record) error) error
	Name() string
}

// FileSource reads newline-delimited JSON.
//
// This is the shape an operator can produce, inspect and check into a ticket
// before anything is planned against a live store. Each line is one record;
// unknown fields are refused, so an export written against a different shape
// fails at the first line instead of producing a plan full of empty ids.
type FileSource struct{ Path string }

// NewFileSource names an export file.
func NewFileSource(path string) *FileSource { return &FileSource{Path: path} }

// Name implements Source.
func (f *FileSource) Name() string { return "file:" + f.Path }

type fileRecord struct {
	ChunkID      string `json:"chunk_id"`
	RAGProject   string `json:"rag_project"`
	DocumentID   string `json:"document_id"`
	SourceDigest string `json:"source_digest"`
	Sensitivity  string `json:"sensitivity"`
	Text         string `json:"text"`
}

// Each implements Source.
func (f *FileSource) Each(ctx context.Context, fn func(Record) error) error {
	fh, err := os.Open(f.Path)
	if err != nil {
		return err
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	line := 0
	for sc.Scan() {
		line++
		if ctx.Err() != nil {
			return ctx.Err()
		}
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.DisallowUnknownFields()
		var r fileRecord
		if err := dec.Decode(&r); err != nil {
			return fmt.Errorf("migrate: %s line %d is not a record: %w", f.Path, line, err)
		}
		if err := fn(Record{
			ChunkID: r.ChunkID, RAGProject: r.RAGProject, DocumentID: r.DocumentID,
			SourceDigest: r.SourceDigest, Sensitivity: r.Sensitivity, Text: r.Text,
		}); err != nil {
			return err
		}
	}
	return sc.Err()
}

// PointLister is the part of a vector store this package reads. It is an
// interface so planning can be tested without a running Qdrant, and so this
// package cannot accidentally acquire the ability to write to one.
type PointLister interface {
	ListPoints(ctx context.Context, projectID string, metaFilter map[string]string) ([]rag.PointInfo, error)
}

// PointsSource reads a rag project's points directly.
//
// It is read-only by construction: the interface it holds has one method and
// that method reads. Planning against a live store is still an operator
// decision -- a scroll over a production collection is a bulk read of a
// production corpus -- and this type does not make that decision, it only makes
// it possible once somebody has.
type PointsSource struct {
	Store   PointLister
	Project string
}

// Name implements Source.
func (p *PointsSource) Name() string { return "points:" + p.Project }

// Each implements Source.
func (p *PointsSource) Each(ctx context.Context, fn func(Record) error) error {
	points, err := p.Store.ListPoints(ctx, p.Project, nil)
	if err != nil {
		return err
	}
	for _, pt := range points {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := fn(recordFromPoint(p.Project, pt)); err != nil {
			return err
		}
	}
	return nil
}

// recordFromPoint reads the metadata this corpus actually carries.
//
// DocID is the original document identifier the store preserves alongside the
// uuid it indexes under; ContentHash is what re-chunking changes. A missing
// document id is normal for the oldest points and is derived downstream rather
// than treated as an error -- those are exactly the points a deletion has the
// most trouble finding, so dropping them would defeat the purpose.
func recordFromPoint(project string, pt rag.PointInfo) Record {
	doc := firstNonEmpty(pt.DocID, pt.SourceFile, pt.Meta["document_id"], pt.Meta["source_id"])
	digest := firstNonEmpty(pt.ContentHash, pt.Meta["source_digest"], pt.Meta["sha256"])
	return Record{
		ChunkID:      pt.ID,
		RAGProject:   project,
		DocumentID:   doc,
		SourceDigest: digest,
		Sensitivity:  firstNonEmpty(pt.Meta["sensitivity"], pt.Meta["sensitivity_class"]),
		Text:         pt.Content,
	}
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
