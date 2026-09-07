// Package env is the one place this module reads configuration out of the
// environment.
//
// # Why it exists
//
// AGENTS.md names the shape it ends: "A copied helper is how a fix stops
// spreading: 17 services each had their own secret() and 15 were wrong while 2
// were right." That one was repaired and guarded. Its siblings in the SAME FILES
// were never touched, and they had grown larger than the original — 40 copies of
// envOr in five variants, 27 of parseLevel in three, 13 of splitList (#641).
//
// The counts in the issue were 39, 26 and 13. They were 40 and 27 by the time
// the repair started, which is the argument in miniature: the copies were still
// multiplying while the issue describing them sat open.
//
// # What they disagreed about
//
// ONE service of twenty-seven accepted `warning`. The other twenty-six matched
// `warn` only, so LOG_LEVEL=warning fell through to a default of INFO — a MORE
// verbose level than the operator asked for, chosen silently. Setting `warning`
// estate-wide gave the requested level in identity and INFO everywhere else, off
// one ConfigMap key, with nothing saying the value was not understood.
//
// FIFTEEN of twenty-seven trimmed whitespace. `LOG_LEVEL=" debug"` — a YAML
// block scalar, a value read from a mounted file with a trailing newline — was
// DEBUG in fifteen services and INFO in the rest.
//
// And envOr split three ways on a whitespace-only value: fifteen copies treated
// it as unset and used the default, twenty-four handed the raw value on. So "what
// does a config key set to a single space mean" had three answers, decided by
// which file the author happened to copy from.
package env

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Or returns the environment variable's value, or def when it is unset or blank.
//
// # The two decisions in here, and why
//
// IT TRIMS. A value that arrives from a mounted file, a YAML block scalar or a
// templated ConfigMap routinely carries a trailing newline, and handing that to
// a DSN parser or a broker dialer is a defect that surfaces far from its cause.
// Twenty-four of the forty copies did not trim.
//
// A SET-BUT-BLANK KEY FALLS BACK TO def, and that is the compromise worth
// stating plainly rather than discovering later. In Kubernetes an
// `env: - name: X value: ""` produced by a template that failed to interpolate
// is common, and substituting the compiled-in default there is exactly the
// "nothing configured and checked-and-fine look the same" state the coding
// standards forbid.
//
// IT IS KEPT BECAUSE THE ESTATE ALREADY MEANS IT THAT WAY, which was measured
// rather than assumed: eight manifests carry `value: ""` deliberately to mean
// "not configured" — OMS_VENUE_ACCOUNTS, BINANCE_VENUE_ACCOUNT_UID,
// WEBHOOK_INGEST_IP_ALLOWLIST among them. Making a set-but-blank key return ""
// instead would change behaviour at all eight for a semantic nobody asked for.
//
// AND THE COMPROMISE BIT IMMEDIATELY, WHICH IS WHY THIS PARAGRAPH IS LONGER THAN
// IT WOULD OTHERWISE BE. The OMS previously used a NON-TRIMMING copy, so
// OMS_PRICE_SUBJECTS=" " passed through as " ", split to an empty list, and
// Load REFUSED — a pod subscribing to nothing folds no marks and rejects every
// MARKET/STOP order. Unifying on the trimming form collapsed " " to "" and then
// to the default, and TestEmptyPriceSubjectsIsAnError caught it on the first
// run of the suite.
//
// The resolution is that per-key strictness belongs at the CALL SITE, not in a
// global rule: that one key now reads through Lookup and refuses a blank, and
// the other 138 keep the convention the manifests already encode. Which is the
// argument for having one helper at all — the decision is now made once per key,
// visibly, instead of being inherited from whichever of five copies an author
// happened to paste.
func Or(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// Lookup returns the trimmed value and whether the key was set to anything
// meaningful. It exists so a caller that must refuse a blank value can, without
// writing a fortieth copy of the read to do it.
func Lookup(key string) (string, bool) {
	v, ok := os.LookupEnv(key)
	if !ok {
		return "", false
	}
	v = strings.TrimSpace(v)
	return v, v != ""
}

// ParseLevel maps a log-level string to a slog.Level, reporting whether it was
// recognised.
//
// BOTH `warn` AND `warning` ARE ACCEPTED. Twenty-six of twenty-seven copies took
// only `warn`, so an operator who wrote `warning` got INFO — more verbose than
// asked for, and silently.
//
// ok IS RETURNED RATHER THAN DEFAULTED HERE. An unrecognised level is a
// misconfiguration, and a helper that quietly answers Info for it is the same
// silence in a different place. Most callers still use ParseLevelOr, because
// refusing to start over a log level is a decision each composition root should
// make deliberately and one it can only make once it has somewhere to report the
// refusal — which, before the logger exists, it does not.
func ParseLevel(s string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, true
	case "info":
		return slog.LevelInfo, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	}
	return slog.LevelInfo, false
}

