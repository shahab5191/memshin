package llm

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/shahab5191/memshin/internal/pipeline"
	"google.golang.org/genai"
)

const (
	defaultGeminiModel   = "gemini-2.5-flash"
	defaultGeminiTimeout = 60 * time.Second
)

// GeminiConfig carries everything the provider needs. Zero values fall back to
// the defaults above so callers only set what they actually care about.
type GeminiConfig struct {
	APIKey string
	Model  string

	// Timeout bounds a single GenerateResponse call. The caller's context still
	// wins if it expires first.
	Timeout time.Duration

	// Temperature is a pointer so "unset" stays distinguishable from 0, which is
	// a meaningful (fully deterministic) value.
	Temperature     *float32
	MaxOutputTokens int32
}

// GeminiConfigFromEnv reads the provider settings from the environment. Only
// the API key is required; the rest keep their defaults when unset.
func GeminiConfigFromEnv() (GeminiConfig, error) {
	cfg := GeminiConfig{
		APIKey: os.Getenv("GEMINI_API_KEY"),
		Model:  os.Getenv("GEMINI_MODEL"),
	}
	if cfg.APIKey == "" {
		return GeminiConfig{}, fmt.Errorf("gemini: GEMINI_API_KEY is not set")
	}

	if raw := os.Getenv("GEMINI_TEMPERATURE"); raw != "" {
		t, err := strconv.ParseFloat(raw, 32)
		if err != nil {
			return GeminiConfig{}, fmt.Errorf("gemini: parse GEMINI_TEMPERATURE: %w", err)
		}
		cfg.Temperature = genai.Ptr(float32(t))
	}

	if raw := os.Getenv("GEMINI_MAX_OUTPUT_TOKENS"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 32)
		if err != nil {
			return GeminiConfig{}, fmt.Errorf("gemini: parse GEMINI_MAX_OUTPUT_TOKENS: %w", err)
		}
		cfg.MaxOutputTokens = int32(n)
	}

	return cfg, nil
}

type Gemini struct {
	client  *genai.Client
	model   string
	timeout time.Duration
	genCfg  *genai.GenerateContentConfig
}

// NewGemini builds a provider backed by the Gemini Developer API.
func NewGemini(ctx context.Context, cfg GeminiConfig) (*Gemini, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("gemini: api key is required")
	}
	if cfg.Model == "" {
		cfg.Model = defaultGeminiModel
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultGeminiTimeout
	}

	// Backend is pinned explicitly: the SDK otherwise switches to Vertex AI when
	// GOOGLE_GENAI_USE_VERTEXAI happens to be set in the environment.
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  cfg.APIKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, fmt.Errorf("gemini: create client: %w", err)
	}

	return &Gemini{
		client:  client,
		model:   cfg.Model,
		timeout: cfg.Timeout,
		genCfg: &genai.GenerateContentConfig{
			Temperature:     cfg.Temperature,
			MaxOutputTokens: cfg.MaxOutputTokens,
		},
	}, nil
}

func (g *Gemini) Name() string {
	return "Gemini/" + g.model
}

func (g *Gemini) GenerateResponse(ctx context.Context, chat *pipeline.ChatContext) (string, error) {
	if chat == nil {
		return "", fmt.Errorf("%s: nil chat context", g.Name())
	}

	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()

	cfg := *g.genCfg // copy: the per-call system instruction must not leak across calls
	if chat.SystemMessage != "" {
		cfg.SystemInstruction = genai.NewContentFromText(chat.SystemMessage, genai.RoleUser)
	}

	resp, err := g.client.Models.GenerateContent(ctx, g.model, genai.Text(buildPrompt(chat)), &cfg)
	if err != nil {
		return "", fmt.Errorf("%s: generate content: %w", g.Name(), err)
	}

	if fb := resp.PromptFeedback; fb != nil && fb.BlockReason != "" {
		return "", fmt.Errorf("%s: prompt blocked: %s", g.Name(), fb.BlockReason)
	}
	if len(resp.Candidates) == 0 {
		return "", fmt.Errorf("%s: no candidates in response", g.Name())
	}

	// A non-STOP finish reason with no text means the model produced nothing
	// usable — surface it instead of handing an empty string to the memory layers.
	text := resp.Text()
	if text == "" {
		reason := resp.Candidates[0].FinishReason
		if reason == "" || reason == genai.FinishReasonStop {
			return "", fmt.Errorf("%s: empty response", g.Name())
		}
		return "", fmt.Errorf("%s: empty response (finish reason %s)", g.Name(), reason)
	}

	return text, nil
}

// buildPrompt puts the assembled memory ahead of the user's turn so the model
// reads the recalled context before the question it has to answer.
func buildPrompt(chat *pipeline.ChatContext) string {
	memory := chat.RenderMemory()
	if memory == "" {
		return chat.OriginalPrompt
	}

	var b strings.Builder
	b.WriteString(memory)
	b.WriteByte('\n')
	b.WriteString(chat.OriginalPrompt)
	return b.String()
}
