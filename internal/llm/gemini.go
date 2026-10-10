package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/shahab5191/memshin/internal/memory"
	"google.golang.org/genai"
)

const (
	defaultGeminiModel    = "gemini-2.5-flash"
	defaultEmbeddingModel = "gemini-embedding-001"
	defaultGeminiTimeout  = 60 * time.Second

	// embeddingDimension is the fixed output dimensionality requested from the
	// embedding model. It must match the vector(N) column in the mid_term_memory
	// migration (00004).
	embeddingDimension = 768
)

// Embedding task types. The store-side and search-side embeddings must use the
// matching task type for the retrieval model to compare them meaningfully.
const (
	taskTypeRetrievalDocument = "RETRIEVAL_DOCUMENT"
	taskTypeRetrievalQuery    = "RETRIEVAL_QUERY"
)

// summarizeSystemPrompt asks the model to extract a list of atomic, self-contained
// facts from a batch, rather than replay the conversation verbatim.
const summarizeSystemPrompt = `You extract important, durable details from a batch of chat messages for a memory store.

Return JSON with a single field:
- "facts": an array of strings.

Each fact is one atomic, self-contained memory — a single piece of information that will be stored and recalled on its own, without the surrounding conversation. It must be complete in isolation: name people and things explicitly (never "he", "she", "it", "they", or "the user above"), and include enough detail to be meaningful on its own.

Rules:
- One fact per array item; do not merge unrelated details into one sentence.
- Do not repeat information across facts or restate the same thing in different words.
- State facts directly; do not narrate the conversation, restate questions, or include greetings and filler.
- Return an empty array if nothing in the batch is worth remembering.`

// summarizeSchema pins the extraction output to {"facts": [...]}, so the
// response is always well-formed JSON the caller can decode directly.
var summarizeSchema = &genai.Schema{
	Type: genai.TypeObject,
	Properties: map[string]*genai.Schema{
		"facts": {Type: genai.TypeArray, Items: &genai.Schema{Type: genai.TypeString}},
	},
	Required: []string{"facts"},
}

// focusSystemPrompt asks the model to track what the current conversation is
// about — its subject and the entities involved — returning whether to keep the
// existing focus and the focus that now holds.
const focusSystemPrompt = `You track the enduring macro-subject of an ongoing conversation that grows incrementally turn by turn.

You are given:
1. "CURRENT_FOCUS": The established subject from the previous evaluation.
2. "INCREMENTAL_CONVERSATION": The conversation window so far, which adds new messages at the end on each step.

Return JSON with exactly two fields:
- "keep": boolean
- "content": string

### EVALUATION RULES FOR INCREMENTAL WINDOWS:
1. **Cumulative Continuity:** As the conversation window grows longer with each step, look at the *entire arc* of the window, not just the newest messages at the end. 
2. **Sub-tasks do NOT invalidate the Macro-Subject:** Moving from core architecture to implementation details, testing harnesses, or writing a conference talk about the project are **sub-tasks**, not subject changes. Do not drop the primary project or system name (e.g., memshin) when a sub-task is introduced.
3. **When to update ("keep": false):** Only set "keep": false if the user completely abandons the current project/domain to start an entirely unrelated topic (e.g., stopping software engineering work to ask about cooking or travel).
4. **Content Formatting:** 
   - If "keep" is true: Return CURRENT_FOCUS verbatim. (Optionally, if a major phase shift occurred within the same project, you may refine it to include the active sub-goal, e.g., "Shahab is building memshin memory architecture and drafting a conference talk about it").
   - If "keep" is false: State the new macro-subject clearly. Never use ambiguous pronouns ("it", "this").

If CURRENT_FOCUS is empty, set "keep": false and generate the initial focus sentence.`;

// focusExtractSchema pins the extraction output to {"keep": bool, "content":
// string}. Both fields are required, so the response is always well-formed JSON
// the layer can decode directly.
var focusExtractSchema = &genai.Schema{
	Type: genai.TypeObject,
	Properties: map[string]*genai.Schema{
		"keep":    {Type: genai.TypeBoolean},
		"content": {Type: genai.TypeString},
	},
	Required: []string{"keep", "content"},
}

// GeminiConfig carries everything the provider needs. Zero values fall back to
// the defaults above so callers only set what they actually care about.
type GeminiConfig struct {
	APIKey string
	Model  string

	// EmbeddingModel is the model used for Embed; Model remains the chat model.
	EmbeddingModel string

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
		APIKey:         os.Getenv("GEMINI_API_KEY"),
		Model:          os.Getenv("GEMINI_MODEL"),
		EmbeddingModel: os.Getenv("EMBEDDING_MODEL"),
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
	client         *genai.Client
	model          string
	embeddingModel string
	timeout        time.Duration
	genCfg         *genai.GenerateContentConfig
}

// NewGemini builds a provider backed by the Gemini Developer API.
func NewGemini(ctx context.Context, cfg GeminiConfig) (*Gemini, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("gemini: api key is required")
	}
	if cfg.Model == "" {
		cfg.Model = defaultGeminiModel
	}
	if cfg.EmbeddingModel == "" {
		cfg.EmbeddingModel = defaultEmbeddingModel
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
		client:         client,
		model:          cfg.Model,
		embeddingModel: cfg.EmbeddingModel,
		timeout:        cfg.Timeout,
		genCfg: &genai.GenerateContentConfig{
			Temperature:     cfg.Temperature,
			MaxOutputTokens: cfg.MaxOutputTokens,
		},
	}, nil
}

