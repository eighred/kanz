// Package promscrape reads a Prometheus text-format /metrics endpoint.
//
// IT LIVES HERE BECAUSE A SECOND CONSUMER APPEARED (#1050). It was written for
// test/load/orderflow, and test/load/ingest now needs the same thing: a load
// harness cannot report a component's capacity number without reading that
// component's own exported series. Copying it would have been the failure this
// estate names explicitly — a copied helper is how a fix stops spreading — and
// the distinction its whole design turns on, ABSENT versus ZERO, is exactly the
// one a second hand-rolled copy would get wrong.
package promscrape

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// Sample is one Prometheus series: the label text exactly as exported (empty for
// an unlabelled metric) and its value.
type Sample struct {
	Labels string
	Value  float64
}

// Scrape is one metrics snapshot, keyed by metric family name.
//
// IT DISTINGUISHES "ABSENT" FROM "ZERO", and that distinction is the whole
// reason this type exists rather than a map[string]float64. Every refusal in
// preflight.go turns on it: a family this binary does not export is UNKNOWN, and
// an unknown on the capital path fails closed. Collapsed into a zero, an OMS too
// old to report its venue posture would read as "no live adapters" — the exact
// answer that lets the load run proceed.
type Scrape map[string][]Sample

// Value returns the single unlabelled sample of a family, and whether the family
// was present at all.
func (s Scrape) Value(name string) (float64, bool) {
	ss, ok := s[name]
	if !ok || len(ss) == 0 {
		return 0, false
	}
	// An unlabelled family has exactly one series. A labelled one reaching here is
	// a caller error, and summing would invent a number nobody exported, so take
	// the first and let Sum() be the explicit choice for label sets.
	return ss[0].Value, true
}

// Sum adds every series of a family whose label text contains each of `must`.
// Present reports whether the FAMILY exists, independently of whether any series
// matched — "the metric is not exported" and "the metric is exported and no
// series matches this filter" are different facts and the callers act on them
// differently.
func (s Scrape) Sum(name string, must ...string) (total float64, matched int, present bool) {
	ss, ok := s[name]
	if !ok {
		return 0, 0, false
	}
	for _, x := range ss {
		hit := true
		for _, m := range must {
			if !strings.Contains(x.Labels, m) {
				hit = false
				break
			}
		}
		if hit {
			total += x.Value
			matched++
		}
	}
	return total, matched, true
}

// scrapeClient is this file's OWN client, never http.DefaultClient.
//
// DefaultClient is a shared mutable global: anything in the process can retune
// every call made through it from somewhere no reader of this file would look,
// and its Timeout — if one were set — is enforced independently of the request
// context, so it would win invisibly over the deadline the caller passed. It
// also carries DefaultTransport's MaxIdleConnsPerHost of 2, which matters here
// because the backlog sampler polls this endpoint once a second for the whole
// run beside a submitter opening its own connections.
//
// NO Timeout FIELD, deliberately: every call is bounded by its context, which is
// the one bound a reader can see at the call site.
var scrapeClient = &http.Client{
	Transport: &http.Transport{MaxIdleConns: 8, MaxIdleConnsPerHost: 8},
}

// Fetch reads a Prometheus text-format endpoint.
//
// HAND-PARSED, ON PURPOSE. prometheus/common/expfmt would do this properly, but
// it is an INDIRECT dependency of this module — pulling it in promotes it, which
// is a go.mod change made by a load-test helper. The subset needed here is a
// dozen lines and the parser is exercised by scrape_test.go against real
// exported text, including the two shapes that matter: a labelled family and an
// absent one.
//
// A LINE THIS PARSER CANNOT READ IS AN ERROR, not a skipped line. A metrics
// endpoint that answers with an HTML error page, a proxy's login form, or a
// truncated body would otherwise parse to an EMPTY scrape — and an empty scrape
// is indistinguishable from "the OMS exports no venue posture", which is a
// refusal, so this particular mistake would fail safe. It is still reported,
// because a preflight that refuses for the wrong reason sends whoever reads it
// to the wrong place.
func Fetch(ctx context.Context, url string) (Scrape, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := scrapeClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("scrape %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("scrape %s: http %d", url, resp.StatusCode)
	}
	return Parse(bufio.NewScanner(resp.Body))
}

// Parse reads the Prometheus text exposition subset these harnesses need:
// `name value` and `name{labels} value`. HELP/TYPE lines and blanks are skipped.
func Parse(sc *bufio.Scanner) (Scrape, error) {
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	out := Scrape{}
	lines := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lines++
		sp := strings.LastIndex(line, " ")
		if sp < 0 {
			return nil, fmt.Errorf("metrics line %q has no value", line)
		}
		key, raw := line[:sp], line[sp+1:]
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("metrics line %q: %w", line, err)
		}
		name, labels := key, ""
		if b := strings.IndexByte(key, '{'); b >= 0 {
			name, labels = key[:b], key[b:]
		}
		out[name] = append(out[name], Sample{Labels: labels, Value: v})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if lines == 0 {
		// An endpoint that answered 200 with no series at all is not a process this
		// harness can make any claim about. Saying so beats returning an empty map
		// that every later check reads as "the metric is absent".
		return nil, fmt.Errorf("the metrics endpoint returned no series")
	}
	return out, nil
}
