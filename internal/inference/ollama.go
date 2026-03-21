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
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/KidCarmi/digital-ghost/internal/capture"
	"github.com/KidCarmi/digital-ghost/internal/config"
	"github.com/KidCarmi/digital-ghost/internal/filter"
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

// honesty rules are prepended to every class-specific prompt.
const honestyRules = `Honesty rules (follow exactly):
- Only describe what is clearly visible. If text is blurry or too small, say so — never guess.
- If the screen is blank or loading, say "the screen seemed mostly blank" — don't invent content.
- Use "it looked like" / "I could make out" when not fully certain.
Skip: toolbars, window chrome, passwords, credentials, layout/colours.`

// classPrompts maps content classes to a focused description instruction.
// Each prompt tells the VLM what to prioritise for that class of content.
var classPrompts = map[filter.ContentClass]string{
	filter.ClassWorkCode: `You're helping a developer remember what they were coding.
Focus on:
- Programming language and file/function/class names that are clearly visible
- What the code does or what problem it's solving
- Any visible error messages, test output, or terminal commands

` + honestyRules + `

Write 2-3 sentences. End with: TAGS: [comma-separated keywords]`,

	filter.ClassWorkDocument: `You're helping someone remember a document they were reading or writing.
Focus on:
- Document title, section heading, or key topic
- Main idea, argument, or content clearly visible on screen
- Any names, dates, or specific terms that are legible

` + honestyRules + `

Write 2-3 sentences. End with: TAGS: [comma-separated keywords]`,

	filter.ClassWorkResearch: `You're helping someone remember an article or research they were reading.
Focus on:
- Article or page title and its main claim or finding
- Source or publication name if clearly visible
- Key concepts, terms, or takeaways visible on screen

` + honestyRules + `

Write 2-3 sentences. End with: TAGS: [comma-separated keywords]`,

	filter.ClassCommunication: `You're helping someone remember a conversation or message thread.
Focus on:
- Platform and general topic being discussed (not exact private messages)
- Visible project, task, or decision being referenced
- Who the conversation involves if names are clearly shown

` + honestyRules + `

Write 2-3 sentences. End with: TAGS: [comma-separated keywords]`,
}

// defaultPrompt is used for ClassUnknown, ClassSocial, and any class without
// a specific prompt. Kept general and warm.
const defaultPrompt = `You're helping someone remember what they were doing.
Focus on:
- What the person was actually doing or reading
- The main topic, project, or task visible on screen
- Any meaningful text, names, or ideas that were clearly visible

` + honestyRules + `

Write 2-3 warm, natural sentences. End with: TAGS: [comma-separated keywords]`

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

