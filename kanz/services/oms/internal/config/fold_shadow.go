package config

import (
	"errors"
	"github.com/eighred/kanz/services/oms/internal/foldshadow"
	"os"
	"strconv"
	"time"
)

func fillFoldShadow() (foldshadow.Config, error) {
	const p = "OMS_RUST_FILL_SHADOW_"
	c := foldshadow.Config{Executable: os.Getenv(p + "EXECUTABLE"), ExpectedVersion: os.Getenv(p + "EXPECTED_VERSION")}
	if s, ok := os.LookupEnv(p + "ENABLED"); ok {
		v, e := strconv.ParseBool(s)
		if e != nil {
			return c, errors.New("invalid OMS Rust fill shadow enabled")
		}
		c.Enabled = v
	}
	if s, ok := os.LookupEnv(p + "TIMEOUT"); ok {
		v, e := time.ParseDuration(s)
		if e != nil {
			return c, errors.New("invalid OMS Rust fill shadow timeout")
		}
		c.Timeout = v
	} else if c.Enabled {
		c.Timeout = 5 * time.Second
	}
	return c, c.Validate()
}
