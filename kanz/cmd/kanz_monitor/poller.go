package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// pollMsg carries one poll cycle's result into Update: the latest incident
// counter snapshot, per-service readiness, and a non-fatal error (if any —
// shown in the status line, never a crash; see model.Update).
//
// Filled/Rejected are deliberately left at 0 here: this monitor already gets
// live fill/reject EVENTS off the bus in Task 3 (busEventMsg, decoded from
// order.order.filled / order.order.rejected), and Task 5's render derives
// those two tile values from m.events rather than this snapshot. Re-deriving
// them a second time from kanz_bus_publish_total{subject=...} would mean two
// sources of truth for the same two numbers disagreeing under scrape lag —
// the bus feed is already exact and live; the poller's job is the five
// incident counters that have no bus event of their own.
type pollMsg struct {
	counters counterSnapshot
	health   map[string]bool
	err      error
}

// pollHTTPTimeout bounds every request a poll cycle makes, so an unreachable
// gateway degrades the tick (err set, health/counters left at their prior
// values by Update) rather than hanging the ticker forever.
const pollHTTPTimeout = 5 * time.Second

// pollTick arms a single tea.Tick that fires after cfg.PollInterval and does
// the HTTP scrape off the UI thread inside the tick callback, returning a
// pollMsg. Update re-arms it after every pollMsg (see model.Update), so the
// monitor polls forever at a fixed cadence for the life of the program.
//
// READ-ONLY, deliberately: every request this file issues is an http.Get
// (via http.MethodGet) against one of two distinct read endpoints —
// cfg.MetricsURL + "/metrics" (the OMS's own registry, for the five
// incident counters) and cfg.GatewayURL + "/readyz" (gateway health).
// There is no bus.Producer, no http.MethodPost, nothing here can move
// capital — a monitor scrapes; it does not act.
func (m model) pollTick() tea.Cmd {
	return tea.Tick(m.cfg.PollInterval, func(time.Time) tea.Msg {
		return m.poll()
	})
}

// poll performs one scrape cycle against two distinct endpoints: GET
// /metrics on the OMS (its own Prometheus registry, where the five
// incident counters actually live) and GET /readyz on the api-gateway for
// gateway health. The gateway's own /metrics only exports its middleware
// metrics — it never re-exports the OMS counters — so the counter scrape
// must not be pointed at cfg.GatewayURL.
//
// cfg.Token == "" DEGRADES rather than crashes — a missing bearer is the
// normal local-dev shape (no kanz-devtoken exported), so both routes are
// still tried unauthenticated instead of being skipped outright; whichever
// comes back non-200 just leaves its half of the result at its zero value
// and folds the failure into err for the status line.
func (m model) poll() pollMsg {
	ctx, cancel := context.WithTimeout(context.Background(), pollHTTPTimeout)
	defer cancel()
	client := &http.Client{Timeout: pollHTTPTimeout}

	msg := pollMsg{health: map[string]bool{}}

	// cfg.MetricsURL == "" is a deliberate skip, not a failure: an operator
	// who only wants the bus feed + gateway health hasn't configured an OMS
	// metrics endpoint, and dialing "/metrics" against an empty host would
	// produce a spurious dial error every tick. Counters simply stay at
	// their zero value, same shape as the token=="" degradation below.
	if m.cfg.MetricsURL != "" {
		body, err := httpGetBody(ctx, client, m.cfg.MetricsURL+"/metrics", m.cfg.Token)
		if err != nil {
			msg.err = fmt.Errorf("metrics: %w", err)
		} else {
			msg.counters = parseCounters(body)
		}
	}

	ready, rerr := httpGetReady(ctx, client, m.cfg.GatewayURL+"/readyz", m.cfg.Token)
	if rerr != nil {
		if msg.err == nil {
			msg.err = fmt.Errorf("readyz: %w", rerr)
		}
	} else {
		msg.health["gateway"] = ready
	}

	return msg
}

// httpGetBody issues one authenticated (if token != "") GET and returns the
// response body as text, or an error for anything but 200.
func httpGetBody(ctx context.Context, client *http.Client, url, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// httpGetReady issues one authenticated (if token != "") GET and reports
// whether the response was 200. A non-200 readyz is not ready, not an error —
// only a dial/transport failure is returned as err.
func httpGetReady(ctx context.Context, client *http.Client, url, token string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == http.StatusOK, nil
}

// parseCounters extracts the five OMS/compliance incident counters this
// monitor shows out of a Prometheus exposition-format body (as served by
// /metrics). Every metric name below is verified against
// services/oms/cmd/oms/main.go — see poller_test.go's captured snippet.
//
// A metric ABSENT from the body reads as 0, not an error: a fresh pod
// exports no sample for a counter until its first Inc(), and that must read
// as "nothing has happened yet", never crash the poll.
//
// A metric split across a label (kanz_compliance_unpriced_orders_total has a
// `reason` label with never_seen/expired values) is SUMMED across every
// sample line sharing that base name — the monitor shows one incident count
// per tile, not one per label value.
func parseCounters(metricsText string) counterSnapshot {
	var c counterSnapshot
	sc := bufio.NewScanner(strings.NewReader(metricsText))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := parseMetricSample(line)
		if !ok {
			continue
		}
		switch name {
		case "kanz_oms_orders_quarantined_total":
			c.Quarantined += value
		case "kanz_compliance_ungoverned_orders_total":
			c.Ungoverned += value
		case "kanz_compliance_unpriced_orders_total":
			c.Unpriced += value
		case "kanz_oms_shared_collateral_orders_total":
			c.SharedCollateral += value
		case "kanz_oms_unverified_venue_account_total":
			c.UnverifiedVenueAccount += value
		}
	}
	return c
}

// parseMetricSample parses one Prometheus exposition sample line —
// `name value` or `name{label="v",...} value` — into its bare metric name
// (labels stripped) and integer value. A line this monitor doesn't
// recognize, or can't parse, returns ok=false and is skipped rather than
// erroring the whole scrape over one unrelated line (e.g. a HELP/TYPE
// comment, or an unrelated gauge like go_goroutines).
func parseMetricSample(line string) (name string, value int, ok bool) {
	fields := strings.Fields(line)
	if len(fields) != 2 {
		return "", 0, false
	}
	name = fields[0]
	if i := strings.IndexByte(name, '{'); i >= 0 {
		name = name[:i]
	}
	f, err := strconv.ParseFloat(fields[1], 64)
	if err != nil {
		return "", 0, false
	}
	return name, int(f), true
}
