package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// THE PUBLISHER AND THE CONSUMER MUST NAME THE SAME SUBJECT (#450).
//
// accounting announces what a portfolio can spend; the OMS folds that
// announcement into the balance its pre-trade buying-power gate reads. They are
// separate services, and Go's internal-package rule — the service-isolation
// invariant this directory enforces elsewhere — forbids either importing the
// other's internals. So the subject is written down TWICE.
//
// TWO LITERALS THAT MUST AGREE IS A DRIFT RISK, AND THE DRIFT IS SILENT. Nothing
// fails when they disagree: accounting publishes happily to one subject, the OMS
// subscribes happily to another, and the only symptom is that no balance ever
// arrives. Downstream that reads as "cash unavailable" — which the buying-power
// rule fails closed on — so the visible result is orders being REFUSED under a
// spending mandate, with a correct-looking reason and no hint that a typo caused
// it. The failure is safe and unattributable, which is the worst combination for
// finding it.
//
// A typo in either constant, or a rename of one alone, fails the build here
// instead.
//
// WHY NOT A SHARED CONSTANT: that is the better answer and it is not available.
// Placing it in kanz/internal/ would put an accounting subject in a package
// neither service owns, and the codebase's existing precedent — the fill
// subjects, which accounting takes from config and the OMS publishes from its
// own literal — is duplication WITHOUT a check. This is the same trade with the
// check added.

const (
	cashSubjectPublisher = "services/accounting/internal/consume/announce.go"
	cashSubjectConsumer  = "internal/cashview/cashview.go"
)

var (
	publisherSubjectRe = regexp.MustCompile(`SubjectPortfolioCash\s*=\s*"([^"]+)"`)
	consumerSubjectRe  = regexp.MustCompile(`(?m)^const Subject\s*=\s*"([^"]+)"`)
)

func TestTheCashSubjectAgreesAcrossTheTwoServices(t *testing.T) {
	root := moduleRoot(t)

	pub := subjectLiteral(t, filepath.Join(root, filepath.FromSlash(cashSubjectPublisher)),
		publisherSubjectRe, "consume.SubjectPortfolioCash")
	con := subjectLiteral(t, filepath.Join(root, filepath.FromSlash(cashSubjectConsumer)),
		consumerSubjectRe, "cashview.Subject")

	if pub != con {
		t.Fatalf("the cash-balance subject disagrees across services:\n"+
			"  publisher (%s): %q\n"+
			"  consumer  (%s): %q\n\n"+
			"accounting would publish to one subject and the OMS subscribe to another. NOTHING "+
			"would fail: no balance would ever arrive, every portfolio would read UNKNOWN, and "+
			"the buying-power rule would refuse orders with a correct-looking reason that names "+
			"no cause.",
			cashSubjectPublisher, pub, cashSubjectConsumer, con)
	}

	// NON-VACUITY: it must be a real {domain}.{entity}.{event_type} name, not an
	// empty string both sides happen to share.
	if !regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z0-9_]+){2}$`).MatchString(pub) {
		t.Fatalf("cash subject %q is not a {domain}.{entity}.{event_type} name — the Kafka "+
			"archiver maps the first two segments to a topic, so a malformed one is unarchived", pub)
	}
}

// subjectLiteral extracts the single subject constant from a source file, failing
// loudly when the declaration has moved rather than quietly comparing nothing.
func subjectLiteral(t *testing.T, path string, re *regexp.Regexp, what string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	m := re.FindStringSubmatch(string(body))
	if m == nil {
		t.Fatalf("could not find %s in %s — the constant was renamed or moved, and this guard "+
			"can no longer see it", what, path)
	}
	return m[1]
}
