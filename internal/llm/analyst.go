package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"google.golang.org/genai"
)

const (
	// Flash Lite because both calls are structured extraction, not reasoning,
	// and the decomposer sits on the request path ahead of the main generation.
	// Pinned rather than the gemini-flash-lite-latest alias: these two prompts
	// are tuned against one model's behaviour, and a silent version bump moves
	// extraction granularity underneath them.
	defaultAnalystModel = "gemini-3.5-flash-lite"

	// Short by design. The read path falls back to probing with the raw prompt
	// when this call does not land, so waiting on it past a second costs more
	// than the sharper probes are worth.
	defaultAnalystTimeout = 4 * time.Second
)

type AnalystConfig struct {
	APIKey  string
	Model   string
	Timeout time.Duration
}

func AnalystConfigFromEnv() (AnalystConfig, error) {
	cfg := AnalystConfig{
		APIKey: os.Getenv("GEMINI_API_KEY"),
		Model:  os.Getenv("GEMINI_ANALYST_MODEL"),
	}
	if cfg.APIKey == "" {
		return AnalystConfig{}, fmt.Errorf("analyst: GEMINI_API_KEY is not set")
	}

	if raw := os.Getenv("GEMINI_ANALYST_TIMEOUT"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return AnalystConfig{}, fmt.Errorf("analyst: parse GEMINI_ANALYST_TIMEOUT: %w", err)
		}
		cfg.Timeout = d
	}

	return cfg, nil
}

// Analyst is the small-model half of mid-term: it decomposes a transcript into
// facts on the way in, and a prompt into retrieval probes on the way out. Both
// directions deliberately share a granularity — a probe phrased like a stored
// fact lands near it, and one phrased unlike it does not.
type Analyst struct {
	client  *genai.Client
	model   string
	timeout time.Duration
}

func NewAnalyst(ctx context.Context, cfg AnalystConfig) (*Analyst, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("analyst: api key is required")
	}
	if cfg.Model == "" {
		cfg.Model = defaultAnalystModel
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultAnalystTimeout
	}

	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  cfg.APIKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, fmt.Errorf("analyst: create client: %w", err)
	}

	return &Analyst{client: client, model: cfg.Model, timeout: cfg.Timeout}, nil
}

func (a *Analyst) Name() string { return "Analyst/" + a.model }

// ExtractedFact is one atomic proposition worth remembering past the point the
// conversation that produced it leaves the short-term window.
type ExtractedFact struct {
	Content   string
	ValidFrom *time.Time
}

const extractInstruction = `You extract durable facts from a conversation transcript.

Rules:
- One fact per item. Never combine two claims into one item.
- Each fact must stand alone, with no pronouns and no reference to "the conversation".
  Write "the user is allergic to shellfish", not "they are allergic to it".
- Keep facts short: a single clause, phrased declaratively.
- Extract only what is durable: preferences, decisions, commitments, relationships,
  stable attributes, and constraints. Skip pleasantries, transient state, and
  anything true only for the current exchange.
- If the transcript says when something became true, set valid_from to that date in
  YYYY-MM-DD form. Leave it empty when the transcript does not say. Do not guess.
- Extract nothing rather than padding. An empty list is a valid answer.`

// ExtractFacts decomposes a promoted batch into the rows mid-term stores. It
// runs off the request path, in the promotion handler, so it is the one place
// here that can afford to be slow.
func (a *Analyst) ExtractFacts(ctx context.Context, transcript string) ([]ExtractedFact, error) {
	if strings.TrimSpace(transcript) == "" {
		return nil, nil
	}

	schema := &genai.Schema{
		Type: genai.TypeArray,
		Items: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"content": {
					Type:        genai.TypeString,
					Description: "The fact, as a single self-contained declarative clause.",
				},
				"valid_from": {
					Type:        genai.TypeString,
					Description: "YYYY-MM-DD when the fact became true, or empty if not stated.",
				},
			},
			Required: []string{"content", "valid_from"},
		},
	}

	var raw []struct {
		Content   string `json:"content"`
		ValidFrom string `json:"valid_from"`
	}
	if err := a.structured(ctx, extractInstruction, transcript, schema, &raw); err != nil {
		return nil, fmt.Errorf("%s: extract facts: %w", a.Name(), err)
	}

	facts := make([]ExtractedFact, 0, len(raw))
	for _, r := range raw {
		content := strings.TrimSpace(r.Content)
		if content == "" {
			continue
		}

		fact := ExtractedFact{Content: content}
		// A malformed date is dropped rather than rejected: the fact itself is
		// still worth storing, and readers fall back to ingestion time.
		if d := strings.TrimSpace(r.ValidFrom); d != "" {
			if t, err := time.Parse(time.DateOnly, d); err == nil {
				fact.ValidFrom = &t
			}
		}
		facts = append(facts, fact)
	}

	return facts, nil
}

