package env_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/env"
)

// A MALFORMED VALUE IS AN ERROR, NEVER THE DEFAULT (#692).
//
// The defect this replaced returned the default on a parse failure, so a typo
// left a service on a schedule the operator did not choose while the deployment
// reported a clean start. Two of the five keys it governed in datamaster —
// DATAMASTER_DUAL_CONTROL_TTL and DATAMASTER_LAPSED_PROPOSAL_RETENTION — bound
// how long a dual-control proposal stays approvable, so the silent fallback
// changed an authorization window with nothing said.

func TestDurationUnsetIsTheDefault(t *testing.T) {
	t.Setenv("KANZ_TEST_DUR", "")
	got, err := env.Duration("KANZ_TEST_DUR", 5*time.Minute)
	if err != nil {
		t.Fatalf("unset key returned an error: %v", err)
	}
	if got != 5*time.Minute {
		t.Errorf("got %s, want the default 5m", got)
	}
}

func TestDurationReadsAValue(t *testing.T) {
	t.Setenv("KANZ_TEST_DUR", "90s")
	got, err := env.Duration("KANZ_TEST_DUR", 5*time.Minute)
	if err != nil {
		t.Fatalf("Duration: %v", err)
	}
	if got != 90*time.Second {
		t.Errorf("got %s, want 90s", got)
	}
}

// THE MUTATION #692 NAMES, at the level of the helper: the value the issue used
// as its example must not become the default.
func TestDurationRefusesAMalformedValue(t *testing.T) {
	t.Setenv("KANZ_TEST_DUR", "5minutes")
	got, err := env.Duration("KANZ_TEST_DUR", 42*time.Hour)
	if err == nil {
		t.Fatalf("a malformed duration returned %s with no error — the caller is now on a "+
			"schedule nobody chose, and the deployment reports a clean start", got)
	}
	if got != 0 {
		t.Errorf("got %s alongside the error; a caller ignoring err would use it", got)
	}
	// The message has to name the key AND the value: an operator reading a crash
	// log needs to know which of five intervals was wrong and what it said.
	for _, want := range []string{"KANZ_TEST_DUR", "5minutes"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q: %v", want, err)
		}
	}
}

// A WELL-FORMED NON-POSITIVE VALUE IS RETURNED, NOT REFUSED.
//
// The old copy folded `err != nil || d <= 0` into one branch, so an unparseable
// value and an explicit 0 were the same outcome — and on DATAMASTER_OUTBOX_INTERVAL
// the default IS 0, which made a typo indistinguishable from the intended
// setting. Whether zero is meaningful is the caller's decision; refusing a bad
// parse is this function's.
func TestDurationPassesAWellFormedZeroThrough(t *testing.T) {
	t.Setenv("KANZ_TEST_DUR", "0s")
	got, err := env.Duration("KANZ_TEST_DUR", time.Hour)
	if err != nil {
		t.Fatalf("a well-formed zero was refused: %v", err)
	}
	if got != 0 {
		t.Errorf("got %s, want 0 — the caller asked for zero and must be able to tell that "+
			"apart from a typo", got)
	}
}

// WHITESPACE IS TRIMMED, because a YAML block scalar and a mounted file both
// deliver one. Lookup already does it; this pins that Duration goes through it
// rather than reading os.Getenv itself.
func TestDurationTrimsWhitespace(t *testing.T) {
	t.Setenv("KANZ_TEST_DUR", "  30s\n")
	got, err := env.Duration("KANZ_TEST_DUR", time.Hour)
	if err != nil {
		t.Fatalf("a padded value was refused: %v", err)
	}
	if got != 30*time.Second {
		t.Errorf("got %s, want 30s", got)
	}
}

// A BLANK VALUE IS UNSET, not a parse failure — the convention every manifest
// in this estate already encodes by writing `value: ""` for "leave it alone".
func TestDurationTreatsBlankAsUnset(t *testing.T) {
	t.Setenv("KANZ_TEST_DUR", "   ")
	got, err := env.Duration("KANZ_TEST_DUR", 7*time.Second)
	if err != nil {
		t.Fatalf("a blank value was refused: %v", err)
	}
	if got != 7*time.Second {
		t.Errorf("got %s, want the default", got)
	}
}

func TestBool(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		want  bool
		fails bool
	}{
		{"true", true, false},
		{"1", true, false},
		{"TRUE", true, false},
		{"false", false, false},
		// `yes` IS NOT A GO BOOL, and this is the case that matters: a control
		// reading DATAMASTER_REQUIRE_DUAL_CONTROL=yes as false is a maker-checker
		// gate that reports itself armed and is not.
		{"yes", false, true},
		{"on", false, true},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			t.Setenv("KANZ_TEST_BOOL", tc.raw)
			got, err := env.Bool("KANZ_TEST_BOOL", false)
			if tc.fails {
				if err == nil {
					t.Fatalf("%q was accepted as %v — a posture nobody set", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Bool(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Errorf("Bool(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestBoolUnsetIsTheDefault(t *testing.T) {
	t.Setenv("KANZ_TEST_BOOL", "")
	got, err := env.Bool("KANZ_TEST_BOOL", true)
	if err != nil || !got {
		t.Fatalf("unset = %v, %v; want true, nil", got, err)
	}
}

func TestInt(t *testing.T) {
	t.Setenv("KANZ_TEST_INT", "42")
	got, err := env.Int("KANZ_TEST_INT", 7)
	if err != nil || got != 42 {
		t.Fatalf("got %d, %v; want 42, nil", got, err)
	}

	t.Setenv("KANZ_TEST_INT", "1k")
	if got, err := env.Int("KANZ_TEST_INT", 7); err == nil {
		t.Fatalf("1k was accepted as %d — a load generator quoting that number is measuring "+
			"against a rate nobody set", got)
	}

	t.Setenv("KANZ_TEST_INT", "")
	if got, err := env.Int("KANZ_TEST_INT", 7); err != nil || got != 7 {
		t.Fatalf("unset = %d, %v; want 7, nil", got, err)
	}
}

// THE ERRORS ARE WRAPPED, so a caller can match on the underlying parse failure
// rather than on the message text.
func TestErrorsWrapTheParseFailure(t *testing.T) {
	t.Setenv("KANZ_TEST_DUR", "nope")
	_, err := env.Duration("KANZ_TEST_DUR", time.Hour)
	if err == nil {
		t.Fatal("no error")
	}
	if errors.Unwrap(err) == nil {
		t.Error("the parse error was not wrapped, so a caller can only match on message text")
	}
}
