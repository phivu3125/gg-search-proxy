package config

import (
	"os"
	"strings"
	"time"
)

// Config holds all runtime configuration, sourced from environment variables.
type Config struct {
	Addr            string        // GGSP_ADDR, default ":20129"
	DBPath          string        // GGSP_DB, default "data/ggsearch.db"
	AdminToken      string        // GGSP_ADMIN_TOKEN, enables /v1 admin endpoints
	UpstreamTimeout time.Duration // GGSP_UPSTREAM_TIMEOUT, default 20s
	DefaultModel    string        // GGSP_DEFAULT_MODEL, default "gemini-2.5-flash"
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Load reads configuration from the environment, first importing KEY=VALUE
// pairs from a .env file in the working directory (real env vars win).
func Load() *Config {
	loadDotEnv(".env")
	timeout := 20 * time.Second
	if v := os.Getenv("GGSP_UPSTREAM_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			timeout = d
		}
	}
	return &Config{
		Addr:            envOr("GGSP_ADDR", ":20129"),
		DBPath:          envOr("GGSP_DB", "data/ggsearch.db"),
		AdminToken:      os.Getenv("GGSP_ADMIN_TOKEN"),
		UpstreamTimeout: timeout,
		DefaultModel:    envOr("GGSP_DEFAULT_MODEL", "gemini-2.5-flash"),
	}
}

// loadDotEnv parses a minimal .env format: KEY=VALUE lines, # comments,
// optional `export ` prefix, surrounding single/double quotes stripped.
// Variables already present in the environment are never overridden.
func loadDotEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return // missing file is fine
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if n := len(value); n >= 2 {
			first, last := value[0], value[n-1]
			if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
				value = value[1 : n-1]
			}
		}
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue // real environment wins over .env
		}
		_ = os.Setenv(key, value)
	}
}
