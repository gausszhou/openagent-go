package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	openaisdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/openai/openai-go/v3/shared"

	openagent "github.com/yusheng-g/openagent-go"

	"github.com/yusheng-g/openagent-go/utils"
)

// Model implements openagent.Model via openai-go v3.
type Model struct {
	client        openaisdk.Client
	modelID       string
	contextWindow int
}

// New creates a Model with the given API key, model ID, and base URL.
// The context window is automatically detected from the model ID. Call
// WithContextWindow to override.
//
// baseURL may be empty (the SDK default — api.openai.com — is used):
// option.WithBaseURL("") would shadow the SDK default with an unusable
// empty URL, so it is only applied when non-empty.
func New(apiKey, modelID, baseURL string) *Model {
	opts := []option.RequestOption{
		option.WithAPIKey(apiKey),
		option.WithHTTPClient(&http.Client{
			Timeout: 5 * time.Minute,
		}),
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	return &Model{
		client:        openaisdk.NewClient(opts...),
		modelID:       modelID,
		contextWindow: modelContextWindow(modelID),
	}
}

func (m *Model) WithContextWindow(tokens int) *Model { m.contextWindow = tokens; return m }
func (m *Model) ContextWindow() int                  { return m.contextWindow }

// ListModels queries the provider's /models endpoint for the list of
// available models. Works against any OpenAI-compatible base URL (OpenAI,
// OpenRouter, Together, Groq, Ollama, ...). The endpoint path is relative
// to the Model's baseURL — the SDK issues GET <baseURL>/models.
//
// The standard OpenAI response carries only id/created/owned_by. Other
// fields are resolved from the built-in lookup table. Providers that add
// extension fields in the response override the table:
//   - OpenRouter: context_length (= max input tokens),
//     top_provider.max_completion_tokens, pricing.{prompt,completion}
//     (strings, USD per 1M tokens — converted to per-token here).
//
// Returns models sorted by ID.
func (m *Model) ListModels(ctx context.Context) ([]openagent.AvailableModel, error) {
	page, err := m.client.Models.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	out := make([]openagent.AvailableModel, 0, len(page.Data))
	for _, mo := range page.Data {
		am := openagent.AvailableModel{
			ID:             mo.ID,
			OwnedBy:        mo.OwnedBy,
			MaxInputTokens: utils.ModelContextWindow(mo.ID),
		}
		if raw := mo.RawJSON(); raw != "" {
			applyExtensionFields(&am, raw)
		}
		out = append(out, am)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// applyExtensionFields fills AvailableModel fields from provider-specific
// extensions in the raw /models JSON. Only non-zero response values
// override the table-derived defaults.
func applyExtensionFields(am *openagent.AvailableModel, rawJSON string) {
	// OpenRouter shape: top-level context_length, top_provider.{...},
	// pricing.{prompt,completion} as strings (USD per 1M tokens).
	var probe struct {
		ContextLength int `json:"context_length"`
		TopProvider   struct {
			MaxCompletionTokens int `json:"max_completion_tokens"`
		} `json:"top_provider"`
		Pricing struct {
			Prompt     string `json:"prompt"`
			Completion string `json:"completion"`
		} `json:"pricing"`
	}
	if err := json.Unmarshal([]byte(rawJSON), &probe); err != nil {
		return
	}
	if probe.ContextLength > 0 {
		am.MaxInputTokens = probe.ContextLength
	}
	if probe.TopProvider.MaxCompletionTokens > 0 {
		am.MaxOutputTokens = probe.TopProvider.MaxCompletionTokens
	}
	// OpenRouter prices are USD per 1M tokens — same unit as the fields.
	if v := parsePerMillionPrice(probe.Pricing.Prompt); v > 0 {
		am.InputCostPerMillion = v
	}
	if v := parsePerMillionPrice(probe.Pricing.Completion); v > 0 {
		am.OutputCostPerMillion = v
	}
}

// parsePerMillionPrice parses a price string expressed as USD per 1M tokens.
// Returns 0 on parse failure or non-positive.
func parsePerMillionPrice(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 {
		return 0
	}
	return f
}
// TokenizerModel returns a tiktoken-encodable canonical name for the
// model, not the user-assigned model ID (which tiktoken cannot map).
// o1/o3 reasoning models use the o200k encoding; everything else uses
// cl100k.
func (m *Model) TokenizerModel() string {
	// o1/o3/gpt-4o use the o200k encoding; everything else maps to
	// cl100k. cl100k overcounts CJK by ~60%, so gpt-4o sessions would
	// compact prematurely without this branch.
	if strings.HasPrefix(m.modelID, "o1") || strings.HasPrefix(m.modelID, "o3") ||
		strings.HasPrefix(m.modelID, "gpt-4o") {
		return "gpt-4o"
	}
	return "gpt-4"
}

// modelContextWindow resolves the context window for a model ID. Falls
// back to the shared model.ContextWindow lookup table.
func modelContextWindow(modelID string) int {
	if modelID == "" {
		return 0
	}
	return utils.ModelContextWindow(modelID)
}

// Ensure *Model implements the optional TokenizerModeler interface.
var _ openagent.TokenizerModeler = (*Model)(nil)
var _ openagent.ModelLister = (*Model)(nil)

func (m *Model) ChatCompletion(ctx context.Context, req openagent.ChatCompletionRequest) (*openagent.ChatCompletionResponse, error) {
	modelID := req.Model
	if modelID == "" {
		modelID = m.modelID
	}

	params := openaisdk.ChatCompletionNewParams{
		Model:    openaisdk.ChatModel(modelID),
		Messages: toSDKMessages(req.Messages),
		Tools:    toSDKTools(req.Tools),
	}
	if req.Temperature != nil {
		params.Temperature = param.NewOpt(*req.Temperature)
	}
	if req.MaxTokens != 0 {
		params.MaxTokens = param.NewOpt(int64(req.MaxTokens))
	}
	if req.TopP != nil {
		params.TopP = param.NewOpt(*req.TopP)
	}
	if len(req.Stop) > 0 {
		params.Stop = openaisdk.ChatCompletionNewParamsStopUnion{
			OfStringArray: req.Stop,
		}
	}
	if req.ReasoningEffort != "" {
		params.ReasoningEffort = shared.ReasoningEffort(req.ReasoningEffort)
	}

	completion, err := m.client.Chat.Completions.New(ctx, params)
	if err != nil {
		return nil, toRetryableError(err)
	}
	return toResponse(completion), nil
}

// ChatCompletionStream implements openagent.Model.
func (m *Model) ChatCompletionStream(ctx context.Context, req openagent.ChatCompletionRequest) (openagent.StreamReader, error) {
	modelID := req.Model
	if modelID == "" {
		modelID = m.modelID
	}

	params := openaisdk.ChatCompletionNewParams{
		Model:    openaisdk.ChatModel(modelID),
		Messages: toSDKMessages(req.Messages),
		Tools:    toSDKTools(req.Tools),
	}
	if req.Temperature != nil {
		params.Temperature = param.NewOpt(*req.Temperature)
	}
	if req.MaxTokens != 0 {
		params.MaxTokens = param.NewOpt(int64(req.MaxTokens))
	}
	if req.TopP != nil {
		params.TopP = param.NewOpt(*req.TopP)
	}
	if len(req.Stop) > 0 {
		params.Stop = openaisdk.ChatCompletionNewParamsStopUnion{
			OfStringArray: req.Stop,
		}
	}
	if req.ReasoningEffort != "" {
		params.ReasoningEffort = shared.ReasoningEffort(req.ReasoningEffort)
	}
	// Streaming responses omit usage by default; request it explicitly so
	// the accumulated RunResult carries token counts.
	params.StreamOptions = openaisdk.ChatCompletionStreamOptionsParam{
		IncludeUsage: param.NewOpt(true),
	}

	stream := m.client.Chat.Completions.NewStreaming(ctx, params)
	if err := stream.Err(); err != nil {
		return nil, toRetryableError(err)
	}
	return &streamReader{stream: stream}, nil
}

// toRetryableError wraps transient API errors (429 + server 5xx) so the
// Runner can retry with backoff. The Retry-After header, when present,
// becomes the explicit backoff hint (RFC 7231; seconds or HTTP date).
func toRetryableError(err error) error {
	var apiErr *openaisdk.Error
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case 429, 500, 502, 503, 504:
			re := &openagent.RetryableError{Err: err}
			if apiErr.Response != nil {
				if v := apiErr.Response.Header.Get("Retry-After"); v != "" {
					if d, derr := http.ParseTime(v); derr == nil {
						re.RetryAfter = time.Until(d)
					} else if sec, serr := strconv.Atoi(v); serr == nil {
						re.RetryAfter = time.Duration(sec) * time.Second
					}
				}
			}
			return re
		}
	}
	return fmt.Errorf("chat completion: %w", err)
}

// ── openagent → SDK ──

func toSDKMessages(msgs []openagent.Message) []openaisdk.ChatCompletionMessageParamUnion {
	out := make([]openaisdk.ChatCompletionMessageParamUnion, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, toSDKMessage(m))
	}
	return out
}

