package identity

// THE CREDENTIAL PRIMITIVE (#99).
//
// Every failure mode here is silent. A hash that verifies the wrong password, a
// salt that is not random, a comparison that leaks timing — none of them produce
// an error, a failing probe, or an unusual log line. The system keeps working
// and keeps letting the wrong people in, which is why these are asserted rather
// than assumed from "it uses argon2".

import (
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"
)

func TestAHashVerifiesItsOwnPasswordAndNothingElse(t *testing.T) {
	h, err := HashCredential("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashCredential: %v", err)
	}
	if err := Verify(h, "correct horse battery staple"); err != nil {
		t.Fatalf("the correct credential did not verify: %v", err)
	}
	for _, wrong := range []string{
		"correct horse battery stapl",   // one char short
		"correct horse battery staple ", // trailing space
		"Correct horse battery staple",  // case
		"",
	} {
		if err := Verify(h, wrong); err == nil {
			t.Errorf("Verify accepted %q", wrong)
		}
	}
}

// THE SALT MUST BE RANDOM PER CREDENTIAL.
//
// Two users with the same password must not share a digest. If they do, one
// stolen hash tells an attacker every account that reused that password, and a
// precomputed table works against the whole store at once.
func TestTheSameCredentialHashesDifferentlyEveryTime(t *testing.T) {
	const pw = "same password"
	seen := map[Hash]bool{}
	for i := 0; i < 8; i++ {
		h, err := HashCredential(pw)
		if err != nil {
			t.Fatalf("HashCredential: %v", err)
		}
		if seen[h] {
			t.Fatal("two hashes of the same password are IDENTICAL — the salt is not random " +
				"(or is not being used). One stolen hash would then identify every account " +
				"sharing that password, and one table would break the whole store.")
		}
		seen[h] = true
		if err := Verify(h, pw); err != nil {
			t.Fatalf("hash %d did not verify its own password: %v", i, err)
		}
	}
}

// AN EMPTY CREDENTIAL IS REFUSED AT THE DOOR.
//
// Not because it would fail to hash — it hashes fine — but because an account
// whose credential is "" is an account anyone can use, and the only place to
// refuse it is before it is stored.
func TestAnEmptyCredentialCannotBeHashed(t *testing.T) {
	if _, err := HashCredential(""); err == nil {
		t.Fatal("HashCredential(\"\") succeeded — an account with an empty credential is an " +
			"account with no credential")
	}
}

// A MALFORMED HASH FAILS CLOSED, and reports the same error as a wrong password.
//
// A distinguishable error is an oracle: it tells a caller which accounts exist,
// or which rows are corrupt and therefore worth attacking.
func TestAMalformedHashIsRefusedAndIndistinguishable(t *testing.T) {
	good, err := HashCredential("pw")
	if err != nil {
		t.Fatalf("HashCredential: %v", err)
	}
	for name, h := range map[string]Hash{
		"empty":            "",
		"not a phc string": "hunter2",
		"wrong algorithm":  Hash(strings.Replace(string(good), "argon2id", "argon2i", 1)),
		"truncated":        Hash(strings.Join(strings.Split(string(good), "$")[:4], "$")),
		"bad base64":       Hash(strings.Replace(string(good), "$m=", "$!!$m=", 1)),
	} {
		err := Verify(h, "pw")
		if err == nil {
			t.Errorf("%s: Verify accepted a malformed hash", name)
			continue
		}
		if err != ErrCredentialMismatch {
			t.Errorf("%s: Verify returned %v, want ErrCredentialMismatch — a distinguishable "+
				"error tells a caller which accounts exist or which rows are corrupt", name, err)
		}
	}
}

// THE PARAMETERS TRAVEL WITH THE HASH, so raising them later does not invalidate
// every credential already stored.
func TestAHashCarriesTheParametersItWasMadeWith(t *testing.T) {
	h, err := HashCredential("pw")
	if err != nil {
		t.Fatalf("HashCredential: %v", err)
	}
	for _, want := range []string{"$argon2id$", "v=19", "m=65536", "t=2", "p=4"} {
		if !strings.Contains(string(h), want) {
			t.Errorf("encoded hash %q does not carry %q.\n\n"+
				"Without the parameters in the string, Verify has to assume the CURRENT ones — so "+
				"the day they are raised, every credential in the store stops verifying at once.",
				h, want)
		}
	}
}

