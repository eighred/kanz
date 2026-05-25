package config

import (
	"errors"
	"log/slog"
	"os"
	"strings"
)

type Config struct {
	Listen      string
	DatabaseURL string
	LogLevel    slog.Level
}

func Load() (Config, error) {
	cfg := Config{
		Listen:      envOr("SCHEMA_REGISTRY_LISTEN", ":8080"),
		DatabaseURL: secret("SCHEMA_REGISTRY_DATABASE_URL"),
		LogLevel:    parseLevel(envOr("SCHEMA_REGISTRY_LOG_LEVEL", "info")),
	}
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("SCHEMA_REGISTRY_DATABASE_URL is required")
	}
	return cfg, nil
}

// secret resolves a sensitive value, preferring a CSI/Vault file mount
// (SEC-01d: the path in <k>_FILE) over a plaintext <k> env var, so the DSN is
// never plaintext in the pod spec or etcd. Empty (→ required-field error) when
// neither is set or the file path fails to read.
func secret(k string) string {
	if p := os.Getenv(k + "_FILE"); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return os.Getenv(k)
}

func envOr(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
