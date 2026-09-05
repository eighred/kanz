package main

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eighred/kanz/test/load/internal/promscrape"
)

// THE CAPACITY HALF OF THIS HARNESS (#1050).
//
// ingest already drove the exact input that widens the risk engine's recompute
// fan-out — PositionState across PORTFOLIOS=N — and measured publish rate and bus
// backlog, which are properties of the GENERATOR and the SPINE. Neither says
// anything about the process under test, so the one component whose working set
// is a function of estate size rather than a constant had no capacity number at
// all, and #231's resources: block had no measurement to be derived from.
//
// WHAT IT READS AND WHY IT READS IT FROM THE ENGINE'S OWN /metrics.
// The engine is a separate process; a goroutine count taken in THIS process would
// measure the load generator. So the numbers come from the series the engine
// already exports — go_goroutines, process_resident_memory_bytes, the recompute
// duration histogram — plus the two #1050 added. Nothing here is a new producer:
// a load harness that had to instrument the thing it measures would be measuring
// its own instrumentation.
//
// AN UNREADABLE ENDPOINT IS REPORTED AS UNKNOWN, NEVER AS ZERO. A run that could
// not scrape the engine still produced a valid publish-rate number, and it must
// not also produce a peak RSS of 0 B that somebody later quotes into a resources:
// block. "Nothing configured" and "checked, and fine" must not look the same,
// least of all in the artifact a memory limit gets sized from.

// recomputeDuration is the histogram family whose _sum/_count/_bucket series this
// harness reads.
//
// NAMED ONCE, AS THE FAMILY, AND THE SUFFIXES APPENDED. `_sum`, `_count` and
// `_bucket` are Prometheus's own exposition suffixes on ONE family, not three
// metrics — and test/arch/load_harness_front_door_test.go checks every bare
// kanz_* literal under test/load against the families some other package actually
// declares, precisely so a watch list cannot go stale in silence. Writing the
// suffixed forms as literals would name three families nothing exports, and the
// guard would be right to reject them.
const recomputeDuration = "kanz_risk_recompute_duration_seconds"

// engineSample is one poll of the engine's exported state.
type engineSample struct {
	goroutines  float64
	rssBytes    float64 // process_resident_memory_bytes; LINUX ONLY, see rssKnown
	rssKnown    bool
	goSysBytes  float64 // go_memstats_sys_bytes — the portable proxy
	goSysKnown  bool
	inflight    float64
	inflightOK  bool
	queueDepth  float64
	queueDepthK bool
	buckets     map[float64]float64 // cumulative recompute-duration buckets
	obs         float64             // recompute count
	sum         float64             // total recompute seconds
}

// capacityReport accumulates the peaks across a run.
//
// PEAKS, NOT AVERAGES. A memory limit is sized against the worst moment a
// workload has, and the worst moment here is by construction a burst: the widest
// fan-out coincides with the correlated move that dirties every book at once. An
// average over a two-minute run would report the quiet majority of it.
type capacityReport struct {
	mu sync.Mutex

	polls   int
	failed  int
	lastErr error

	peakGoroutines float64
	peakRSS        float64
	sawRSS         bool
	peakGoSys      float64
	sawGoSys       bool
	peakInflight   float64
	sawInflight    bool
	peakQueue      float64
	sawQueue       bool

	first, last engineSample
	haveFirst   bool
}

