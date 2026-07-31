package version

import (
	"strings"
	"testing"
)

// The precedence, exhaustively. The third row is the one #143 is about: a
// container build has no link stamp and no VCS metadata, and it must not
// produce a value anybody would mistake for a deliberate choice.
func TestPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stamp, rev string
		revOK      bool
		want       string
	}{
		{"a release build uses its stamp", "v0.2.0", "", false, "v0.2.0"},
		{"the stamp beats the VCS revision", "v0.2.0", "abc123", true, "v0.2.0"},
		{"a local build falls back to the revision", "", "abc123", true, "abc123"},
		{"no stamp and no VCS is UNSTAMPED", "", "", false, Unstamped},
		{"VCS present but empty is still unstamped", "", "", true, Unstamped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := choose(tc.stamp, tc.rev, tc.revOK); got != tc.want {
				t.Fatalf("choose(%q, %q, %v) = %q, want %q", tc.stamp, tc.rev, tc.revOK, got, tc.want)
			}
		})
	}
}

// THE FALLBACK MUST NOT BE "dev".
//
// This is the whole defect, so it gets its own guard rather than riding on a
// table row. 26 binaries reported "dev" from images built at a signed release
// tag and nobody noticed, because "dev" reads as something a person chose. A
// future edit that "tidies" the fallback back to "dev" restores the bug exactly.
func TestTheUnstampedValueIsNotMistakableForAChoice(t *testing.T) {
	if Unstamped == "dev" || Unstamped == "" {
		t.Fatalf("Unstamped = %q — a plausible-looking version is why this went unnoticed "+
			"for 26 binaries; it must read as a defect", Unstamped)
	}
	if strings.ContainsAny(Unstamped, " \t") {
		t.Errorf("Unstamped = %q contains whitespace — it lands in an OTel resource "+
			"attribute and an envelope field", Unstamped)
	}
}

// IsStamped is what a service asks to decide whether to warn at startup, so it
// must track the stamp and nothing else.
func TestIsStampedTracksTheLinkTimeStampOnly(t *testing.T) {
	saved := stamped
	t.Cleanup(func() { stamped = saved })

	stamped = ""
	if IsStamped() {
		t.Error("IsStamped() is true with no link-time stamp — a service would stay quiet " +
			"about publishing FACTs it cannot attribute to a build")
	}
	stamped = "v9.9.9"
	if !IsStamped() {
		t.Error("IsStamped() is false with a link-time stamp set")
	}
}

// String must be stable across calls: it is read once per span and once per
// published envelope, and a value that could differ between two reads would put
// two versions on one process's output.
func TestStringIsStable(t *testing.T) {
	first := String()
	if first == "" {
		t.Fatal("String() is empty — producer_version is a REQUIRED envelope field")
	}
	for i := 0; i < 3; i++ {
		if got := String(); got != first {
			t.Fatalf("String() returned %q then %q", first, got)
		}
	}
}

// NON-VACUITY for the test binary itself: this test runs from a git checkout, so
// the VCS path must actually be producing something. If fromBuildInfo silently
// stopped working, every local build would report Unstamped and the tests above
// would still pass.
func TestBuildInfoPathWorksInThisCheckout(t *testing.T) {
	rev, ok := fromBuildInfo()
	if !ok || rev == "" {
		t.Skip("no VCS metadata in this build — expected when tests run from an unpacked " +
			"source tree rather than a checkout")
	}
	if strings.TrimSuffix(rev, "-dirty") == "" {
		t.Errorf("fromBuildInfo returned %q, which is only the dirty marker", rev)
	}
}
