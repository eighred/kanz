package config

import (
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/pit"
)

// THE HORIZON/CADENCE PAIR IS VALIDATED AT STARTUP, NOT DISCOVERED IN THE
// STORE (#811).
//
// Retention in the point-in-time curve store is horizon/cadence versions per
// currency. Before the horizon existed it was uptime/cadence and nothing said
// so; the horizon closes that, and these arms are what stop the knob from being
// set to something that reopens it or that no pod can hold.
func TestCalibrationHorizon(t *testing.T) {
	for name, tc := range map[string]struct {
		raw      string
		interval time.Duration
		want     time.Duration
		wantErr  string
	}{
		"unset takes the documented default": {
			raw: "", interval: 5 * time.Minute, want: pit.DefaultHorizon,
		},
		"unset with the scheduler off still resolves": {
			raw: "", interval: 0, want: pit.DefaultHorizon,
		},
		"a valid duration is honoured": {
			raw: "48h", interval: time.Minute, want: 48 * time.Hour,
		},
		// parseDuration answers 0 for anything ParseDuration rejects, and 0 would
		// have become the default here — the operator sees their variable set and
		// gets a horizon they did not choose.
		"a malformed duration is refused": {
			raw: "7days", interval: time.Minute, wantErr: "is not a Go duration",
		},
		// The one that matters: an operator writing 0 to mean "no limit" would get
		// #811 back with the knob reading as configured.
		"zero is refused rather than meaning forever": {
			raw: "0s", interval: time.Minute, wantErr: "retain every calibrated curve",
		},
		"a negative horizon is refused": {
			raw: "-1h", interval: time.Minute, wantErr: "is not positive",
		},
		// 7d at 1s is ~605k curves per currency: a pod that reports healthy until
		// it is OOM-killed, with calibration latency climbing the whole way.
		"a cadence the horizon cannot hold is refused": {
			raw: "", interval: time.Second, wantErr: "above the",
		},
		// The ceiling is on the PAIR, so shortening the horizon rescues the same
		// cadence — which is what makes the refusal actionable rather than a ban
		// on fast calibration.
		"the same cadence passes under a shorter horizon": {
			raw: "1h", interval: time.Second, want: time.Hour,
		},
		// With the scheduler off nothing publishes versions, so the pair cannot
		// breach anything and must not refuse the start.
		"the scheduler being off skips the pair check": {
			raw: "8760h", interval: 0, want: 8760 * time.Hour,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("RISK_ENGINE_CALIBRATION_HORIZON", tc.raw)
			got, err := calibrationHorizon(tc.interval)
			switch {
			case tc.wantErr != "":
				if err == nil {
					t.Fatalf("calibrationHorizon(%s) with %q returned %s and no error, want an "+
						"error containing %q", tc.interval, tc.raw, got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q — the message is what tells the "+
						"operator which of the two numbers to change", err, tc.wantErr)
				}
			case err != nil:
				t.Fatalf("calibrationHorizon(%s) with %q: unexpected error %v", tc.interval, tc.raw, err)
			case got != tc.want:
				t.Fatalf("calibrationHorizon(%s) with %q = %s, want %s", tc.interval, tc.raw, got, tc.want)
			}
		})
	}
}

// THE REFUSAL NAMES BOTH NUMBERS AND BOTH VARIABLES. A ceiling breach is a
// statement about a pair, and an operator handed only one half has to guess
// which to change; the guess that shortens the horizon is the one that quietly
// costs point-in-time reads.
func TestCalibrationHorizonRefusalNamesBothSides(t *testing.T) {
	t.Setenv("RISK_ENGINE_CALIBRATION_HORIZON", "")
	_, err := calibrationHorizon(time.Second)
	if err == nil {
		t.Fatal("a 1s cadence under the default horizon was accepted")
	}
	for _, want := range []string{
		"RISK_ENGINE_CALIBRATION_INTERVAL",
		"RISK_ENGINE_CALIBRATION_HORIZON",
		"1s",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}
