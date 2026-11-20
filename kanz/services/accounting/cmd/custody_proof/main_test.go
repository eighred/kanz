package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/eighred/kanz/services/accounting/internal/custody"
)

func fixture(t *testing.T, file string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestSignedAccountEvidence(t *testing.T) {
	var rows []struct {
		Kind string `json:"kind"`
		Key  []byte `json:"public_key"`
	}
	if err := json.Unmarshal(fixture(t, "issuers.json"), &rows); err != nil || len(rows) != 2 || bytes.Equal(rows[0].Key, rows[1].Key) {
		t.Fatal("two distinct issuer keys required", err)
	}
	for i, issuer := range rows {
		raw := fixture(t, issuer.Kind+".json")
		sig := fixture(t, issuer.Kind+".sig")
		e, err := custody.ParseAccountEvidence(raw)
		if err != nil || e.Kind != issuer.Kind || !custody.VerifyAccountEvidence(e, raw, sig, issuer.Key) {
			t.Fatal("verified fixture", e.Kind, err)
		}
		other := rows[1-i]
		if custody.VerifyAccountEvidence(e, raw, sig, other.Key) || custody.VerifyAccountEvidence(e, raw, sig[:ed25519.SignatureSize-1], issuer.Key) || custody.VerifyAccountEvidence(e, append(raw, ' '), sig, issuer.Key) {
			t.Fatal("accepted alternate trust root or byte sequence")
		}
		mutations := []func(*custody.AccountEvidence){
			func(e *custody.AccountEvidence) { e.TenantID = "elsewhere" }, func(e *custody.AccountEvidence) { e.Authority = "someone-else" },
			func(e *custody.AccountEvidence) { e.AccountID = "other" }, func(e *custody.AccountEvidence) { e.ProofID = "another-proof" },
			func(e *custody.AccountEvidence) { e.EffectiveFrom = "2000-01-01" }, func(e *custody.AccountEvidence) { e.EffectiveUntil = "2039-01-01" },
			func(e *custody.AccountEvidence) { e.PortfolioID = "another-portfolio" }, func(e *custody.AccountEvidence) { e.CustodianID = "another-custodian" },
			func(e *custody.AccountEvidence) { e.Revision++ }, func(e *custody.AccountEvidence) { e.ExchangeAccountID = "other-venue-account" },
		}
		for _, change := range mutations {
			changed := e
			change(&changed)
			if custody.VerifyAccountEvidence(changed, raw, sig, issuer.Key) {
				t.Fatal("unbound row field", changed.Kind)
			}
		}
	}
}

func TestAccountEvidenceRefusesAmbiguousInput(t *testing.T) {
	raw := fixture(t, "venue_identity.json")
	cases := [][]byte{
		append(append([]byte{}, raw...), []byte("{}")...), bytes.Replace(raw, []byte(`"venue":`), []byte(`"venue":"OTHER","venue":`), 1),
		bytes.Replace(raw, []byte(`"venue":`), []byte(`"Venue":`), 1), bytes.Replace(raw, []byte(`"venue":`), []byte(`"revision":1,"venue":`), 1),
		bytes.Replace(raw, []byte("xs-account-1"), nil, 1), bytes.Replace(raw, []byte("2030-01-01"), []byte("2020-02-04"), 1),
		bytes.Replace(raw, []byte("2020-02-04"), []byte("0000-01-01"), 1), bytes.Repeat([]byte{' '}, custody.AccountEvidenceLimit+1),
	}
	for i, b := range cases {
		if _, err := custody.ParseAccountEvidence(b); err == nil {
			t.Fatalf("accepted malformed case %d", i)
		}
	}
}

func TestAssignmentWithdrawalRequiresItsOwnSignature(t *testing.T) {
	var issuers []struct {
		Kind string
		Key  []byte `json:"public_key"`
	}
	if err := json.Unmarshal(fixture(t, "issuers.json"), &issuers); err != nil {
		t.Fatal(err)
	}
	var key []byte
	for _, i := range issuers {
		if i.Kind == "custody_assignment" {
			key = i.Key
		}
	}
	for _, f := range []struct {
		name      string
		rev       int64
		withdrawn bool
	}{{"custody_assignment", 1, false}, {"custody_assignment_withdrawn", 2, true}, {"custody_assignment_reinstated", 3, false}} {
		raw := fixture(t, f.name+".json")
		sig := fixture(t, f.name+".sig")
		e, err := custody.ParseAccountEvidence(raw)
		if err != nil || e.Revision != f.rev || e.Withdrawn != f.withdrawn || !custody.VerifyAccountEvidence(e, raw, sig, key) {
			t.Fatal("incorrect signed governance revision", f.name)
		}
		e.Withdrawn = !e.Withdrawn
		if custody.VerifyAccountEvidence(e, raw, sig, key) {
			t.Fatal("altered withdrawal admitted", f.name)
		}
	}
}