// ParseLevelOr maps a log-level string to a slog.Level, falling back to def when
// it is not recognised. The drop-in for the twenty-seven copies this replaced —
// same behaviour, one implementation, and now agreeing about `warning` and about
// whitespace.
func ParseLevelOr(s string, def slog.Level) slog.Level {
	if lvl, ok := ParseLevel(s); ok {
		return lvl
	}
	return def
}

// SplitList splits a comma-separated list, trimming each element and dropping
// empties.
//
// THIRTEEN COPIES THAT AGREED BY LUCK. Their two bodies differed only in a
// parameter name, which is the reason to promote it rather than the reason not
// to: nothing was keeping them agreeing, and the next edit to one of them would
// have been the first disagreement.
func SplitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Duration reads a Go duration, or returns def when the key is unset or blank.
//
// A MALFORMED VALUE IS AN ERROR, NEVER THE DEFAULT (#692). This is the whole
// reason the function returns one. Silently falling back leaves a service on a
// schedule the operator did not choose while the deployment reports a clean
// start — "nothing configured" and "checked, and fine" looking the same, which is
// the rule AGENTS.md states and the shape this package exists to end.
//
// It mattered most where it was least visible. datamaster parsed five intervals
// through a swallowing copy, two of which — DATAMASTER_DUAL_CONTROL_TTL and
// DATAMASTER_LAPSED_PROPOSAL_RETENTION — bound how long a dual-control proposal
// stays approvable. A typo there changed an authorization window with nothing
// said. Its sibling in accounting had the correct behaviour AND the paragraph
// explaining why; the copy without the doc was the copy with the defect.
//
// # A NON-POSITIVE VALUE IS RETURNED, NOT REJECTED
//
// Zero and negative are legal here and mean whatever the caller decides they
// mean: DATAMASTER_OUTBOX_INTERVAL defaults to 0 and reads it as "no relay on
// this deployment". The old copy folded `err != nil || d <= 0` into one branch,
// so an unparseable value and an explicit 0 were the same outcome — and on that
// key the default IS 0, making a typo indistinguishable from the intended
// setting. Refusing a bad parse and passing a well-formed 0 through are
// different jobs, and only the first belongs here.
func Duration(key string, def time.Duration) (time.Duration, error) {
	raw, ok := Lookup(key)
	if !ok {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("env: %s=%q is not a duration: %w", key, raw, err)
	}
	return d, nil
}

// Bool reads a boolean, or returns def when the key is unset or blank.
//
// A VALUE strconv.ParseBool CANNOT READ IS AN ERROR, never a silent disarm. The
// values this gates are postures — OMS_REQUIRE_DUAL_CONTROL,
// DATAMASTER_REQUIRE_DUAL_CONTROL, OMS_REQUIRE_MANDATE — and a control that
// reads `DATAMASTER_REQUIRE_DUAL_CONTROL=yes` as false is a maker-checker gate
// that reports itself armed and is not. `yes` is not a Go bool; `true`, `1`,
// `T` and `TRUE` are.
func Bool(key string, def bool) (bool, error) {
	raw, ok := Lookup(key)
	if !ok {
		return def, nil
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("env: %s=%q is not a boolean: %w", key, raw, err)
	}
	return b, nil
}

// Int reads an integer, or returns def when the key is unset or blank. A
// malformed value is an error, for Duration's reason.
func Int(key string, def int) (int, error) {
	raw, ok := Lookup(key)
	if !ok {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("env: %s=%q is not an integer: %w", key, raw, err)
	}
	return n, nil
}
