//go:build synthetic_handoff

package main

import (
	"os"
	"testing"

	"github.com/eighred/kanz/services/oms/tools/syntheticvenue"
)

func TestSyntheticFundedRemainderGoRustGo(t *testing.T) {
	if os.Getenv("KANZ_SYNTHETIC_GO_FUNDED_REMAINDER") != "1" || os.Getenv("KANZ_SYNTHETIC_GO_REMAINDER_FIRST_BOOKED") == "1" || os.Getenv("KANZ_SYNTHETIC_GO_REMAINDER_RESTART") == "1" {
		t.Skip("requires the disposable funded remainder handback")
	}
	exerciseGoRustGoProfile(t, fundedWorkingTenant, noGoWithdrawal, syntheticvenue.ProjectRemainder)
}

func TestSyntheticFundedRemainderFirstBookedGoRustGo(t *testing.T) {
	if os.Getenv("KANZ_SYNTHETIC_GO_FUNDED_REMAINDER") != "1" || os.Getenv("KANZ_SYNTHETIC_GO_REMAINDER_FIRST_BOOKED") != "1" || os.Getenv("KANZ_SYNTHETIC_GO_REMAINDER_RESTART") == "1" {
		t.Skip("requires a separate funded handback and an accounting booking before the remainder")
	}
	exerciseGoRustGoProfile(t, fundedWorkingTenant, noGoWithdrawal, syntheticvenue.ProjectRemainder)
}

func TestSyntheticFundedRemainderRestartGoRustGo(t *testing.T) {
	if os.Getenv("KANZ_SYNTHETIC_GO_FUNDED_REMAINDER") != "1" || os.Getenv("KANZ_SYNTHETIC_GO_REMAINDER_RESTART") != "1" {
		t.Skip("requires a disposable remainder publication restart")
	}
	exerciseGoRustGoProfile(t, fundedWorkingTenant, noGoWithdrawal, syntheticvenue.ProjectRemainder)
}
