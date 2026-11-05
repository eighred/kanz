package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestJuliaShadowConfigDefaultAndRefusal(t *testing.T) {
	for _, key := range []string{"ENABLED", "EXECUTABLE", "PROJECT", "SCRIPT", "DEPOT", "TIMEOUT"} {
		t.Setenv("RISK_ENGINE_JULIA_SHADOW_"+key, "")
	}
	// An explicitly blank boolean is invalid; absent is the default-off posture.
	if _, e := juliaShadow(); e == nil {
		t.Fatal("blank enabled accepted")
	}
	t.Setenv("RISK_ENGINE_JULIA_SHADOW_ENABLED", "false")
	t.Setenv("RISK_ENGINE_JULIA_SHADOW_TIMEOUT", "0s")
	if c, e := juliaShadow(); e != nil || c.Enabled {
		t.Fatalf("disabled: %v", e)
	}
	t.Setenv("RISK_ENGINE_JULIA_SHADOW_EXECUTABLE", "relative")
	if _, e := juliaShadow(); e == nil {
		t.Fatal("partial disabled config ignored")
	}
	t.Setenv("RISK_ENGINE_JULIA_SHADOW_ENABLED", "true")
	if _, e := juliaShadow(); e == nil {
		t.Fatal("incomplete enabled config accepted")
	}
}

func TestJuliaShadowRequiresScheduledCalibration(t *testing.T) {
	project := t.TempDir()
	if e := os.Mkdir(filepath.Join(project, "bin"), 0700); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"Project.toml", "Manifest.toml", "bin/curve_worker.jl", "julia"} {
		if e := os.WriteFile(filepath.Join(project, name), nil, 0600); e != nil {
			t.Fatal(e)
		}
	}
	for key, value := range map[string]string{"ENABLED": "true", "EXECUTABLE": filepath.Join(project, "julia"), "PROJECT": project, "SCRIPT": filepath.Join(project, "bin", "curve_worker.jl"), "DEPOT": t.TempDir(), "TIMEOUT": "30s"} {
		t.Setenv("RISK_ENGINE_JULIA_SHADOW_"+key, value)
	}
	t.Setenv("RISK_ENGINE_CALIBRATION_INTERVAL", "")
	t.Setenv("RISK_ENGINE_CALIBRATION_RATES", "")
	t.Setenv("RISK_ENGINE_NATS_URL", "")
	if _, e := juliaShadow(); e == nil {
		t.Fatal("enabled shadow without scheduler accepted")
	}
	t.Setenv("RISK_ENGINE_CALIBRATION_INTERVAL", "1m")
	t.Setenv("RISK_ENGINE_CALIBRATION_RATES", "fixture")
	t.Setenv("RISK_ENGINE_NATS_URL", "nats://fixture.invalid")
	if c, e := juliaShadow(); e != nil || !c.Enabled {
		t.Fatalf("complete fixture config: %v", e)
	}
}
