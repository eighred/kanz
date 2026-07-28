package config

import (
	"errors"
	"log/slog"
	"os"
	"strings"

	"github.com/eighred/kanz/pkg/secret"
)

type Config struct {
	Listen      string
	DatabaseURL string
	LogLevel    slog.Level
}

func Load() (Config, error) {
	// The DSN comes from a CSI/Vault file mount (SEC-01d) in preference to a
	// plaintext env var, so it never rides in the pod spec or etcd. Resolved
	// before the literal so an unreadable mount stops Load HERE, with the path
	// named: the local helper this replaces returned "" for that case, which the
	// required-field check below then reported as "SCHEMA_REGISTRY_DATABASE_URL is
	// required" — sending an operator to look for missing config when the config
	// was present and the mount was broken. See pkg/secret.
	databaseURL, err := secret.Read("SCHEMA_REGISTRY_DATABASE_URL")
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		Listen:      envOr("SCHEMA_REGISTRY_LISTEN", ":8080"),
		DatabaseURL: databaseURL,
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