func toSDKMessage(m openagent.Message) openaisdk.ChatCompletionMessageParamUnion {
	switch m.Role {
	case openagent.RoleSystem:
		return openaisdk.SystemMessage(m.Content)

	case openagent.RoleUser:
		if m.IsMultimodal() {
			return openaisdk.UserMessage(toSDKContentParts(m.ContentParts))
		}
		return openaisdk.UserMessage(m.Content)

	case openagent.RoleAssistant:
		if len(m.ToolCalls) > 0 {
			assistant := &openaisdk.ChatCompletionAssistantMessageParam{
				ToolCalls: toSDKToolCallParams(m.ToolCalls),
			}
			if m.Content != "" {
				assistant.Content = openaisdk.ChatCompletionAssistantMessageParamContentUnion{
					OfString: openaisdk.String(m.Content),
				}
			}
			if m.ReasoningContent != "" {
				assistant.SetExtraFields(map[string]any{
					"reasoning_content": m.ReasoningContent,
				})
			}
			return openaisdk.ChatCompletionMessageParamUnion{OfAssistant: assistant}
		}
		assistant := &openaisdk.ChatCompletionAssistantMessageParam{
			Content: openaisdk.ChatCompletionAssistantMessageParamContentUnion{
				OfString: openaisdk.String(m.Content),
			},
		}
		if m.ReasoningContent != "" {
			assistant.SetExtraFields(map[string]any{
				"reasoning_content": m.ReasoningContent,
			})
		}
		return openaisdk.ChatCompletionMessageParamUnion{OfAssistant: assistant}

	case openagent.RoleTool:
		return openaisdk.ToolMessage(m.Content, m.ToolCallID)

	default:
		return openaisdk.UserMessage(m.Content)
	}
}