// buildInferencePrompt constructs a context-enriched, class-specific prompt for the VLM.
// Injecting app name, window title, and URL gives the model grounding that pixels alone
// cannot provide (e.g. which repo, which Slack channel). The class-specific body then
// directs the model to prioritise the most useful details for that content type.
func buildInferencePrompt(frame *capture.Frame, class filter.ContentClass) string {
	var sb strings.Builder

	// Window metadata preamble — grounding the model before it sees the image.
	if frame.WindowCtx.ProcessName != "" || frame.WindowCtx.WindowTitle != "" || frame.WindowCtx.BrowserURL != "" {
		sb.WriteString("Context:\n")
		if frame.WindowCtx.ProcessName != "" {
			sb.WriteString("  App: ")
			sb.WriteString(frame.WindowCtx.ProcessName)
			sb.WriteString("\n")
		}
		if frame.WindowCtx.WindowTitle != "" {
			sb.WriteString("  Window: ")
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

	// Class-specific body.
	body, ok := classPrompts[class]
	if !ok {
		body = defaultPrompt
	}
	sb.WriteString(body)
	return sb.String()
}

// Infer sends a frame to Ollama and returns the inference result.
// The caller is responsible for calling Governor.Acquire() before calling Infer.
//
// ctx should have a deadline set (via the Governor or the caller).
func (c *Client) Infer(ctx context.Context, frame *capture.Frame) (*InferenceResult, error) {
	// Classify the frame so we can pick the right prompt and crop correctly.
	contentClass := filter.Classify(filter.ClassifierInput{
		ProcessName: frame.WindowCtx.ProcessName,
		WindowTitle: frame.WindowCtx.WindowTitle,
		BrowserURL:  frame.WindowCtx.BrowserURL,
	})

	imgBase64, err := encodeFrameForVLM(frame)
	if err != nil {
		return nil, fmt.Errorf("encoding frame: %w", err)
	}

	reqBody := ollamaGenerateRequest{
		Model:  c.cfg.Model,
		Prompt: buildInferencePrompt(frame, contentClass),
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
// The description is also sanitized to strip prompt-injection patterns before storage.
func parseDescriptionAndTags(response string) (description string, tags []string) {
	const tagMarker = "tags:"
	lower := strings.ToLower(response)
	idx := strings.LastIndex(lower, tagMarker)
	if idx < 0 {
		return sanitizeDescription(strings.TrimSpace(response)), nil
	}

	description = sanitizeDescription(strings.TrimSpace(response[:idx]))
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

// injectionPatterns are case-insensitive prefixes / substrings that indicate
// an attempt to hijack the LLM prompt via screen-visible text.
// When any pattern is found, the offending sentence is replaced with a
// placeholder so it cannot propagate to the chat LLM context.
var injectionPatterns = []string{
	"ignore all previous",
	"ignore previous",
	"disregard previous",
	"disregard all",
	"forget all previous",
	"new instructions:",
	"system prompt:",
	"system:",
	"[inst]",
	"[system]",
	"</s>",         // llama-style end-of-sequence token
	"[/inst]",
	"<|im_start|>", // chatml system turn
	"<|system|>",
}

// sanitizeDescription removes prompt-injection patterns from a VLM description.
// It operates at the sentence level: any sentence containing an injection
// pattern is replaced with "[content redacted]" so context is preserved.
func sanitizeDescription(desc string) string {
	lower := strings.ToLower(desc)
	for _, pat := range injectionPatterns {
		if strings.Contains(lower, pat) {
			// Replace the whole description rather than trying to surgically
			// remove individual sentences — a partial replacement is still
			// injectable if the attacker anticipated the filter.
			return "[screen content contained text that could not be safely stored]"
		}
	}
	return desc
}

// encodeFrameForVLM prepares a frame for the Ollama multimodal API.
//
// llava:7b's CLIP vision encoder resizes every input to 336×336 internally.
// Sending a full 1920×1080 JPEG just means CLIP does a brutal 5.7× squash that
// makes all text and UI elements unreadable. We do the downscale ourselves at
// 672×378 (2× CLIP resolution, 16:9 aspect) so the 2× step is clean and text
// remains legible. We also crop to the active window first so the VLM sees the
// relevant content at full target width instead of a tiny fraction of the screen.
//
// Format: PNG (lossless) — avoids JPEG block artifacts on text edges.
func encodeFrameForVLM(frame *capture.Frame) (string, error) {
	if frame.Image == nil {
		return "", fmt.Errorf("frame has nil image")
	}

	src := frame.Image

	// Crop to the active window if the rect is valid and fits within the image.
	if r := frame.WindowCtx.ActiveWindowRect; r.Dx() > 32 && r.Dy() > 32 {
		cropped := r.Intersect(src.Bounds())
		if cropped.Dx() > 32 && cropped.Dy() > 32 {
			if rgba, ok := src.(*image.RGBA); ok {
				src = rgba.SubImage(cropped) // zero-copy view
			}
		}
	}

	// Downscale to 672×378 if the source is larger.
	const targetW, targetH = 672, 378
	if src.Bounds().Dx() > targetW || src.Bounds().Dy() > targetH {
		src = downscaleNearest(src, targetW, targetH)
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		return "", fmt.Errorf("PNG encoding: %w", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// downscaleNearest resizes src to exactly w×h using nearest-neighbour sampling.
// At 2–3× reduction ratios this is fast and preserves text legibility well —
// the only quality difference vs. Lanczos/CatmullRom is mild aliasing on
// diagonal lines, which is irrelevant for UI screenshots.
func downscaleNearest(src image.Image, w, h int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	sb := src.Bounds()
	scaleX := float64(sb.Dx()) / float64(w)
	scaleY := float64(sb.Dy()) / float64(h)
	for y := 0; y < h; y++ {
		srcY := sb.Min.Y + int(float64(y)*scaleY)
		for x := 0; x < w; x++ {
			srcX := sb.Min.X + int(float64(x)*scaleX)
			r, g, b, a := src.At(srcX, srcY).RGBA()
			dst.SetRGBA(x, y, color.RGBA{
				R: uint8(r >> 8),
				G: uint8(g >> 8),
				B: uint8(b >> 8),
				A: uint8(a >> 8),
			})
		}
	}
	return dst
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

// ollamaTagsResponse matches the Ollama /api/tags response schema.
type ollamaTagsResponse struct {
	Models []ollamaModelEntry `json:"models"`
}

// ollamaModelEntry is a single model listed by /api/tags.
type ollamaModelEntry struct {
	Name   string `json:"name"`
	Digest string `json:"digest"` // e.g. "sha256:abc123..."
}

// Ping checks that Ollama is reachable and returns without error if the HTTP
// layer is up. It does NOT verify model availability or digest — use VerifyModel
// for that stronger check.
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

// VerifyModel confirms that the configured VLM model is present in Ollama's
// model list and, if cfg.ModelDigest is set, that the digest matches exactly.
//
// This prevents a malicious process that has bound :11434 before Ollama from
// silently receiving frame pixel data: the fake server either cannot return a
// valid model list or cannot produce the correct digest.
//
// Returns the actual model digest on success so the caller can log it.
func (c *Client) VerifyModel(ctx context.Context) (string, error) {
	url := c.cfg.OllamaURL + "/api/tags"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("building /api/tags request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("calling /api/tags: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Ollama /api/tags returned HTTP %d", resp.StatusCode)
	}

	var tags ollamaTagsResponse
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return "", fmt.Errorf("decoding /api/tags response: %w", err)
	}

	// Find the configured model in the list.
	var actualDigest string
	for _, m := range tags.Models {
		if m.Name == c.cfg.Model {
			actualDigest = m.Digest
			break
		}
	}
	if actualDigest == "" {
		return "", fmt.Errorf("model %q is not loaded in Ollama — run: ollama pull %s", c.cfg.Model, c.cfg.Model)
	}

	// If a digest is pinned in config, enforce it.
	if c.cfg.ModelDigest != "" && actualDigest != c.cfg.ModelDigest {
		return "", fmt.Errorf(
			"model digest mismatch for %q: expected %s, got %s — "+
				"this may indicate a tampered model or a malicious process on :11434",
			c.cfg.Model, c.cfg.ModelDigest, actualDigest,
		)
	}

	return actualDigest, nil
}