func (g *Gemini) Name() string {
	return "Gemini/" + g.model
}

func (g *Gemini) GenerateResponse(ctx context.Context, chat *memory.ChatContext) (string, error) {
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

// Embed computes a 768-dimension embedding for text using the embedding model.
// taskType must be one of the retrieval task types; the store and search sides
// must use matching types for the retrieval model to compare them meaningfully.
func (g *Gemini) Embed(ctx context.Context, text, taskType string) ([]float32, error) {
	if text == "" {
		return nil, fmt.Errorf("%s: embed: empty text", g.Name())
	}

	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()

	resp, err := g.client.Models.EmbedContent(ctx, g.embeddingModel,
		genai.Text(text), &genai.EmbedContentConfig{
			TaskType:            taskType,
			OutputDimensionality: genai.Ptr[int32](embeddingDimension),
		})
	if err != nil {
		return nil, fmt.Errorf("%s: embed content: %w", g.Name(), err)
	}

	if len(resp.Embeddings) == 0 || len(resp.Embeddings[0].Values) == 0 {
		return nil, fmt.Errorf("%s: embed: no embedding returned", g.Name())
	}

	return resp.Embeddings[0].Values, nil
}

// Summarize distils a batch of messages into a list of atomic, self-contained
// facts using the default chat model.
func (g *Gemini) Summarize(ctx context.Context, text string) ([]string, error) {
	if text == "" {
		return nil, fmt.Errorf("%s: summarize: empty text", g.Name())
	}

	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()

	cfg := *g.genCfg // copy so the system instruction and schema do not leak across calls
	cfg.SystemInstruction = genai.NewContentFromText(summarizeSystemPrompt, genai.RoleUser)
	cfg.ResponseMIMEType = "application/json"
	cfg.ResponseSchema = summarizeSchema

	resp, err := g.client.Models.GenerateContent(ctx, g.model, genai.Text(text), &cfg)
	if err != nil {
		return nil, fmt.Errorf("%s: summarize: %w", g.Name(), err)
	}

	if fb := resp.PromptFeedback; fb != nil && fb.BlockReason != "" {
		return nil, fmt.Errorf("%s: summarize: prompt blocked: %s", g.Name(), fb.BlockReason)
	}
	if len(resp.Candidates) == 0 {
		return nil, fmt.Errorf("%s: summarize: no candidates in response", g.Name())
	}

	raw := strings.TrimSpace(resp.Text())
	if raw == "" {
		return nil, fmt.Errorf("%s: summarize: empty response", g.Name())
	}

	var out struct {
		Facts []string `json:"facts"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("%s: summarize: decode response: %w", g.Name(), err)
	}

	return out.Facts, nil
}

// Extract distils what the conversation is currently about — its subject and the
// entities involved — into a single concise focus sentence, deciding whether the
// existing focus still holds. text is the full short-term window (both sides of
// each exchange); currentSubject may be empty on the first extraction. Structured
// JSON output is enforced via a response schema.
func (g *Gemini) Extract(ctx context.Context, currentSubject, text string) (bool, string, error) {
	if text == "" {
		return false, "", fmt.Errorf("%s: extract: empty text", g.Name())
	}

	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()

	cfg := *g.genCfg // copy so the system instruction and schema do not leak across calls
	cfg.SystemInstruction = genai.NewContentFromText(focusSystemPrompt, genai.RoleUser)
	cfg.ResponseMIMEType = "application/json"
	cfg.ResponseSchema = focusExtractSchema

	resp, err := g.client.Models.GenerateContent(ctx, g.model, genai.Text(buildFocusPrompt(currentSubject, text)), &cfg)
	if err != nil {
		return false, "", fmt.Errorf("%s: extract: generate content: %w", g.Name(), err)
	}

	if fb := resp.PromptFeedback; fb != nil && fb.BlockReason != "" {
		return false, "", fmt.Errorf("%s: extract: prompt blocked: %s", g.Name(), fb.BlockReason)
	}
	if len(resp.Candidates) == 0 {
		return false, "", fmt.Errorf("%s: extract: no candidates in response", g.Name())
	}

	raw := strings.TrimSpace(resp.Text())
	if raw == "" {
		return false, "", fmt.Errorf("%s: extract: empty response", g.Name())
	}

	var out struct {
		Keep    bool   `json:"keep"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return false, "", fmt.Errorf("%s: extract: decode response: %w", g.Name(), err)
	}

	return out.Keep, out.Content, nil
}

// buildFocusPrompt hands the model the current subject alongside the window it
// must reduce to a single sentence, so it can decide to keep, refine, or replace.
func buildFocusPrompt(currentSubject, text string) string {
	var b strings.Builder
	b.WriteString("Current topic: ")
	if currentSubject == "" {
		b.WriteString("(none)")
	} else {
		b.WriteString(currentSubject)
	}
	b.WriteString("\n\nConversation:\n")
	b.WriteString(text)
	return b.String()
}

// buildPrompt puts the assembled memory ahead of the user's turn so the model
// reads the recalled context before the question it has to answer.
func buildPrompt(chat *memory.ChatContext) string {
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