func toSDKContentParts(parts []openagent.ContentPart) []openaisdk.ChatCompletionContentPartUnionParam {
	out := make([]openaisdk.ChatCompletionContentPartUnionParam, len(parts))
	for i, p := range parts {
		switch p.Type {
		case "text":
			out[i] = openaisdk.TextContentPart(p.Text)
		case "image_url":
			out[i] = openaisdk.ImageContentPart(openaisdk.ChatCompletionContentPartImageImageURLParam{
				URL: p.ImageURL.URL,
			})
		default:
			// An unknown part type would serialize as an empty {} and be
			// silently dropped by the API — drop it explicitly instead.
			slog.Warn("unsupported content part type, dropping", "type", p.Type)
		}
	}
	return out
}

func toSDKToolCallParams(calls []openagent.ToolCall) []openaisdk.ChatCompletionMessageToolCallUnionParam {
	out := make([]openaisdk.ChatCompletionMessageToolCallUnionParam, len(calls))
	for i, c := range calls {
		out[i] = openaisdk.ChatCompletionMessageToolCallUnionParam{
			OfFunction: &openaisdk.ChatCompletionMessageFunctionToolCallParam{
				ID: c.ID,
				Function: openaisdk.ChatCompletionMessageFunctionToolCallFunctionParam{
					Name:      c.Function.Name,
					Arguments: c.Function.Arguments,
				},
			},
		}
	}
	return out
}

// toSDKTools serializes the tool set for the API, deduplicated by name
// (the LAST definition wins — mode switches rebind plan/execution tools,
// so later registrations carry the current intent) and stable-sorted by
// name so the same tool set always serializes to the same order. A
// stable tools prefix keeps the prompt-cache prefix stable across turns
// and retries; duplicate names also break strict providers (DeepSeek
// rejects "Tool names must be unique").
func toSDKTools(defs []openagent.FunctionDefinition) []openaisdk.ChatCompletionToolUnionParam {
	last := make(map[string]int, len(defs)) // name → index of the last definition
	for i, d := range defs {
		last[d.Name] = i
	}
	uniq := make([]openagent.FunctionDefinition, 0, len(last))
	for i, d := range defs {
		if last[d.Name] == i {
			uniq = append(uniq, d)
		}
	}
	sort.SliceStable(uniq, func(i, j int) bool { return uniq[i].Name < uniq[j].Name })

	out := make([]openaisdk.ChatCompletionToolUnionParam, len(uniq))
	for i, d := range uniq {
		params := d.Parameters.SchemaMap()
		out[i] = openaisdk.ChatCompletionToolUnionParam{
			OfFunction: &openaisdk.ChatCompletionFunctionToolParam{
				Function: openaisdk.FunctionDefinitionParam{
					Name:        d.Name,
					Description: param.NewOpt(d.Description),
					Parameters:  openaisdk.FunctionParameters(params),
				},
			},
		}
	}
	return out
}

