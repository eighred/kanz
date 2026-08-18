package main

import (
	"log/slog"
	"testing"
	"time"

	"github.com/eighred/kanz/services/datamaster/internal/config"
	"github.com/eighred/kanz/services/datamaster/internal/store"
)

// A PURGE THAT IS NOT RUNNING MUST SAY SO AT WARN (#563).
//
// The retention window is not a cleanup setting — it is how long a proposer can
// still see that their override lapsed. So a deployment with the purge disabled
// is one where exception_override_proposals grows with the lapse rate forever,
// and the only person who could notice is reading this log.
//
// Asserted on the LEVEL. The disabled case logging at Info reads as
// confirmation, which is the mistake #539 made one service over: a posture that
// is wrong must not be logged at the level a posture that is right uses.
func TestAPurgeThatIsNotRunningIsWarnedAboutNotAnnounced(t *testing.T) {
	h := &storeLogCapture{}
	cfg := config.Config{LapsedProposalRetention: 7 * 24 * time.Hour} // interval unset ⇒ 0

	if armProposalPurge(cfg, store.NewMemoryProposals(), slog.New(h)) {
		t.Fatal("the purge loop was started with no interval configured — it would spin on a zero " +
			"ticker, which panics")
	}
	if !h.hasWarnContaining("DATAMASTER_PROPOSAL_PURGE_INTERVAL is 0", "grows with the lapse rate") {
		t.Fatalf("no WARN naming the disabled purge.\n\n"+
			"An operator has no other signal: the surface keeps listing lapsed proposals and looks "+
			"healthy while the table it reads from never stops growing.\n\nlog was:\n%s", h.all())
	}
}

// AND A RUNNING PURGE MUST NOT WARN. Without this the guard above is satisfied
// by warning unconditionally, which is how a real warning stops being read.
func TestAConfiguredPurgeIsArmedQuietly(t *testing.T) {
	h := &storeLogCapture{}
	cfg := config.Config{
		LapsedProposalRetention: 7 * 24 * time.Hour,
		ProposalPurgeInterval:   time.Hour,
	}

	if !armProposalPurge(cfg, store.NewMemoryProposals(), slog.New(h)) {
		t.Fatal("a configured purge was not armed")
	}
	if h.hasWarnContaining("DATAMASTER_PROPOSAL_PURGE_INTERVAL") {
		t.Errorf("a configured purge still warns — the condition is inverted or unguarded:\n%s", h.all())
	}
}

// NO STORE, NO POSTURE. An ephemeral deployment has no proposals to purge, and
// openStores has already said what that deployment is; a second warning here
// would be a second answer to a question already answered.
func TestWithNoProposalStoreThereIsNothingToPurge(t *testing.T) {
	h := &storeLogCapture{}
	if armProposalPurge(config.Config{ProposalPurgeInterval: time.Hour}, nil, slog.New(h)) {
		t.Fatal("the purge loop was armed with no proposal store — it would call PurgeLapsed on nil")
	}
	if h.hasWarnContaining("DATAMASTER_PROPOSAL_PURGE_INTERVAL") {
		t.Errorf("warned about a purge interval for a deployment that stores no proposals:\n%s", h.all())
	}
}
