package semantic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

// EmbeddingKind selects the Nomic task prefix. The model was trained to keep
// documents and search queries in different input roles; omitting the prefixes
// silently degrades retrieval quality.
type EmbeddingKind string

const (
	EmbeddingDocument EmbeddingKind = "search_document"
	EmbeddingQuery    EmbeddingKind = "search_query"
)

// Embedder is the only inference dependency visible to the semantic store.
// Implementations must return one finite normalized vector per input.
type Embedder interface {
	Embed(ctx context.Context, kind EmbeddingKind, texts []string) ([][]float32, error)
}

// HTTPEmbedder calls an OpenAI-compatible embedding endpoint served by the
// installation-wide llama.cpp process. It holds no project state.
type HTTPEmbedder struct {
	Endpoint   string
	Model      string
	Dimensions int
	Client     *http.Client
}

type embeddingRequest struct {
	Model          string   `json:"model"`
	Input          []string `json:"input"`
	EncodingFormat string   `json:"encoding_format"`
}

type embeddingResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Embed validates ordering, dimensions, finiteness, and normalization at the
// adapter boundary. Invalid model output can therefore never enter persistent
// project state.
func (e *HTTPEmbedder) Embed(ctx context.Context, kind EmbeddingKind, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if e.Endpoint == "" || e.Model == "" || e.Dimensions <= 0 {
		return nil, fail(CodeEmbeddingUnavailable,
			"The embedding service is not configured.", false, nil)
	}
	prefixed := make([]string, len(texts))
	for i, value := range texts {
		prefixed[i] = embeddingInput(kind, value)
	}
	body, err := json.Marshal(embeddingRequest{
		Model: e.Model, Input: prefixed, EncodingFormat: "float",
	})
	if err != nil {
		return nil, fail(CodeInternalError, "The embedding request could not be encoded.", false, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fail(CodeInternalError, "The embedding request could not be created.", false, err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := e.Client
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, fail(CodeEmbeddingUnavailable,
			"The local embedding service could not be reached.", true, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 32<<20))
	if err != nil {
		return nil, fail(CodeEmbeddingUnavailable,
			"The local embedding response could not be read.", true, err)
	}
	var decoded embeddingResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fail(CodeEmbeddingUnavailable,
			"The local embedding service returned invalid JSON.", true, err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := strings.TrimSpace(response.Status)
		if decoded.Error != nil && strings.TrimSpace(decoded.Error.Message) != "" {
			message = strings.TrimSpace(decoded.Error.Message)
		}
		return nil, fail(CodeEmbeddingUnavailable,
			"The local embedding service refused the request: "+message, true, nil)
	}
	if len(decoded.Data) != len(texts) {
		return nil, fail(CodeEmbeddingUnavailable,
			fmt.Sprintf("The local embedding service returned %d vectors for %d inputs.", len(decoded.Data), len(texts)),
			true, nil)
	}
	sort.Slice(decoded.Data, func(i, j int) bool { return decoded.Data[i].Index < decoded.Data[j].Index })
	vectors := make([][]float32, len(texts))
	for i, item := range decoded.Data {
		if item.Index != i {
			return nil, fail(CodeEmbeddingUnavailable,
				"The local embedding service returned non-contiguous vector indexes.", true, nil)
		}
		if len(item.Embedding) < e.Dimensions {
			return nil, fail(CodeModelMismatch,
				fmt.Sprintf("The embedding model returned %d dimensions; at least %d are required.", len(item.Embedding), e.Dimensions),
				false, nil)
		}
		vector := append([]float32(nil), item.Embedding[:e.Dimensions]...)
		if err := normalize(vector); err != nil {
			return nil, err
		}
		vectors[i] = vector
	}
	return vectors, nil
}

// embeddingInput owns the exact task-prefix representation shared by the HTTP
// adapter and the chunker's complete-input budget calculation.
func embeddingInput(kind EmbeddingKind, text string) string {
	return string(kind) + ": " + text
}

// normalize gives cosine distance consistent inputs and rejects NaN, infinity,
// and zero vectors before sqlite-vec sees them.
func normalize(vector []float32) error {
	var squares float64
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return fail(CodeEmbeddingUnavailable,
				"The embedding model returned a non-finite value.", true, nil)
		}
		squares += float64(value) * float64(value)
	}
	if squares == 0 {
		return fail(CodeEmbeddingUnavailable,
			"The embedding model returned a zero vector.", true, nil)
	}
	scale := float32(1 / math.Sqrt(squares))
	for i := range vector {
		vector[i] *= scale
	}
	return nil
}