// ── SDK → openagent ──

func toResponse(c *openaisdk.ChatCompletion) *openagent.ChatCompletionResponse {
	resp := &openagent.ChatCompletionResponse{}
	for _, choice := range c.Choices {
		msg := openagent.Message{
			Role:             openagent.RoleAssistant,
			Content:          choice.Message.Content,
			ReasoningContent: extractReasoning(choice.Message.RawJSON()),
		}
		for _, tc := range choice.Message.ToolCalls {
			msg.ToolCalls = append(msg.ToolCalls, openagent.ToolCall{
				ID:   tc.ID,
				Type: tc.Type,
				Function: openagent.ToolCallFunction{
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				},
			})
		}
		resp.Choices = append(resp.Choices, openagent.Choice{
			Index:        int(choice.Index),
			Message:      msg,
			FinishReason: choice.FinishReason,
		})
	}
	if c.Usage.TotalTokens > 0 {
		resp.Usage = openagent.Usage{
			PromptTokens:     int(c.Usage.PromptTokens),
			CompletionTokens: int(c.Usage.CompletionTokens),
			TotalTokens:      int(c.Usage.TotalTokens),
			CacheReadTokens:  int(c.Usage.PromptTokensDetails.CachedTokens),
		}
	}
	return resp
}

// ── Stream wrapper ──

// streamReader adapts ssestream.Stream to openagent.StreamReader.
type streamReader struct {
	stream  *ssestream.Stream[openaisdk.ChatCompletionChunk]
	current openagent.StreamChunk
	done    bool
}

func (s *streamReader) Next() bool {
	if s.done {
		return false
	}
	if !s.stream.Next() {
		s.done = true
		return false
	}
	s.current = toStreamChunk(s.stream.Current())
	return true
}

func (s *streamReader) Current() openagent.StreamChunk { return s.current }
func (s *streamReader) Err() error {
	err := s.stream.Err()
	// Wrap transient failures so the runner can retry mid-stream:
	//   - 429/503 API errors (backpressure / overload)
	//   - io.ErrUnexpectedEOF / net.Error (connection cut mid-chunk —
	//     local model servers and flaky gateways truncate streams)
	// Non-retryable errors are returned as-is to preserve error semantics.
	var apiErr *openaisdk.Error
	if errors.As(err, &apiErr) && (apiErr.StatusCode == 429 || apiErr.StatusCode == 503) {
		return &openagent.RetryableError{Err: err}
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return &openagent.RetryableError{Err: err}
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return &openagent.RetryableError{Err: err}
	}
	return err
}
func (s *streamReader) Close() error { return s.stream.Close() }

func toStreamChunk(c openaisdk.ChatCompletionChunk) openagent.StreamChunk {
	sc := openagent.StreamChunk{}
	if c.Usage.TotalTokens > 0 {
		sc.Usage = &openagent.Usage{
			PromptTokens:     int(c.Usage.PromptTokens),
			CompletionTokens: int(c.Usage.CompletionTokens),
			TotalTokens:      int(c.Usage.TotalTokens),
			CacheReadTokens:  int(c.Usage.PromptTokensDetails.CachedTokens),
		}
	}
	for _, choice := range c.Choices {
		sd := openagent.StreamDelta{
			Content:          choice.Delta.Content,
			ReasoningContent: extractReasoning(choice.Delta.RawJSON()),
			FinishReason:     choice.FinishReason,
		}
		for _, tc := range choice.Delta.ToolCalls {
			sd.ToolCalls = append(sd.ToolCalls, openagent.ToolCallDelta{
				Index: int(tc.Index),
				ID:    tc.ID,
				Type:  tc.Type,
				Function: openagent.FunctionDelta{
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				},
			})
		}
		sc.Choices = append(sc.Choices, sd)
	}
	return sc
}

// extractReasoning extracts "reasoning_content" from raw JSON. The openai-go
// SDK doesn't have a typed field for it (as of v3.41), but reasoning models
// (o1, deepseek-r1) include it in delta chunks and message responses.
func extractReasoning(raw string) string {
	if raw == "" {
		return ""
	}
	var delta struct {
		ReasoningContent *string `json:"reasoning_content"`
	}
	if err := json.Unmarshal([]byte(raw), &delta); err != nil {
		return ""
	}
	if delta.ReasoningContent != nil && *delta.ReasoningContent != "" {
		return *delta.ReasoningContent
	}
	return ""
}
