package engine

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRecomputeCounterHasBothOutcomesBeforeAnEvent(t *testing.T) {
	reg := prometheus.NewRegistry()
	NewMetrics(reg)
	const want = `
# HELP kanz_risk_recompute_total portfolio recomputes, by status (ok|error).
# TYPE kanz_risk_recompute_total counter
kanz_risk_recompute_total{status="error"} 0
kanz_risk_recompute_total{status="ok"} 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "kanz_risk_recompute_total"); err != nil {
		t.Fatal(err)
	}
}
