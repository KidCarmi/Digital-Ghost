// ollama.go implements the Ollama HTTP client for Vision-Language Model inference.
//
// All Ollama communication is via http://127.0.0.1:11434 (local only).
// No frames or descriptions leave the machine.
//
// The client:
//   - Encodes frames as base64 JPEG before sending (Ollama multimodal API format)
//   - Enforces a per-request timeout (default: 30s)
//   - Retries on transient errors with a fixed backoff
//   - Returns a structured InferenceResult with description and extracted tags
package inference

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/jpeg"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/KidCarmi/digital-ghost/internal/capture"
	"github.com/KidCarmi/digital-ghost/internal/config"
)

// InferenceResult is the output of a single VLM inference call.
type InferenceResult struct {
	// Description is the VLM's natural-language description of the frame content.
	Description string

	// Tags are keywords extracted from the description for graph indexing.
	Tags []string

	// Model is the Ollama model that produced this result.
	Model string

	// LatencyMS is the inference round-trip time in milliseconds.
	LatencyMS int64
}

// Client is a thread-safe Ollama HTTP client.
type Client struct {
	cfg    config.InferenceConfig
	http   *http.Client
	logger *slog.Logger
}

// ollamaGenerateRequest matches the Ollama /api/generate endpoint schema.
type ollamaGenerateRequest struct {
	Model   string         `json:"model"`
	Prompt  string         `json:"prompt"`
	Images  []string       `json:"images"` // base64-encoded images
	Stream  bool           `json:"stream"`
	Options map[string]any `json:"options,omitempty"`
}

// ollamaGenerateResponse matches the Ollama /api/generate response schema.
type ollamaGenerateResponse struct {
	Response string `json:"response"`
	Done     bool   `json:"done"`
	Error    string `json:"error,omitempty"`
}

// inferencePrompt is the prompt sent to the VLM.
// Tuned to produce warm, personal memory notes that read like a thoughtful
// human recall rather than a robotic screen description.
// Uncertainty rules prevent fabrication: if the image is unclear, the model
// must say so rather than guess.
const inferencePrompt = `You're helping someone remember what they were doing. Write a short, warm memory note — like a friend describing what they noticed on the screen, in plain conversational English.

CRITICAL — honesty rules (follow these exactly):
- Only describe what you can actually see clearly. If text is blurry, small, or hard to read, say "there was some text I couldn't quite make out" rather than guessing what it said.
- If the screen is mostly blank, a loading spinner, or unrecognisable, say "the screen seemed mostly blank or loading" — don't invent content.
- Never name specific people, companies, or projects unless the name is clearly legible in the image.
- Use phrases like "it looked like", "there seemed to be", "I could make out" when you're not fully certain.

Focus on (only what's clearly visible):
- What the person was actually doing or reading
- The main topic, project, or task they seemed to be working on
- Any meaningful text, code, names, or ideas that were clearly visible

Skip entirely:
- Toolbars, window chrome, UI widgets, and menu bars
- Layout, colors, and visual design
- Anything that looks like passwords, credentials, or private data

Write 2-3 warm, natural sentences — like a memory you'd want to find later. End with: TAGS: [comma-separated keywords]`

// NewClient creates an Ollama client from configuration.
func NewClient(cfg config.InferenceConfig, logger *slog.Logger) *Client {
	return &Client{
		cfg: cfg,
		http: &http.Client{
			Timeout: time.Duration(cfg.TimeoutSec) * time.Second,
		},
		logger: logger,
	}
}

// buildInferencePrompt constructs a context-enriched prompt for the VLM.
// Injecting app name, window title, and URL gives the model grounding that
// pixels alone cannot provide (e.g. which Python version, which GitHub repo,
// which Slack channel) and produces significantly more specific descriptions.
func buildInferencePrompt(frame *capture.Frame) string {
	var sb strings.Builder
	if frame.WindowCtx.ProcessName != "" || frame.WindowCtx.WindowTitle != "" || frame.WindowCtx.BrowserURL != "" {
		sb.WriteString("Context about what's on screen:\n")
		if frame.WindowCtx.ProcessName != "" {
			sb.WriteString("  App: ")
			sb.WriteString(frame.WindowCtx.ProcessName)
			sb.WriteString("\n")
		}
		if frame.WindowCtx.WindowTitle != "" {
			sb.WriteString("  Window title: ")
			sb.WriteString(frame.WindowCtx.WindowTitle)
			sb.WriteString("\n")
		}
		if frame.WindowCtx.BrowserURL != "" {
			sb.WriteString("  URL: ")
			sb.WriteString(frame.WindowCtx.BrowserURL)
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}
	sb.WriteString(inferencePrompt)
	return sb.String()
}

// Infer sends a frame to Ollama and returns the inference result.
// The caller is responsible for calling Governor.Acquire() before calling Infer.
//
// ctx should have a deadline set (via the Governor or the caller).
func (c *Client) Infer(ctx context.Context, frame *capture.Frame) (*InferenceResult, error) {
	imgBase64, err := encodeFrameJPEG(frame)
	if err != nil {
		return nil, fmt.Errorf("encoding frame: %w", err)
	}

	reqBody := ollamaGenerateRequest{
		Model:  c.cfg.Model,
		Prompt: buildInferencePrompt(frame),
		Images: []string{imgBase64},
		Stream: false,
		// temperature:0 makes the model deterministic and anchors it to the
		// most-likely tokens — significantly reduces hallucination on visual
		// content that is ambiguous or partially visible.
		Options: map[string]any{"temperature": 0},
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	url := c.cfg.OllamaURL + "/api/generate"

	var result *InferenceResult
	var lastErr error

	for attempt := 0; attempt <= c.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}

		start := time.Now()
		result, lastErr = c.doRequest(ctx, url, bodyBytes)
		elapsed := time.Since(start)

		if lastErr == nil {
			result.LatencyMS = elapsed.Milliseconds()
			c.logger.Debug("inference complete",
				"model", c.cfg.Model,
				"latency_ms", result.LatencyMS,
				"tags", result.Tags)
			return result, nil
		}

		c.logger.Warn("inference attempt failed",
			"attempt", attempt+1,
			"max", c.cfg.MaxRetries+1,
			"error", lastErr)
	}

	return nil, fmt.Errorf("inference failed after %d attempts: %w", c.cfg.MaxRetries+1, lastErr)
}

