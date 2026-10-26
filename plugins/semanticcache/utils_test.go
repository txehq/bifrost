package semanticcache

import (
	"testing"

	"github.com/capsohq/bifrost/core/schemas"
)

// Embedding parameters that change the returned vector must reach the cache metadata,
// or a request differing only in one of them would be answered from another's entry
// (e.g. a RETRIEVAL_QUERY embedding served the cached RETRIEVAL_DOCUMENT vector).
func TestExtractEmbeddingParametersToMetadata_IncludesVectorAffectingParams(t *testing.T) {
	plugin := newTestPlugin(t, newObservableStore())
	metadataFor := func(p *schemas.EmbeddingParameters) map[string]interface{} {
		m := map[string]interface{}{}
		plugin.extractEmbeddingParametersToMetadata(p, m)
		return m
	}
	query, doc := "RETRIEVAL_QUERY", "RETRIEVAL_DOCUMENT"
	titleA, titleB := "a", "b"
	instrA, instrB := "Instruction: retrieve", "Instruction: cluster"
	for _, tc := range []struct {
		name string
		a, b *schemas.EmbeddingParameters
	}{
		{"task_type", &schemas.EmbeddingParameters{TaskType: &query}, &schemas.EmbeddingParameters{TaskType: &doc}},
		{"title", &schemas.EmbeddingParameters{Title: &titleA}, &schemas.EmbeddingParameters{Title: &titleB}},
		{"auto_truncate", &schemas.EmbeddingParameters{AutoTruncate: schemas.Ptr(true)}, &schemas.EmbeddingParameters{AutoTruncate: schemas.Ptr(false)}},
		{"instructions", &schemas.EmbeddingParameters{Instructions: &instrA}, &schemas.EmbeddingParameters{Instructions: &instrB}},
		{"sparse_embedding", &schemas.EmbeddingParameters{SparseEmbedding: map[string]interface{}{"type": "enabled"}}, &schemas.EmbeddingParameters{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ma, mb := metadataFor(tc.a), metadataFor(tc.b)
			if ma[tc.name] == mb[tc.name] {
				t.Fatalf("metadata[%q] is %v for both requests; they would share a cache entry", tc.name, ma[tc.name])
			}
		})
	}
}
