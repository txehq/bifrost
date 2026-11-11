package vectorstore

import (
	"fmt"
	"math"
	"math/rand"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/capsohq/bifrost/core/network"
	"github.com/google/uuid"
)

// offlineHostKeys are the variables naming the service a vector-store test connects to. Under
// scripts/test-offline.sh (BIFROST_TEST_OFFLINE=1) each must name a loopback host (TXE-7083):
// Pinecone's index data plane and Weaviate's gRPC connection dial on their own, out of reach of
// the dial guard, and the tests create, write and delete data on whatever the variable names.
var offlineHostKeys = map[string]bool{"QDRANT_HOST": true, "REDIS_ADDR": true, "WEAVIATE_HOST": true, "PINECONE_INDEX_HOST": true}

// Helper functions
func getEnvWithDefault(key, defaultValue string) string {
	value := defaultValue
	if v := os.Getenv(key); v != "" {
		value = v
	}
	if offlineHostKeys[key] && network.OfflineEgressGuarded() && !network.IsLoopbackHost(hostOf(value)) {
		panic(fmt.Sprintf("%s=%q is not loopback: an offline run (BIFROST_TEST_OFFLINE=1) reaches loopback services only (TXE-7083)", key, value))
	}
	return value
}

// hostOf returns the host in "host", "host:port" or "scheme://host:port/...".
func hostOf(value string) string {
	value = strings.TrimSpace(value)
	if strings.Contains(value, "://") {
		if u, err := url.Parse(value); err == nil {
			return u.Hostname()
		}
	}
	if host, _, err := net.SplitHostPort(value); err == nil {
		return host
	}
	return value
}

func getEnvWithDefaultInt(key string, defaultValue int) (int, error) {
	if value := os.Getenv(key); value != "" {
		return strconv.Atoi(value)
	}
	return defaultValue, nil
}

func generateUUID() string {
	return uuid.New().String()
}

func generateTestEmbedding(dim int) []float32 {
	embedding := make([]float32, dim)
	for i := range embedding {
		embedding[i] = rand.Float32()*2 - 1 // Random values between -1 and 1
	}
	return embedding
}

// unitVectorAtCosine returns a unit vector of dim dimensions whose cosine similarity with
// (1, 0, 0, ...) is exactly cosine; the dimensions past the first two are zero.
func unitVectorAtCosine(dim int, cosine float64) []float32 {
	v := make([]float32, dim)
	v[0] = float32(cosine)
	v[1] = float32(math.Sqrt(1 - cosine*cosine))
	return v
}

func generateSimilarEmbedding(original []float32, similarity float32) []float32 {
	similar := make([]float32, len(original))
	for i := range similar {
		// Add small random noise to create similar but not identical embedding
		noise := (rand.Float32()*2 - 1) * (1 - similarity) * 0.1
		similar[i] = original[i] + noise
	}
	return similar
}
