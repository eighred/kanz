package config

import (
	"errors"
	"os"
	"strconv"
	"time"

	"github.com/eighred/kanz/services/risk-engine/internal/calibrationshadow"
)

func juliaShadow() (calibrationshadow.Config, error) {
	const prefix = "RISK_ENGINE_JULIA_SHADOW_"
	c := calibrationshadow.Config{Executable: os.Getenv(prefix + "EXECUTABLE"), Project: os.Getenv(prefix + "PROJECT"), Script: os.Getenv(prefix + "SCRIPT"), Depot: os.Getenv(prefix + "DEPOT")}
	if s, ok := os.LookupEnv(prefix + "ENABLED"); ok {
		v, e := strconv.ParseBool(s)
		if e != nil {
			return c, errors.New("invalid Julia shadow enabled flag")
		}
		c.Enabled = v
	}
	if s, ok := os.LookupEnv(prefix + "TIMEOUT"); ok {
		v, e := time.ParseDuration(s)
		if e != nil {
			return c, errors.New("invalid Julia shadow timeout")
		}
		c.Timeout = v
	} else if c.Enabled {
		c.Timeout = 30 * time.Second
	}
	if e := c.Validate(); e != nil {
		return c, e
	}
	if c.Enabled && (parseDuration(os.Getenv("RISK_ENGINE_CALIBRATION_INTERVAL")) <= 0 || os.Getenv("RISK_ENGINE_CALIBRATION_RATES") == "" || os.Getenv("RISK_ENGINE_NATS_URL") == "") {
		return c, errors.New("Julia shadow requires scheduled curve calibration and NATS")
	}
	return c, nil
}