// A HASH MADE WITH WEAKER PARAMETERS STILL VERIFIES, and is reported for upgrade.
//
// This is the property the PHC encoding exists for. Without it, raising the cost
// protects only accounts created after the change — leaving the oldest accounts,
// which are the most valuable ones, on the weakest parameters forever.
func TestAWeakerHashStillVerifiesAndIsFlaggedForRehash(t *testing.T) {
	// A hash written under deliberately weaker parameters than the current ones.
	weak := encodeWith(t, "pw", 1, 8*1024, 1)

	if err := Verify(weak, "pw"); err != nil {
		t.Fatalf("a credential stored under older parameters no longer verifies: %v.\n\n"+
			"Raising the cost would lock every existing user out of the platform.", err)
	}
	if !NeedsRehash(weak) {
		t.Error("NeedsRehash said no for a hash weaker than the current parameters — it would " +
			"never be upgraded, so the raise protects only new accounts")
	}
	current, err := HashCredential("pw")
	if err != nil {
		t.Fatalf("HashCredential: %v", err)
	}
	if NeedsRehash(current) {
		t.Error("NeedsRehash said yes for a freshly-made hash — every login would rewrite the " +
			"credential, turning a read path into a write path")
	}
	if !NeedsRehash("not a hash") {
		t.Error("NeedsRehash said no for an unparseable hash — it cannot be verified against, " +
			"so the next successful login is the only chance to repair it")
	}
}

// encodeWith derives a hash under explicit parameters, standing in for a
// credential written before the current constants were raised.
func encodeWith(t *testing.T, plaintext string, time, memKiB uint32, threads uint8) Hash {
	t.Helper()
	salt := []byte("0123456789abcdef")
	key := argon2.IDKey([]byte(plaintext), salt, time, memKiB, threads, argonKeyLen)
	return encode(salt, key, time, memKiB, threads)
}

// A CHOSEN CREDENTIAL HAS A MINIMUM LENGTH (#364).
//
// Before this existed the only rule was non-empty, so a one-character password
// was accepted for an account carrying kanz-trader — which places orders.
func TestAChosenCredentialMustBeLongEnough(t *testing.T) {
	if err := ValidateCredential("x"); err == nil {
		t.Fatal("a one-character credential was accepted for an account that can place orders")
	}
	if err := ValidateCredential(strings.Repeat("a", MinCredentialLen-1)); err == nil {
		t.Fatalf("a credential of %d characters was accepted, one under the minimum",
			MinCredentialLen-1)
	}
	if err := ValidateCredential(strings.Repeat("a", MinCredentialLen)); err != nil {
		t.Fatalf("a credential of exactly the minimum was refused: %v", err)
	}
	if err := ValidateCredential(strings.Repeat("a", MaxCredentialLen+1)); err == nil {
		t.Fatal("an unbounded credential was accepted — that is work an unauthenticated caller " +
			"can hand this process")
	}
}

// LENGTH IS COUNTED IN RUNES, NOT BYTES, so a passphrase written in a script
// whose characters take several bytes each is not held to a shorter limit than
// the same length in ASCII.
func TestCredentialLengthIsCountedInRunes(t *testing.T) {
	// Twelve runes, thirty-six bytes.
	twelve := strings.Repeat("日", MinCredentialLen)
	if got := len(twelve); got == MinCredentialLen {
		t.Fatalf("test is not exercising multi-byte runes: byte length %d", got)
	}
	if err := ValidateCredential(twelve); err != nil {
		t.Fatalf("a %d-rune passphrase was refused: %v — counting bytes would make this pass "+
			"while an ASCII passphrase of the same visible length failed", MinCredentialLen, err)
	}
	// Eleven runes must still be refused, so the rune count is a real check.
	if err := ValidateCredential(strings.Repeat("日", MinCredentialLen-1)); err == nil {
		t.Fatal("an under-length multi-byte passphrase was accepted — the check is counting bytes")
	}
}

// THE POLICY IS NOT APPLIED WHEN VERIFYING. An account whose credential predates
// a raised minimum must still be able to sign in: applying today's rule at login
// would turn a hardening change into an estate-wide lockout.
func TestVerifyIgnoresTheLengthPolicy(t *testing.T) {
	h, err := HashCredential("short")
	if err != nil {
		t.Fatalf("HashCredential: %v", err)
	}
	if err := Verify(h, "short"); err != nil {
		t.Fatalf("an existing short credential stopped verifying: %v\n\n"+
			"Enforcing the minimum on the verify path locks out every account created "+
			"before the policy, which is an outage rather than a hardening.", err)
	}
}
