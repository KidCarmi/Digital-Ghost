// Package config provides validated configuration loading for Digital Ghost.
// All fields have safe defaults. Invalid values are rejected at load time —
// there is no runtime fallback to an unsafe default.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config holds all runtime configuration for the DG daemon.
// Zero values are not used; always load via Load().
type Config struct {
	Capture        CaptureConfig        `yaml:"capture"`
	ResourceBudget ResourceBudgetConfig `yaml:"resource_budget"`
	Inference      InferenceConfig      `yaml:"inference"`
	SemanticFilter SemanticFilterConfig `yaml:"semantic_filter"`
	Storage        StorageConfig        `yaml:"storage"`
	Logging        LoggingConfig        `yaml:"logging"`
}

type CaptureConfig struct {
	FPS           float64 `yaml:"fps"`
	IdleFPS       float64 `yaml:"idle_fps"`
	QueueDepth    int     `yaml:"queue_depth"`
	HashThreshold int     `yaml:"hash_threshold"`
	// Backend selects the screen-capture backend.
	// "gdi"  — GDI BitBlt (default; works everywhere, higher CPU)
	// "dxgi" — DXGI Desktop Duplication (GPU-accelerated; requires D3D11)
	Backend string `yaml:"backend"`
}

type ResourceBudgetConfig struct {
	MaxCPUPct          int `yaml:"max_cpu_pct"`
	MaxGPUPct          int `yaml:"max_gpu_pct"`
	MaxInferencePerMin int `yaml:"max_inference_per_min"`
	CPUIdleThreshold   int `yaml:"cpu_idle_threshold"`
	GPUIdleThreshold   int `yaml:"gpu_idle_threshold"`
}

type InferenceConfig struct {
	OllamaURL   string `yaml:"ollama_url"`
	Model       string `yaml:"model"`
	// EmbedModel is the Ollama model used for text embeddings.
	// Defaults to "nomic-embed-text" which produces better semantic search
	// results than using the VLM (llava:7b) for embeddings.
	EmbedModel  string `yaml:"embed_model"`
	TimeoutSec  int    `yaml:"timeout_sec"`
	MaxRetries  int    `yaml:"max_retries"`
}

type SemanticFilterConfig struct {
	MinEngagementScore       float64 `yaml:"min_engagement_score"`
	MinDwellSeconds          float64 `yaml:"min_dwell_seconds"`
	GraphCoherenceThreshold  float64 `yaml:"graph_coherence_threshold"`
	IsolatedNodeScore        float64 `yaml:"isolated_node_score"`
}

type StorageConfig struct {
	DataDir         string `yaml:"data_dir"`
	RetentionDays   int    `yaml:"retention_days"`
	MaxRetentionDays int   `yaml:"max_retention_days"`
	SecureDelete    bool   `yaml:"secure_delete"`
}

type LoggingConfig struct {
	Level string `yaml:"level"`
	File  string `yaml:"file"`
}

// Defaults returns the safe baseline configuration.
// These match configs/default.yaml and are used if fields are absent in the user config.
func Defaults() Config {
	return Config{
		Capture: CaptureConfig{
			FPS:           2.0,
			IdleFPS:       0.5,
			QueueDepth:    100,
			HashThreshold: 10,
			Backend:       "gdi",
		},
		ResourceBudget: ResourceBudgetConfig{
			MaxCPUPct:          20,
			MaxGPUPct:          15,
			MaxInferencePerMin: 6,
			CPUIdleThreshold:   40,
			GPUIdleThreshold:   30,
		},
		Inference: InferenceConfig{
			OllamaURL:  "http://127.0.0.1:11434",
			Model:      "llava:7b",
			EmbedModel: "nomic-embed-text",
			TimeoutSec: 120, // llava:7b cold-start can take 60-90s on first load
			MaxRetries: 2,
		},
		SemanticFilter: SemanticFilterConfig{
			MinEngagementScore:      0.3,
			MinDwellSeconds:         3.0,
			GraphCoherenceThreshold: 0.4,
			IsolatedNodeScore:       0.8,
		},
		Storage: StorageConfig{
			DataDir:          "~/.local/share/digitalghost",
			RetentionDays:    90,
			MaxRetentionDays: 730,
			SecureDelete:     true,
		},
		Logging: LoggingConfig{
			Level: "info",
			File:  "~/.local/share/digitalghost/dg.log",
		},
	}
}

