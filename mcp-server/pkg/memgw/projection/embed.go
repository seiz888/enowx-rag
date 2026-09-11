package projection

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"math"
	"strings"
	"unicode"
)

// FixtureModelName is written into the payload of every point embedded by
// FixtureEmbedder.
//
// It is not decoration. A collection built from fixture vectors and a
// collection built from a real embedding model are indistinguishable by size,
// shape or point count, and the difference matters enormously: one supports a
// retrieval claim and the other supports none. Recording the model in the data
// means the question "what built this collection?" is answered by looking,
// not by remembering which script was run.
const FixtureModelName = "memgw-fixture-hash-v1"

// FixtureEmbedder produces deterministic vectors with no semantic content.
//
// It exists because the projection has to be verified without a paid embedding
// API and without sending anything to an external provider, and because the
// property the projection actually needs from an embedder in those tests is
// determinism: the same document must produce the same vector, so re-applying
// an event is a no-op rather than a second, slightly different point.
//
// What it does is feature hashing. Each token is hashed; the hash picks a
// coordinate and a sign; the coordinates are summed and the result is
// normalised. Two texts that share tokens therefore land closer together than
// two that share none, which is enough to tell that the store, the ids, the
// filters and the deletions work.
//
// What it emphatically does NOT do is capture meaning. "the build failed" and
// "the compilation broke" share no tokens and are orthogonal here. Any
// retrieval quality measured against these vectors is a measurement of token
// overlap, and reporting it as semantic accuracy would be inventing a result.
// Nothing in the non-production verification claims semantic quality, and
// nothing should: it is untested.
type FixtureEmbedder struct {
	dim int
}

// NewFixtureEmbedder returns an embedder of the given width. 384 matches the
// dimension the small sentence-transformer models use, so a collection created
// with fixtures has the same shape as one a real embedder would create and the
// swap does not need a different collection.
func NewFixtureEmbedder(dim int) *FixtureEmbedder {
	if dim <= 0 {
		dim = 384
	}
	return &FixtureEmbedder{dim: dim}
}

// VectorSize implements rag.EmbeddingClient.
func (e *FixtureEmbedder) VectorSize() int { return e.dim }

// ModelName implements rag.ModelNamer, so the provider stamps every point with
// the fixture's name.
func (e *FixtureEmbedder) ModelName() string { return FixtureModelName }

// Embed implements rag.EmbeddingClient. It never fails, never blocks and never
// leaves the process.
func (e *FixtureEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = e.vector(t)
	}
	return out, nil
}

func (e *FixtureEmbedder) vector(text string) []float32 {
	v := make([]float64, e.dim)
	for _, tok := range tokenize(text) {
		sum := sha256.Sum256([]byte(tok))
		idx := int(binary.BigEndian.Uint32(sum[0:4]) % uint32(e.dim))
		// The sign comes from a different part of the digest than the index, so
		// two tokens landing in the same coordinate are as likely to cancel as
		// to reinforce -- which is what keeps a collision from silently acting
		// like a repeated token.
		if sum[4]&1 == 1 {
			v[idx]--
		} else {
			v[idx]++
		}
	}
	var norm float64
	for _, x := range v {
		norm += x * x
	}
	out := make([]float32, e.dim)
	if norm == 0 {
		// A text with no tokens at all still needs a vector Qdrant will accept
		// under cosine distance, and a zero vector has no direction. Hashing
		// the whole text keeps even this case deterministic.
		sum := sha256.Sum256([]byte(text))
		out[int(binary.BigEndian.Uint32(sum[0:4])%uint32(e.dim))] = 1
		return out
	}
	norm = math.Sqrt(norm)
	for i, x := range v {
		out[i] = float32(x / norm)
	}
	return out
}

// tokenize lowercases and splits on anything that is not a letter or a digit.
// Underscores survive as separators, so an identifier contributes its parts.
func tokenize(text string) []string {
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := fields[:0]
	for _, f := range fields {
		if len(f) > 1 {
			out = append(out, f)
		}
	}
	return out
}