// doRequest performs a single HTTP request to Ollama and parses the response.
func (c *Client) doRequest(ctx context.Context, url string, body []byte) (*InferenceResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("Ollama returned HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var ollamaResp ollamaGenerateResponse
	if err := json.NewDecoder(resp.Body).Decode(&ollamaResp); err != nil {
		return nil, fmt.Errorf("decoding Ollama response: %w", err)
	}
	if ollamaResp.Error != "" {
		return nil, fmt.Errorf("Ollama error: %s", ollamaResp.Error)
	}

	description, tags := parseDescriptionAndTags(ollamaResp.Response)

	return &InferenceResult{
		Description: description,
		Tags:        tags,
		Model:       c.cfg.Model,
	}, nil
}

// parseDescriptionAndTags splits the VLM response into a description and keyword tags.
// The VLM is prompted to end with "TAGS: [kw1, kw2, ...]".
func parseDescriptionAndTags(response string) (description string, tags []string) {
	const tagMarker = "tags:"
	lower := strings.ToLower(response)
	idx := strings.LastIndex(lower, tagMarker)
	if idx < 0 {
		return strings.TrimSpace(response), nil
	}

	description = strings.TrimSpace(response[:idx])
	tagLine := strings.TrimSpace(response[idx+len(tagMarker):])

	// Strip surrounding brackets if present.
	tagLine = strings.Trim(tagLine, "[]")
	for _, t := range strings.Split(tagLine, ",") {
		t = strings.TrimSpace(t)
		if t != "" {
			tags = append(tags, t)
		}
	}
	return description, tags
}

// encodeFrameJPEG encodes the frame's image as a base64 JPEG string.
func encodeFrameJPEG(frame *capture.Frame) (string, error) {
	if frame.Image == nil {
		return "", fmt.Errorf("frame has nil image")
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, frame.Image, &jpeg.Options{Quality: 85}); err != nil {
		return "", fmt.Errorf("JPEG encoding: %w", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// ollamaEmbedRequest matches the Ollama /api/embeddings endpoint schema.
type ollamaEmbedRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

// ollamaEmbedResponse matches the Ollama /api/embeddings response schema.
type ollamaEmbedResponse struct {
	Embedding []float32 `json:"embedding"`
}

// chatModel returns the model to use for text-only chat generation.
// Falls back to the VLM model if ChatModel is not configured.
func (c *Client) chatModel() string {
	if c.cfg.ChatModel != "" {
		return c.cfg.ChatModel
	}
	return c.cfg.Model
}

// Generate calls Ollama /api/generate with a text-only prompt (no images).
// Used for conversational summarization over retrieved memory descriptions.
// Uses ChatModel (if configured) instead of the VLM for better text quality.
func (c *Client) Generate(ctx context.Context, prompt string) (string, error) {
	reqBody := ollamaGenerateRequest{
		Model:  c.chatModel(),
		Prompt: prompt,
		Stream: false,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshaling request: %w", err)
	}

	// Do the HTTP call directly rather than through doRequest, because doRequest
	// calls parseDescriptionAndTags which would silently truncate any chat
	// response that contains the word "tags:" (e.g. "Here are the main tags:...").
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.OllamaURL+"/api/generate", bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("creating chat request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("chat HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("Ollama chat returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	var ollamaResp ollamaGenerateResponse
	if err := json.NewDecoder(resp.Body).Decode(&ollamaResp); err != nil {
		return "", fmt.Errorf("decoding chat response: %w", err)
	}
	if ollamaResp.Error != "" {
		return "", fmt.Errorf("Ollama chat error: %s", ollamaResp.Error)
	}
	return strings.TrimSpace(ollamaResp.Response), nil
}

// Embed returns a vector embedding for the given text using the configured embed model.
// Uses EmbedModel (default: nomic-embed-text) rather than the VLM — a dedicated
// embedding model produces significantly better semantic search results.
func (c *Client) Embed(ctx context.Context, text string) ([]float32, error) {
	reqBody := ollamaEmbedRequest{
		Model:  c.cfg.EmbedModel,
		Prompt: text,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshaling embed request: %w", err)
	}

	url := c.cfg.OllamaURL + "/api/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("creating embed request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("Ollama embed returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	var embedResp ollamaEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&embedResp); err != nil {
		return nil, fmt.Errorf("decoding embed response: %w", err)
	}
	if len(embedResp.Embedding) == 0 {
		return nil, fmt.Errorf("Ollama returned empty embedding")
	}
	return embedResp.Embedding, nil
}

// Ping checks that Ollama is reachable and the configured model is available.
func (c *Client) Ping(ctx context.Context) error {
	url := c.cfg.OllamaURL + "/api/tags"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("Ollama unreachable at %s: %w", c.cfg.OllamaURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Ollama ping returned HTTP %d", resp.StatusCode)
	}
	return nil
}
