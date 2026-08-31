package main

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// sample is one Prometheus series: the label text exactly as exported (empty for
// an unlabelled metric) and its value.
type sample struct {
	labels string
	value  float64
}

// scrape is one metrics snapshot, keyed by metric family name.
//
// IT DISTINGUISHES "ABSENT" FROM "ZERO", and that distinction is the whole
// reason this type exists rather than a map[string]float64. Every refusal in
// preflight.go turns on it: a family this binary does not export is UNKNOWN, and
// an unknown on the capital path fails closed. Collapsed into a zero, an OMS too
// old to report its venue posture would read as "no live adapters" — the exact
// answer that lets the load run proceed.
type scrape map[string][]sample

// value returns the single unlabelled sample of a family, and whether the family
// was present at all.
func (s scrape) value(name string) (float64, bool) {
	ss, ok := s[name]
	if !ok || len(ss) == 0 {
		return 0, false
	}
	// An unlabelled family has exactly one series. A labelled one reaching here is
	// a caller error, and summing would invent a number nobody exported, so take
	// the first and let sum() be the explicit choice for label sets.
	return ss[0].value, true
}

// sum adds every series of a family whose label text contains each of `must`.
// Present reports whether the FAMILY exists, independently of whether any series
// matched — "the metric is not exported" and "the metric is exported and no
// series matches this filter" are different facts and the callers act on them
// differently.
func (s scrape) sum(name string, must ...string) (total float64, matched int, present bool) {
	ss, ok := s[name]
	if !ok {
		return 0, 0, false
	}
	for _, x := range ss {
		hit := true
		for _, m := range must {
			if !strings.Contains(x.labels, m) {
				hit = false
				break
			}
		}
		if hit {
			total += x.value
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

// fetchScrape reads a Prometheus text-format endpoint.
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
func fetchScrape(ctx context.Context, url string) (scrape, error) {
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
	return parseScrape(bufio.NewScanner(resp.Body))
}

// parseScrape reads the Prometheus text exposition subset this harness needs:
// `name value` and `name{labels} value`. HELP/TYPE lines and blanks are skipped.
func parseScrape(sc *bufio.Scanner) (scrape, error) {
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	out := scrape{}
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
		out[name] = append(out[name], sample{labels: labels, value: v})
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

// controlSignal is one counter this harness watches ACROSS the run, with the
// sentence a non-zero delta means. The set is the point: a write-path load run
// that reported only latency and error rate would miss every failure mode #865
// names, because none of them is an error the client sees.
type controlSignal struct {
	metric string
	// invalidates is true when a non-zero delta means the RUN MEASURED SOMETHING
	// ELSE, as opposed to the run having found a real degradation. The two are
	// different verdicts: one says the platform degraded, the other says the
	// number must not be quoted at all.
	invalidates bool
	meaning     string
}

// watchedControls is what a write-path run reports deltas for.
//
// DERIVED FROM NOTHING — IT IS A HAND-WRITTEN LIST, AND THAT IS A KNOWN LIMIT.
// The estate's guidance is to derive such a set from its source of truth where
// one exists; there is no registry of "counters that mean a control degraded",
// and inventing one to serve a load harness would be the harness redefining the
// platform. What this list must not become is silently stale, so
// scrape_test.go asserts every name here is exported by the OMS's own
// registry — a metric renamed out from under this file fails a test rather than
// reporting a clean run forever.
var watchedControls = []controlSignal{
	{
		metric:      "kanz_compliance_ungoverned_orders_total",
		invalidates: true,
		meaning: "orders were admitted for a portfolio NO MANDATE GOVERNS. The pre-trade gate " +
			"returns at its first branch for those — no rule engine, no book projection, no " +
			"margin resolution — so a throughput number taken across them measures a short " +
			"circuit and not the gate. Put the portfolio under mandate (kanz-mandate) and re-run",
	},
	{
		metric:      "kanz_compliance_unreadable_mandate_orders_total",
		invalidates: true,
		meaning: "orders were refused because the portfolio's published mandate could not be " +
			"applied. Those never reach the rule engine either, so the run measured a refusal path",
	},
	{
		metric:      "kanz_compliance_decisions_dropped_total",
		invalidates: false,
		meaning: "pre-trade compliance decisions were NOT recorded. Each one is a decision about " +
			"whether capital moves with no audit record behind it — the recorder's queue could " +
			"not keep up with admission",
	},
	{
		metric:      "kanz_oms_claim_timeouts_total",
		invalidates: false,
		meaning: "the per-order claim (orderlock) timed out: an operator's cancel or amend was " +
			"parked in the DLQ while the order was still working at a venue. This is the " +
			"head-of-line control giving up under contention",
	},
	{
		metric:      "kanz_oms_orders_quarantined_total",
		invalidates: false,
		meaning: "orders were frozen because the platform could not establish what the venue did " +
			"with them. Each one is a position whose true size nobody knows",
	},
	{
		metric:      "kanz_oms_outbox_publish_failures_total",
		invalidates: false,
		meaning: "order FACTs were committed and could not be published. The estate does not know " +
			"about orders the store has admitted",
	},
	{
		metric:      "kanz_oms_shared_collateral_orders_total",
		invalidates: false,
		meaning: "orders executed against an exchange account not bound to their portfolio. " +
			"Expected on a rig with no OMS_VENUE_ACCOUNTS, and reported so the number is never " +
			"read as a clean segregation result",
	},
}

// controlDeltas compares two scrapes over watchedControls. A metric absent from
// either scrape is reported as a delta of NaN with `unknown` set, never as zero:
// this harness may not report "no control degraded" on the strength of a counter
// it could not read.
type controlDelta struct {
	controlSignal
	delta   float64
	unknown bool
}

func controlDeltas(before, after scrape) []controlDelta {
	out := make([]controlDelta, 0, len(watchedControls))
	for _, c := range watchedControls {
		b, okB := before.value(c.metric)
		a, okA := after.value(c.metric)
		out = append(out, controlDelta{controlSignal: c, delta: a - b, unknown: !okA || !okB})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].metric < out[j].metric })
	return out
}

// pendingCommands reads the standing backlog of order COMMANDS for a consumer
// group: kanz_bus_pending_messages{group=<g>,subject=order.order.submit}.
//
// THIS IS THE WRITE PATH'S KNEE SIGNAL, and it is the same series that
// capacity-model.md already sizes KEDA against for the risk-engine — reused
// rather than re-invented, so a write-path capacity number and the autoscaling
// policy speak about one quantity. A backlog that grows across a stage and does not
// drain is the write-path equivalent of the read path's p99 breaking: the client
// still sees 202s, because the gateway accepted the command; the ADMISSION is
// what is falling behind.
func pendingCommands(s scrape, group, subject string) (float64, bool) {
	total, matched, present := s.sum("kanz_bus_pending_messages",
		`group="`+group+`"`, `subject="`+subject+`"`)
	return total, present && matched > 0
}
