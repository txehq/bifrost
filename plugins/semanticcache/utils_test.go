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

// EmbeddingParameters.ExtraParams is json:"-", so a per-item extra parameter only reaches
// the request hash because the hash input carries it explicitly. Without that, Go SDK
// requests differing only in an item's extra params would share a direct-cache entry.
func TestGenerateRequestHash_IncludesPerItemEmbeddingExtraParams(t *testing.T) {
	plugin := newTestPlugin(t, newObservableStore())
	hashFor := func(extra map[string]interface{}) string {
		text := "hello"
		req := &schemas.BifrostRequest{
			RequestType: schemas.EmbeddingRequest,
			EmbeddingRequest: &schemas.BifrostEmbeddingRequest{
				Input: []schemas.EmbeddingInputItem{{
					Content: schemas.EmbeddingContent{{Type: schemas.EmbeddingContentPartTypeText, Text: &text}},
					Params:  &schemas.EmbeddingParameters{ExtraParams: extra},
				}},
			},
		}
		hash, err := plugin.generateRequestHash(req, nil)
		if err != nil {
			t.Fatalf("generateRequestHash: %v", err)
		}
		return hash
	}
	query := hashFor(map[string]interface{}{"taskType": "RETRIEVAL_QUERY"})
	if doc := hashFor(map[string]interface{}{"taskType": "RETRIEVAL_DOCUMENT"}); query == doc {
		t.Fatal("requests differing only in an item's ExtraParams share a request hash")
	}
	if none := hashFor(nil); query == none {
		t.Fatal("an item's ExtraParams do not change the request hash")
	}
	if again := hashFor(map[string]interface{}{"taskType": "RETRIEVAL_QUERY"}); query != again {
		t.Fatal("identical requests hash differently")
	}
}

// An extra param named like a first-class one must not erase the first-class value from the
// params hash: providers give the first-class field precedence, so the vectors (or completions)
// differ even when the alias is the same.
func TestParamsHash_ExtraParamAliasDoesNotHideFirstClassValue(t *testing.T) {
	plugin := newTestPlugin(t, newObservableStore())
	hash := func(extract func(map[string]interface{})) string {
		m := map[string]interface{}{}
		extract(m)
		h, err := hashMap(m)
		if err != nil {
			t.Fatalf("hashMap: %v", err)
		}
		return h
	}
	alias := map[string]interface{}{"task_type": "RETRIEVAL_DOCUMENT"}
	query, doc := "RETRIEVAL_QUERY", "SEMANTIC_SIMILARITY"
	a := hash(func(m map[string]interface{}) {
		plugin.extractEmbeddingParametersToMetadata(&schemas.EmbeddingParameters{TaskType: &query, ExtraParams: alias}, m)
	})
	b := hash(func(m map[string]interface{}) {
		plugin.extractEmbeddingParametersToMetadata(&schemas.EmbeddingParameters{TaskType: &doc, ExtraParams: alias}, m)
	})
	if a == b {
		t.Fatal("embedding requests differing in task_type share a params hash when ExtraParams carries the same alias")
	}

	same := map[string]interface{}{"temperature": 0.5}
	c := hash(func(m map[string]interface{}) {
		plugin.extractChatParametersToMetadata(&schemas.ChatParameters{Temperature: schemas.Ptr(0.1), ExtraParams: same}, m)
	})
	d := hash(func(m map[string]interface{}) {
		plugin.extractChatParametersToMetadata(&schemas.ChatParameters{Temperature: schemas.Ptr(0.9), ExtraParams: same}, m)
	})
	if c == d {
		t.Fatal("chat requests differing in temperature share a params hash when ExtraParams carries the same key")
	}
}
