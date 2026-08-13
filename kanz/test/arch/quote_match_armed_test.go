package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// AN ADAPTER THAT SHIPS A SYMBOL MAP MUST ARM THE CHECK ON IT (#419).
//
// A symbol map is two claims side by side: a canonical instrument id, and the
// symbol the adapter actually sends to the exchange. Only the second decides what
// is bought. The estate shipped `BTC-USD=BTCUSDT` — a USDT-quoted pair recorded
// as dollars — and nothing compared them, because past that point the symbol goes
// to the exchange and the id goes into the ledger and no layer sees both (#407).
//
// THE COST OF A MISMATCH IS NOT A WRONG LABEL. Positions, currency exposure and
// every limit checked against them end up denominated in an asset the adapter
// does not hold; and the rows written under the wrong id neither join to the rest
// of the estate nor disappear, which is what #419 is about cleaning up.
//
// #407 ADDED THE CHECK AND LEFT IT OFF, deliberately and with the reason written
// down at services/venue-binance/cmd/venue-binance/quote_match_test.go: "a
// control that refuses to start every adapter nobody has corrected yet is a
// trading outage, and it is armed WITH the estate in hand."
//
// That was right, and its precondition is now met — the manifests ship the
// corrected maps, and TestTheCorrectedMappingPasses proves the check is silent
// against them. What was missing is the arming step, which nothing required and
// nobody had done: the check sat in every adapter, shipped disabled, for the
// whole window in which it could have prevented a recurrence.
//
// WHAT IT CHECKS: a deploy manifest that sets *_SYMBOLS also sets
// *_REQUIRE_QUOTE_MATCH to "true".
//
// WHAT IT CANNOT CHECK: that the map itself is correct. That is the adapter's own
// startup check — which is exactly what this guard exists to keep switched on.

const quoteMatchDeployDir = "infra/deploy"

var (
	// symbolsEnvRe finds a symbol-map env var and captures its service prefix.
	// MARKET_INGEST_BINANCE_SYMBOLS is deliberately NOT matched: market-ingest
	// publishes marks and has no quote-match check to arm, so requiring one would
	// name a variable that does not exist. Anchored on the whole name for that
	// reason rather than on a "_SYMBOLS" suffix alone.
	symbolsEnvRe = regexp.MustCompile(`name:\s*([A-Z]+)_SYMBOLS\b`)
	// requireEnvRe finds the armed control for a given prefix.
	requireEnvTemplate = `name:\s*%s_REQUIRE_QUOTE_MATCH,\s*value:\s*"?true"?`
)

// quoteMatchExempt maps a manifest basename to the reason its symbol map needs no
// armed check, and what retires the entry.
//
// EMPTY, AND THAT IS THE POINT. An adapter that trades under ids it cannot
// justify is the defect; there is no deployment for which shipping that unchecked
// is fine. The map and the dead-entry arm exist so a future exemption is argued
// in a diff, not so one is expected.
var quoteMatchExempt = map[string]string{}

func TestEverySymbolMapArmsItsQuoteMatch(t *testing.T) {
	root := moduleRoot(t)
	manifests, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(quoteMatchDeployDir), "*.yaml"))
	if err != nil {
		t.Fatalf("glob %s: %v", quoteMatchDeployDir, err)
	}
	// NON-VACUITY, the directory half: a moved or renamed deploy tree returns
	// nothing and this guard would pass having checked no manifest at all.
	if len(manifests) == 0 {
		t.Fatalf("found zero manifests under %s — the scanner is broken, not the estate", quoteMatchDeployDir)
	}

	var unarmed []string
	seenExempt := map[string]bool{}
	withSymbols := 0

	for _, m := range manifests {
		body, err := os.ReadFile(m)
		if err != nil {
			t.Fatalf("read %s: %v", m, err)
		}
		// Normalise line endings: git on Windows hands these over with CRLF, and a
		// guard a contributor cannot run is one they cannot trust.
		manifest := strings.ReplaceAll(string(body), "\r\n", "\n")
		base := filepath.Base(m)

		hits := symbolsEnvRe.FindAllStringSubmatch(manifest, -1)
		if len(hits) == 0 {
			continue
		}
		withSymbols++

		if reason, ok := quoteMatchExempt[base]; ok {
			seenExempt[base] = true
			t.Logf("%s: exempt — %s", base, reason)
			continue
		}
		for _, h := range hits {
			prefix := h[1]
			armed := regexp.MustCompile(strings.Replace(requireEnvTemplate, "%s", prefix, 1))
			if !armed.MatchString(manifest) {
				unarmed = append(unarmed, base+" ("+prefix+"_SYMBOLS without "+prefix+"_REQUIRE_QUOTE_MATCH=true)")
			}
		}
	}

	// NON-VACUITY, the match half: if the env-var spelling changes, this finds no
	// symbol maps and passes while checking nothing.
	if withSymbols < 2 {
		t.Fatalf("found symbol maps in %d manifest(s) — expected at least 2 (the binance and okx "+
			"adapters). The env-var spelling changed and this guard is asserting nothing", withSymbols)
	}

	if len(unarmed) > 0 {
		sort.Strings(unarmed)
		t.Errorf("%d symbol map(s) ship with the quote-match check DISABLED: %v.\n"+
			"An instrument id whose quote the venue does not trade denominates that adapter's "+
			"positions, currency exposure and every limit checked against them in an asset it does "+
			"not hold — and the rows written under it neither join to the rest of the estate nor "+
			"disappear (#407, #419). Unarmed, that is a startup log line nobody reads. Set "+
			"<PREFIX>_REQUIRE_QUOTE_MATCH to \"true\", or add an argued entry to quoteMatchExempt.",
			len(unarmed), unarmed)
	}

	// DEAD-ENTRY ARM: an exemption for a manifest that no longer ships a symbol
	// map has outlived its repair and would wave the next one through.
	for base, reason := range quoteMatchExempt {
		if !seenExempt[base] {
			t.Errorf("exemption for %q (%s) matches no manifest shipping a symbol map — delete it",
				base, reason)
		}
	}
}
