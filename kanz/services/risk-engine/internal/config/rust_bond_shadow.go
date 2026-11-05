package config

import (
	"errors"
	"os"
	"strconv"
	"time"

	"github.com/eighred/kanz/services/risk-engine/internal/bondshadow"
)

func rustBondShadow() (bondshadow.Config, error) {
	const prefix = "RISK_ENGINE_RUST_BOND_SHADOW_"
	c := bondshadow.Config{Executable: os.Getenv(prefix + "EXECUTABLE"), ExpectedVersion: os.Getenv(prefix + "EXPECTED_VERSION")}
	if s, ok := os.LookupEnv(prefix + "ENABLED"); ok {
		v, e := strconv.ParseBool(s)
		if e != nil {
			return c, errors.New("invalid Rust bond shadow enabled flag")
		}
		c.Enabled = v
	}
	if s, ok := os.LookupEnv(prefix + "TIMEOUT"); ok {
		v, e := time.ParseDuration(s)
		if e != nil {
			return c, errors.New("invalid Rust bond shadow timeout")
		}
		c.Timeout = v
	} else if c.Enabled {
		c.Timeout = 5 * time.Second
	}
	if e := c.Validate(); e != nil {
		return c, e
	}
	if c.Enabled && (parseDuration(os.Getenv("RISK_ENGINE_CALIBRATION_INTERVAL")) <= 0 || os.Getenv("RISK_ENGINE_CALIBRATION_RATES") == "" || os.Getenv("RISK_ENGINE_NATS_URL") == "") {
		return c, errors.New("Rust bond shadow requires active fixed-income curve calibration and NATS")
	}
	return c, nil
}
