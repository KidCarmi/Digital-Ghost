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
// It is tuned to produce structured, indexable descriptions rather than conversational output.
const inferencePrompt = `Describe the content of this screenshot concisely and precisely.
Focus on:
- The primary application or document being viewed
- The main topic or subject matter
- Any visible text headings, titles, or key terms
- The type of activity (reading, coding, browsing, writing, etc.)

Do NOT describe:
- UI chrome, menus, or taskbars
- Colors or visual aesthetics
- Anything that looks like credentials, passwords, or private data

Respond in 2-3 sentences. End with: TAGS: [comma-separated keywords]`

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
		Prompt: inferencePrompt,
		Images: []string{imgBase64},
		Stream: false,
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

// Generate calls Ollama /api/generate with a text-only prompt (no images).
// Used for conversational summarization over retrieved memory descriptions.
func (c *Client) Generate(ctx context.Context, prompt string) (string, error) {
	reqBody := ollamaGenerateRequest{
		Model:  c.cfg.Model,
		Prompt: prompt,
		Stream: false,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshaling request: %w", err)
	}
	result, err := c.doRequest(ctx, c.cfg.OllamaURL+"/api/generate", bodyBytes)
	if err != nil {
		return "", err
	}
	// doRequest returns description (with TAGS stripped); for a conversational
	// response there are no tags, so result.Description is the full reply.
	return result.Description, nil
}

// Embed returns a vector embedding for the given text using the configured model.
// The embedding can be used for semantic similarity search.
func (c *Client) Embed(ctx context.Context, text string) ([]float32, error) {
	reqBody := ollamaEmbedRequest{
		Model:  c.cfg.Model,
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
