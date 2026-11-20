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
