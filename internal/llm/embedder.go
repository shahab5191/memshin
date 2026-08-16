package llm

import (
	"context"
	"fmt"
	"math"
	"os"
	"time"

	"google.golang.org/genai"
)

// EmbeddingDimensions is fixed rather than configurable because the schema
// declares vector(768): changing it is a migration, not a setting, and a knob
// that silently breaks every insert is worse than no knob.
const EmbeddingDimensions = 768

const (
	defaultEmbedModel   = "gemini-embedding-001"
	defaultEmbedTimeout = 20 * time.Second
)

// Task types tell the model whether it is embedding something to be found or
// something to search with. Stored facts are declarative and queries are
// interrogative, so embedding both the same way leaves them further apart in
// the space than their meaning warrants.
const (
	taskDocument = "RETRIEVAL_DOCUMENT"
	taskQuery    = "RETRIEVAL_QUERY"
)

type EmbedderConfig struct {
	APIKey  string
	Model   string
	Timeout time.Duration
}

func EmbedderConfigFromEnv() (EmbedderConfig, error) {
	cfg := EmbedderConfig{
		APIKey: os.Getenv("GEMINI_API_KEY"),
		Model:  os.Getenv("GEMINI_EMBED_MODEL"),
	}
	if cfg.APIKey == "" {
		return EmbedderConfig{}, fmt.Errorf("embedder: GEMINI_API_KEY is not set")
	}
	return cfg, nil
}

type GeminiEmbedder struct {
	client  *genai.Client
	model   string
	timeout time.Duration
}

func NewGeminiEmbedder(ctx context.Context, cfg EmbedderConfig) (*GeminiEmbedder, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("embedder: api key is required")
	}
	if cfg.Model == "" {
		cfg.Model = defaultEmbedModel
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultEmbedTimeout
	}

	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  cfg.APIKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, fmt.Errorf("embedder: create client: %w", err)
	}

	return &GeminiEmbedder{client: client, model: cfg.Model, timeout: cfg.Timeout}, nil
}

func (e *GeminiEmbedder) Name() string { return "GeminiEmbedder/" + e.model }

// EmbedDocuments embeds facts on their way into storage.
func (e *GeminiEmbedder) EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error) {
	return e.embed(ctx, texts, taskDocument)
}

// EmbedQueries embeds retrieval probes. Every probe of a decomposed query goes
// in one call, so probe count costs request size rather than round trips.
func (e *GeminiEmbedder) EmbedQueries(ctx context.Context, texts []string) ([][]float32, error) {
	return e.embed(ctx, texts, taskQuery)
}

func (e *GeminiEmbedder) embed(ctx context.Context, texts []string, task string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	contents := make([]*genai.Content, 0, len(texts))
	for i, t := range texts {
		if t == "" {
			return nil, fmt.Errorf("%s: text %d is empty", e.Name(), i)
		}
		contents = append(contents, genai.NewContentFromText(t, genai.RoleUser))
	}

	dims := int32(EmbeddingDimensions)
	resp, err := e.client.Models.EmbedContent(ctx, e.model, contents, &genai.EmbedContentConfig{
		TaskType:             task,
		OutputDimensionality: &dims,
	})
	if err != nil {
		return nil, fmt.Errorf("%s: embed content: %w", e.Name(), err)
	}
	if len(resp.Embeddings) != len(texts) {
		return nil, fmt.Errorf("%s: got %d embeddings for %d texts",
			e.Name(), len(resp.Embeddings), len(texts))
	}

	out := make([][]float32, 0, len(texts))
	for i, emb := range resp.Embeddings {
		if len(emb.Values) != EmbeddingDimensions {
			return nil, fmt.Errorf("%s: embedding %d has %d dimensions, want %d",
				e.Name(), i, len(emb.Values), EmbeddingDimensions)
		}
		out = append(out, normalize(emb.Values))
	}

	return out, nil
}

// normalize scales a vector to unit length. Only the model's full-width output
// is guaranteed normalised; asking for fewer dimensions truncates it, and a
// truncated unit vector is no longer one. Cosine distance tolerates that but
// the inner-product operator does not, so normalising here keeps the choice of
// operator a retrieval decision rather than a correctness trap.
func normalize(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return v
	}

	norm := math.Sqrt(sum)
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(float64(x) / norm)
	}
	return out
}