// Load reads the configuration file at path and merges it over Defaults().
// If path does not exist, Defaults() is returned.
// Returns an error if the file exists but is invalid or contains unsafe values.
func Load(path string) (*Config, error) {
	cfg := Defaults()

	expanded := expandHome(path)
	data, err := os.ReadFile(expanded)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No user config; use defaults.
			if verr := cfg.validate(); verr != nil {
				return nil, fmt.Errorf("default config is invalid: %w", verr)
			}
		} else {
			return nil, fmt.Errorf("reading config file %q: %w", expanded, err)
		}
	} else {
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("parsing config file %q: %w", expanded, err)
		}
		if err := cfg.validate(); err != nil {
			return nil, fmt.Errorf("invalid config: %w", err)
		}
	}

	cfg.Storage.DataDir = expandHome(cfg.Storage.DataDir)
	cfg.Logging.File = expandHome(cfg.Logging.File)

	return &cfg, nil
}

// validate rejects configurations with unsafe or nonsensical values.
func (c *Config) validate() error {
	if c.ResourceBudget.MaxCPUPct > 80 {
		return fmt.Errorf("resource_budget.max_cpu_pct (%d) exceeds 80%%: this would make DG a resource predator", c.ResourceBudget.MaxCPUPct)
	}
	if c.ResourceBudget.MaxCPUPct < 1 {
		return fmt.Errorf("resource_budget.max_cpu_pct must be at least 1")
	}
	if c.ResourceBudget.MaxGPUPct > 80 {
		return fmt.Errorf("resource_budget.max_gpu_pct (%d) exceeds 80%%", c.ResourceBudget.MaxGPUPct)
	}
	if c.Storage.RetentionDays > c.Storage.MaxRetentionDays {
		return fmt.Errorf("storage.retention_days (%d) exceeds max_retention_days (%d)", c.Storage.RetentionDays, c.Storage.MaxRetentionDays)
	}
	if c.Storage.MaxRetentionDays > 730 {
		return fmt.Errorf("storage.max_retention_days (%d) exceeds absolute maximum of 730 days (2 years)", c.Storage.MaxRetentionDays)
	}
	if c.Storage.RetentionDays < 1 {
		return fmt.Errorf("storage.retention_days must be at least 1")
	}
	if c.Capture.FPS <= 0 || c.Capture.FPS > 30 {
		return fmt.Errorf("capture.fps must be between 0 and 30, got %f", c.Capture.FPS)
	}
	if c.Capture.Backend != "" && c.Capture.Backend != "gdi" && c.Capture.Backend != "dxgi" {
		return fmt.Errorf("capture.backend must be \"gdi\" or \"dxgi\", got %q", c.Capture.Backend)
	}
	if c.Capture.QueueDepth < 10 || c.Capture.QueueDepth > 10000 {
		return fmt.Errorf("capture.queue_depth must be between 10 and 10000, got %d", c.Capture.QueueDepth)
	}
	if c.SemanticFilter.MinEngagementScore < 0 || c.SemanticFilter.MinEngagementScore > 1 {
		return fmt.Errorf("semantic_filter.min_engagement_score must be between 0 and 1")
	}
	if c.Inference.TimeoutSec < 5 || c.Inference.TimeoutSec > 300 {
		return fmt.Errorf("inference.timeout_sec must be between 5 and 300, got %d", c.Inference.TimeoutSec)
	}
	if c.Inference.OllamaURL == "" {
		return fmt.Errorf("inference.ollama_url must not be empty")
	}
	if u, err := url.Parse(c.Inference.OllamaURL); err != nil || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") {
		return fmt.Errorf("inference.ollama_url must point to localhost (got %q): Digital Ghost is air-gapped and must not send data to remote hosts", c.Inference.OllamaURL)
	}
	if c.Inference.Model == "" {
		return fmt.Errorf("inference.model must not be empty")
	}
	if c.Inference.EmbedModel == "" {
		return fmt.Errorf("inference.embed_model must not be empty")
	}
	return nil
}

// DataDirPath returns an absolute path within the configured data directory.
func (c *Config) DataDirPath(subpath string) string {
	return filepath.Join(c.Storage.DataDir, subpath)
}

func expandHome(path string) string {
	if len(path) == 0 {
		return path
	}
	if path[0] != '~' {
		return path
	}
	// Accept both ~/ (Unix) and ~\ (Windows), as well as bare ~
	if len(path) > 1 && path[1] != '/' && path[1] != '\\' {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if len(path) == 1 {
		return home
	}
	return filepath.Join(home, path[2:])
}
