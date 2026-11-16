package config

import (
	"errors"
	"github.com/eighred/kanz/internal/env"
	"log/slog"

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
		Listen:      env.Or("SCHEMA_REGISTRY_LISTEN", ":8080"),
		DatabaseURL: databaseURL,
		LogLevel:    env.ParseLevelOr(env.Or("SCHEMA_REGISTRY_LOG_LEVEL", "info"), slog.LevelInfo),
	}
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("SCHEMA_REGISTRY_DATABASE_URL is required")
	}
	return cfg, nil
}