// pollEngine scrapes the engine endpoint until ctx ends, at `every`.
//
// It never fails the run. A load generator that stopped publishing because its
// observability sidecar could not reach a metrics port would destroy the primary
// measurement to protect the secondary one.
func (c *capacityReport) pollEngine(ctx context.Context, url string, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		c.pollOnce(ctx, url)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (c *capacityReport) pollOnce(ctx context.Context, url string) {
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	s, err := promscrape.Fetch(rctx, url)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.polls++
	if err != nil {
		c.failed++
		c.lastErr = err
		return
	}
	sample := readEngine(s)
	if !c.haveFirst {
		c.first, c.haveFirst = sample, true
	}
	c.last = sample

	if sample.goroutines > c.peakGoroutines {
		c.peakGoroutines = sample.goroutines
	}
	if sample.rssKnown {
		c.sawRSS = true
		if sample.rssBytes > c.peakRSS {
			c.peakRSS = sample.rssBytes
		}
	}
	if sample.goSysKnown {
		c.sawGoSys = true
		if sample.goSysBytes > c.peakGoSys {
			c.peakGoSys = sample.goSysBytes
		}
	}
	if sample.inflightOK {
		c.sawInflight = true
		if sample.inflight > c.peakInflight {
			c.peakInflight = sample.inflight
		}
	}
	if sample.queueDepthK {
		c.sawQueue = true
		if sample.queueDepth > c.peakQueue {
			c.peakQueue = sample.queueDepth
		}
	}
}

// readEngine projects a scrape onto the fields this harness reports.
func readEngine(s promscrape.Scrape) engineSample {
	var e engineSample
	e.goroutines, _ = s.Value("go_goroutines")
	e.rssBytes, e.rssKnown = s.Value("process_resident_memory_bytes")
	e.goSysBytes, e.goSysKnown = s.Value("go_memstats_sys_bytes")
	e.inflight, e.inflightOK = s.Value("kanz_risk_recompute_inflight")
	e.queueDepth, e.queueDepthK = s.Value("kanz_risk_recompute_queue_depth")
	e.sum, _ = s.Value(recomputeDuration + "_sum")
	e.obs, _ = s.Value(recomputeDuration + "_count")
	e.buckets = map[float64]float64{}
	for _, b := range s[recomputeDuration+"_bucket"] {
		le, ok := bucketBound(b.Labels)
		if !ok {
			continue
		}
		e.buckets[le] = b.Value
	}
	return e
}

// bucketBound pulls `le` out of a histogram bucket's label text. `+Inf` is a
// legitimate bound and parses as math.Inf(1).
func bucketBound(labels string) (float64, bool) {
	i := strings.Index(labels, `le="`)
	if i < 0 {
		return 0, false
	}
	rest := labels[i+4:]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(rest[:j], 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// histogramQuantile answers at BUCKET RESOLUTION: the smallest bucket bound whose
// cumulative count covers q of the observations.
//
// DELIBERATELY NOT INTERPOLATED. Prometheus's histogram_quantile interpolates
// linearly inside a bucket, which invents digits the exporter never had — and
// this engine's widest bucket spans 1s to 2.5s. A capacity report that printed
// "p99 = 1.43s" out of a bucket that only knows "somewhere between 1s and 2.5s"
// would be quoted as a measurement. "<= 2.5s" is the honest form of the same
// fact.
func histogramQuantile(buckets map[float64]float64, total, q float64) (float64, bool) {
	if total <= 0 || len(buckets) == 0 {
		return 0, false
	}
	bounds := make([]float64, 0, len(buckets))
	for le := range buckets {
		bounds = append(bounds, le)
	}
	sort.Float64s(bounds)
	want := q * total
	for _, le := range bounds {
		if buckets[le] >= want {
			return le, true
		}
	}
	return math.Inf(1), true
}

// deltaBuckets subtracts the opening scrape from the closing one, so the latency
// distribution describes THIS RUN rather than the engine's whole uptime. A
// process that has been up for a day carries a histogram dominated by whatever it
// did yesterday.
func deltaBuckets(first, last engineSample) (map[float64]float64, float64) {
	out := map[float64]float64{}
	for le, v := range last.buckets {
		out[le] = v - first.buckets[le]
	}
	return out, last.obs - first.obs
}

func humanBytes(b float64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2f GiB", b/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", b/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KiB", b/(1<<10))
	default:
		return fmt.Sprintf("%.0f B", b)
	}
}

// render writes the capacity block of the run report.
//
// EVERY LINE IT CANNOT SUBSTANTIATE SAYS SO IN WORDS. There is no field here that
// prints 0 for "not measured": the whole point of the block is to be quotable
// into a resources: block (#231), and a number nobody measured is worse than no
// block at all, because the limit it produces looks chosen.
func (c *capacityReport) render(url string, portfolios int) string {
	c.mu.Lock()
	defer c.mu.Unlock()

	var b strings.Builder
	fmt.Fprintf(&b, "\n=== risk-engine capacity, PORTFOLIOS=%d ===\n", portfolios)
	fmt.Fprintf(&b, "engine metrics: %s (%d polls, %d failed)\n", url, c.polls, c.failed)
	if c.polls == 0 || c.polls == c.failed {
		fmt.Fprintf(&b, "UNKNOWN — the engine's metrics endpoint was never read successfully")
		if c.lastErr != nil {
			fmt.Fprintf(&b, " (%v)", c.lastErr)
		}
		fmt.Fprintf(&b, ".\nNo capacity claim can be made from this run. The publish numbers above "+
			"are still valid; they describe the generator and the spine, not the engine.\n")
		return b.String()
	}
	if c.failed > 0 {
		fmt.Fprintf(&b, "PARTIAL — %d of %d polls failed (last: %v). The peaks below are lower "+
			"bounds: a burst inside a missed poll was not seen.\n", c.failed, c.polls, c.lastErr)
	}

	fmt.Fprintf(&b, "peak goroutines:            %.0f\n", c.peakGoroutines)
	if c.sawRSS {
		fmt.Fprintf(&b, "peak RSS:                   %s\n", humanBytes(c.peakRSS))
	} else {
		fmt.Fprintf(&b, "peak RSS:                   UNKNOWN — process_resident_memory_bytes is not "+
			"exported by this platform. client_golang's process collector is Linux-only, so a run "+
			"against an engine on Windows or macOS cannot state RSS; go_memstats_sys_bytes below "+
			"is the portable proxy and is NOT the number a memory limit is set from.\n")
	}
	if c.sawGoSys {
		fmt.Fprintf(&b, "peak Go heap reserved:      %s (go_memstats_sys_bytes)\n", humanBytes(c.peakGoSys))
	}
	if c.sawInflight {
		fmt.Fprintf(&b, "peak recomputes in flight:  %.0f\n", c.peakInflight)
	} else {
		fmt.Fprintf(&b, "peak recomputes in flight:  UNKNOWN — kanz_risk_recompute_inflight is not "+
			"exported. Either this engine predates #1050 or the collector has been moved behind a "+
			"branch, which is the defect that silenced two alerting layers here (#973, #963).\n")
	}
	if c.sawQueue {
		fmt.Fprintf(&b, "peak recompute backlog:     %.0f portfolios\n", c.peakQueue)
	} else {
		fmt.Fprintf(&b, "peak recompute backlog:     UNKNOWN — kanz_risk_recompute_queue_depth is "+
			"not exported.\n")
	}

	buckets, n := deltaBuckets(c.first, c.last)
	if n <= 0 {
		fmt.Fprintf(&b, "recompute latency:          NO RECOMPUTES RAN during this run. The engine "+
			"is up and exporting, and it did not recompute anything — check that it is subscribed "+
			"to the subject this harness publishes on and that the shard ring assigns it these "+
			"portfolios.\n")
		return b.String()
	}
	fmt.Fprintf(&b, "recomputes:                 %.0f (mean %.1f ms)\n",
		n, ((c.last.sum-c.first.sum)/n)*1000)
	for _, q := range []struct {
		label string
		v     float64
	}{{"p50", 0.50}, {"p95", 0.95}, {"p99", 0.99}} {
		le, ok := histogramQuantile(buckets, n, q.v)
		if !ok {
			continue
		}
		if math.IsInf(le, 1) {
			fmt.Fprintf(&b, "recompute %s:              > 2.5s (over the widest bucket)\n", q.label)
			continue
		}
		fmt.Fprintf(&b, "recompute %s:              <= %s (bucket resolution)\n",
			q.label, time.Duration(le*float64(time.Second)).String())
	}
	return b.String()
}
