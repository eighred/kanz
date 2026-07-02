// Package validation is the PARITY-03h independent validation gate for the Go
// analytics — the MLOPS-01a SR 11-7 stance ("no model serves production
// without recorded, current validation") applied to the quant library. An
// analytic is validated by reproducing an INDEPENDENT benchmark — a published
// worked example, a regulator's unit test, an alternative implementation —
// within tolerance; the evidence is a signed, dated ValidationReport; and the
// Gate denies promotion on a missing, failed, or expired validation.
//
// Benchmark cases are DATA (the caller runs its analytic and supplies got vs
// want), so this package imports no analytic code and any module can validate
// against it — pricing against the Hull worked examples, FRTB against the
// Basel ones, SIMM against the ISDA unit tests. Signing reuses the REG-01
// Signer shape: the default is a SHA-256 content hash, and the AUDIT-01
// hash-chain signer (PARITY-04e) plugs into both seams identically.
package validation

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Case is one benchmark reproduction: the analytic's output vs the independent
// benchmark value, with an absolute tolerance.
type Case struct {
	Name      string
	Got       float64
	Want      float64
	Tolerance float64
}

// Passed reports whether the case reproduces its benchmark within tolerance.
func (c Case) Passed() bool {
	return !math.IsNaN(c.Got) && math.Abs(c.Got-c.Want) <= c.Tolerance
}

// Report is the signed validation evidence for one analytic. A validation is
// current, not an open-ended sign-off — it expires (the MLOPS-01a stance).
type Report struct {
	Analytic    string
	ValidatedAt time.Time
	ExpiresAt   time.Time
	Cases       []Case
	Passed      bool
	Signature   string
}

// Signer signs the canonical report bytes — the REG-01 Signer seam.
type Signer interface {
	Sign(canonical []byte) string
}

// HashSigner is the default content-hash Signer (tamper-evidence without a
// key), identical in stance to regulatory.HashSigner.
type HashSigner struct{}

// Sign returns the hex SHA-256 of the canonical bytes.
func (HashSigner) Sign(canonical []byte) string {
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

// ErrValidation is returned for an unusable validation run.
var ErrValidation = errors.New("validation: invalid validation run")

// DefaultValidity is the default revalidation cadence (the MLOPS-01a 90d).
const DefaultValidity = 90 * 24 * time.Hour

// Validate assembles and signs a report from benchmark cases. It errors on an
// empty case set (an evidence-free sign-off) or a non-positive tolerance (an
// un-passable or meaninglessly loose case must be explicit, not accidental).
// A FAILING report is still produced and signed — failure is audit evidence;
// it simply never satisfies the gate. validity 0 ⇒ DefaultValidity; nil
// signer ⇒ HashSigner.
func Validate(analytic string, cases []Case, validatedAt time.Time, validity time.Duration, signer Signer) (Report, error) {
	if analytic == "" {
		return Report{}, fmt.Errorf("%w: analytic name required", ErrValidation)
	}
	if len(cases) == 0 {
		return Report{}, fmt.Errorf("%w: no benchmark cases (evidence-free sign-off)", ErrValidation)
	}
	for _, c := range cases {
		if c.Tolerance <= 0 {
			return Report{}, fmt.Errorf("%w: case %q has non-positive tolerance", ErrValidation, c.Name)
		}
	}
	if validity <= 0 {
		validity = DefaultValidity
	}
	if signer == nil {
		signer = HashSigner{}
	}
	r := Report{
		Analytic:    analytic,
		ValidatedAt: validatedAt,
		ExpiresAt:   validatedAt.Add(validity),
		Cases:       append([]Case(nil), cases...),
		Passed:      true,
	}
	for _, c := range r.Cases {
		if !c.Passed() {
			r.Passed = false
			break
		}
	}
	r.Signature = signer.Sign(r.canonical())
	return r, nil
}

// canonical renders the report to deterministic bytes for signing (the REG-01
// canonicalization stance): analytic, RFC-3339 times, pass flag, cases sorted.
func (r Report) canonical() []byte {
	lines := []string{
		r.Analytic,
		r.ValidatedAt.UTC().Format(time.RFC3339Nano),
		r.ExpiresAt.UTC().Format(time.RFC3339Nano),
		strconv.FormatBool(r.Passed),
	}
	cs := make([]string, len(r.Cases))
	for i, c := range r.Cases {
		cs[i] = c.Name + "=" + strconv.FormatFloat(c.Got, 'f', -1, 64) +
			"/" + strconv.FormatFloat(c.Want, 'f', -1, 64) +
			"/" + strconv.FormatFloat(c.Tolerance, 'f', -1, 64)
	}
	sort.Strings(cs)
	lines = append(lines, cs...)
	var b []byte
	for _, l := range lines {
		b = append(b, l...)
		b = append(b, '\n')
	}
	return b
}

// ErrDenied is returned by Promote when the gate blocks an analytic.
var ErrDenied = errors.New("validation: promotion denied")

// Gate is the deny-by-default promotion gate: an analytic may promote only
// with a recorded, passing, unexpired, signed validation. Latest report per
// analytic wins (a revalidation supersedes; a recorded failure blocks).
type Gate struct {
	mu      sync.RWMutex
	reports map[string]Report
	clock   func() time.Time
}

// NewGate builds a gate; nil clock defaults to UTC now (injected for
// deterministic expiry tests, the MLOPS-01a pattern).
func NewGate(clock func() time.Time) *Gate {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &Gate{reports: map[string]Report{}, clock: clock}
}

// Record stores an analytic's latest validation. An unsigned report is
// rejected — evidence must be tamper-evident before it can gate anything.
func (g *Gate) Record(r Report) error {
	if r.Analytic == "" || r.Signature == "" {
		return fmt.Errorf("%w: report must name its analytic and be signed", ErrValidation)
	}
	g.mu.Lock()
	g.reports[r.Analytic] = r
	g.mu.Unlock()
	return nil
}

// Promote returns nil only when the analytic holds a current passing
// validation; otherwise ErrDenied with the reason (missing / failed /
// expired).
func (g *Gate) Promote(analytic string) error {
	g.mu.RLock()
	r, ok := g.reports[analytic]
	g.mu.RUnlock()
	switch {
	case !ok:
		return fmt.Errorf("%w: %s has no recorded validation", ErrDenied, analytic)
	case !r.Passed:
		return fmt.Errorf("%w: %s failed validation at %s", ErrDenied, analytic, r.ValidatedAt.Format(time.RFC3339))
	case !g.clock().Before(r.ExpiresAt):
		return fmt.Errorf("%w: %s validation expired %s", ErrDenied, analytic, r.ExpiresAt.Format(time.RFC3339))
	}
	return nil
}

// Report returns the analytic's recorded validation for audit/health reads.
func (g *Gate) Report(analytic string) (Report, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	r, ok := g.reports[analytic]
	return r, ok
}
