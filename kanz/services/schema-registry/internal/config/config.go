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
		DatabaseURL: os.Getenv("SCHEMA_REGISTRY_DATABASE_URL"),
		LogLevel:    parseLevel(envOr("SCHEMA_REGISTRY_LOG_LEVEL", "info")),
	}
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("SCHEMA_REGISTRY_DATABASE_URL is required")
	}
	return cfg, nil
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