const focusInstruction = `You track what a conversation is currently about.

You receive the latest exchange and the topics currently being tracked. Return the
topics under discussion as of this exchange.

Rules:
- When a tracked topic is still being discussed, repeat its phrase EXACTLY as given.
  Reusing the wording is what keeps it recognised as the same topic instead of
  accumulating near-duplicates beside it.
- Add a topic only when the exchange genuinely introduces one.
- Drop topics the conversation has moved on from. Being forgotten costs one turn
  of weaker context; a topic that lingers steers everything that follows toward
  something nobody is discussing.
- Phrase new topics as short noun phrases: "the mid-term schema", not "the user
  asked about how the mid-term schema should be laid out".
- Return at most 7. Prefer fewer.`

// ExtractFocus reports the topics under discussion after an exchange, given the
// ones already tracked.
func (a *Analyst) ExtractFocus(
	ctx context.Context,
	prompt, response string,
	current []string,
) ([]string, error) {
	if strings.TrimSpace(prompt) == "" {
		return nil, nil
	}

	schema := &genai.Schema{
		Type:  genai.TypeArray,
		Items: &genai.Schema{Type: genai.TypeString},
	}

	var input strings.Builder
	if len(current) > 0 {
		input.WriteString("Currently tracked topics:\n")
		for _, c := range current {
			input.WriteString("- ")
			input.WriteString(c)
			input.WriteByte('\n')
		}
	} else {
		input.WriteString("Currently tracked topics: none\n")
	}
	input.WriteString("\nLatest exchange:\nuser: ")
	input.WriteString(prompt)
	input.WriteString("\nassistant: ")
	input.WriteString(response)

	var phrases []string
	if err := a.structured(ctx, focusInstruction, input.String(), schema, &phrases); err != nil {
		return nil, fmt.Errorf("%s: extract focus: %w", a.Name(), err)
	}

	out := make([]string, 0, len(phrases))
	for _, p := range phrases {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}

	return out, nil
}

const decomposeInstruction = `You turn a user's message into search probes for a memory store
holding facts about that user from earlier conversations.

Rules:
- Resolve every pronoun and reference using the topics in focus. A probe that still
  says "that" or "the other one" is useless.
- Emit one probe per distinct thing worth looking up. A message asking about two
  subjects gets two probes.
- Phrase each probe as a short declarative statement about the user, the way a
  stored fact would read — "the user's dietary restrictions", not "what can they eat?".
- Return an empty list only when the message plainly cannot benefit from anything
  known about the user, such as a self-contained coding or formatting request.
  When in doubt, emit a probe: retrieving something unneeded is cheap, and failing
  to retrieve something needed is invisible.`

// DecomposeQuery turns the incoming turn into retrieval probes, resolving
// references against the topics currently in focus. An empty result means this
// turn has nothing to look up, and mid-term contributes no context to it.
func (a *Analyst) DecomposeQuery(ctx context.Context, prompt string, focus []string) ([]string, error) {
	if strings.TrimSpace(prompt) == "" {
		return nil, nil
	}

	schema := &genai.Schema{
		Type:  genai.TypeArray,
		Items: &genai.Schema{Type: genai.TypeString},
	}

	var input strings.Builder
	if len(focus) > 0 {
		input.WriteString("Topics currently in focus:\n")
		for _, f := range focus {
			input.WriteString("- ")
			input.WriteString(f)
			input.WriteByte('\n')
		}
		input.WriteByte('\n')
	}
	input.WriteString("User message:\n")
	input.WriteString(prompt)

	var probes []string
	if err := a.structured(ctx, decomposeInstruction, input.String(), schema, &probes); err != nil {
		return nil, fmt.Errorf("%s: decompose query: %w", a.Name(), err)
	}

	out := make([]string, 0, len(probes))
	for _, p := range probes {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}

	return out, nil
}

// structured runs one JSON-constrained call and unmarshals it into dst.
func (a *Analyst) structured(
	ctx context.Context,
	instruction, input string,
	schema *genai.Schema,
	dst any,
) error {
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	// Deterministic: both jobs are extraction, where sampling variety buys
	// nothing and costs reproducibility when tuning the prompts.
	resp, err := a.client.Models.GenerateContent(ctx, a.model, genai.Text(input),
		&genai.GenerateContentConfig{
			SystemInstruction: genai.NewContentFromText(instruction, genai.RoleUser),
			Temperature:       genai.Ptr[float32](0),
			ResponseMIMEType:  "application/json",
			ResponseSchema:    schema,
		})
	if err != nil {
		return fmt.Errorf("generate: %w", err)
	}

	if fb := resp.PromptFeedback; fb != nil && fb.BlockReason != "" {
		return fmt.Errorf("prompt blocked: %s", fb.BlockReason)
	}

	text := resp.Text()
	if text == "" {
		return fmt.Errorf("empty response")
	}
	if err := json.Unmarshal([]byte(text), dst); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}

	return nil
}
