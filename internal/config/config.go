// Package config loads the optional .dowse.toml from a repo root.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Model      string   `toml:"model"`
	Dimensions int      `toml:"dimensions"`
	Ignore     []string `toml:"ignore"`
	OllamaURL  string   `toml:"ollama_url"`
}

func Default() Config {
	return Config{
		Model:      "embeddinggemma",
		Dimensions: 768,
		OllamaURL:  "http://localhost:11434",
	}
}

// Load reads <root>/.dowse.toml if present and fills in defaults.
// OLLAMA_HOST overrides the configured URL, matching the ollama CLI.
func Load(root string) (Config, error) {
	cfg := Default()
	b, err := os.ReadFile(filepath.Join(root, ".dowse.toml"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return cfg, err
	}
	if err == nil {
		if _, err := toml.Decode(string(b), &cfg); err != nil {
			return cfg, fmt.Errorf(".dowse.toml: %w", err)
		}
	}
	if h := os.Getenv("OLLAMA_HOST"); h != "" {
		if filepath.IsAbs(h) || !hasScheme(h) {
			h = "http://" + h
		}
		cfg.OllamaURL = h
	}
	switch cfg.Dimensions {
	case 768, 512, 256, 128:
	default:
		return cfg, fmt.Errorf(".dowse.toml: dimensions must be 768, 512, 256 or 128, got %d", cfg.Dimensions)
	}
	return cfg, nil
}

func hasScheme(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			return i+2 < len(s) && s[i+1] == '/' && s[i+2] == '/'
		}
	}
	return false
}
