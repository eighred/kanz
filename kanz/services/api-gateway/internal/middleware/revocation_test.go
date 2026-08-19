package middleware

// THE GATEWAY'S HALF OF PER-SUBJECT REVOCATION (#532).

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/revocation"
)

type stubAuthn struct {
	p   *Principal
	err error
}

func (s stubAuthn) Authenticate(string) (*Principal, error) { return s.p, s.err }

type stubChecker struct {
	err error
	// sawSubject and sawIssuedAt record what the decorator handed over, so a
	// test can prove the check is fed the token's own values rather than
	// something reconstructed.
	sawSubject  string
	sawIssuedAt time.Time
}

func (s *stubChecker) Check(subject string, issuedAt time.Time) error {
	s.sawSubject, s.sawIssuedAt = subject, issuedAt
	return s.err
}

// A REVOKED TOKEN IS A 401, and it says nothing more than any other bad
// credential. Telling the holder WHICH check refused them tells the holder of a
// stolen token when the theft was noticed.
func TestARevokedTokenIsRefusedAsUnauthenticated(t *testing.T) {
	chk := &stubChecker{err: revocation.ErrRevoked}
	r, err := NewRevoking(stubAuthn{p: &Principal{Subject: "trader-a"}}, chk)
	if err != nil {
		t.Fatalf("NewRevoking: %v", err)
	}
	p, err := r.Authenticate("token")
	if p != nil {
		t.Error("a revoked caller was returned a Principal")
	}
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("a revoked token produced %v, want ErrUnauthenticated", err)
	}
	if errors.Is(err, ErrAuthUnavailable) {
		t.Fatal("a revoked token answered 'unavailable' — the caller would get a 503 and retry " +
			"forever instead of being told their credential is no good")
	}
}

// AN UNUSABLE FEED IS A 503, NOT A 401. The distinction is the control: a 401 is
// an instruction the web client acts on by destroying the session and sending
// the user to log in — for every signed-in user at once, at the exact moment the
// identity service is unreachable.
func TestAnUnusableFeedIsUnavailableAndNotUnauthenticated(t *testing.T) {
	chk := &stubChecker{err: fmt.Errorf("%w: never fetched", revocation.ErrUnusable)}
	r, err := NewRevoking(stubAuthn{p: &Principal{Subject: "trader-a"}}, chk)
	if err != nil {
		t.Fatalf("NewRevoking: %v", err)
	}
	if _, err := r.Authenticate("token"); !errors.Is(err, ErrAuthUnavailable) {
		t.Fatalf("an unusable feed produced %v, want ErrAuthUnavailable", err)
	}
}

// AN UNCLASSIFIED ERROR REFUSES. A checker that answers something this decorator
// does not recognise has not said the caller is fine, and admitting them would
// make a bug in the checker indistinguishable from a clean bill of health.
func TestAnUnrecognisedCheckerErrorRefuses(t *testing.T) {
	chk := &stubChecker{err: errors.New("something else entirely")}
	r, err := NewRevoking(stubAuthn{p: &Principal{Subject: "trader-a"}}, chk)
	if err != nil {
		t.Fatalf("NewRevoking: %v", err)
	}
	if _, err := r.Authenticate("token"); err == nil {
		t.Fatal("an unrecognised checker error admitted the caller")
	}
}

// The check runs on the token's OWN subject and issuance time, and only after
// the inner authenticator has verified it — so a forged token never reaches the
// lookup and cannot be used to probe the denylist.
func TestTheCheckRunsOnTheVerifiedTokenAndOnlyOnAVerifiedToken(t *testing.T) {
	iat := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)
	chk := &stubChecker{}
	r, err := NewRevoking(stubAuthn{p: &Principal{Subject: "trader-a", IssuedAt: iat}}, chk)
	if err != nil {
		t.Fatalf("NewRevoking: %v", err)
	}
	if _, err := r.Authenticate("token"); err != nil {
		t.Fatalf("a good token was refused: %v", err)
	}
	if chk.sawSubject != "trader-a" || !chk.sawIssuedAt.Equal(iat) {
		t.Fatalf("the checker saw (%q, %s), want (trader-a, %s) — the decorator is not passing the "+
			"token's own values", chk.sawSubject, chk.sawIssuedAt, iat)
	}

	chk2 := &stubChecker{}
	r2, err := NewRevoking(stubAuthn{err: ErrUnauthenticated}, chk2)
	if err != nil {
		t.Fatalf("NewRevoking: %v", err)
	}
	if _, err := r2.Authenticate("forged"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("a token the inner authenticator refused produced %v", err)
	}
	if chk2.sawSubject != "" {
		t.Fatal("an UNVERIFIED token reached the revocation lookup — anybody could probe the " +
			"denylist with forged tokens")
	}
}

// AN AUTHENTICATOR THAT RETURNS NOTHING AT ALL MUST NOT PANIC THE INGRESS.
// A nil principal with a nil error is a bug in the inner authenticator; a nil
// dereference on the request path turns that bug into a total outage of order
// entry, which is a far worse way to find out.
func TestANilPrincipalWithNoErrorIsRefusedRatherThanPanicking(t *testing.T) {
	r, err := NewRevoking(stubAuthn{}, &stubChecker{})
	if err != nil {
		t.Fatalf("NewRevoking: %v", err)
	}
	p, err := r.Authenticate("token")
	if p != nil {
		t.Error("a principal appeared from nowhere")
	}
	if !errors.Is(err, ErrAuthUnavailable) {
		t.Fatalf("got %v, want ErrAuthUnavailable", err)
	}
}

// A NIL CHECKER IS REFUSED AT CONSTRUCTION. Wrapping with one produces a
// pass-through that looks, in a composition root and in a diff, exactly like a
// working revocation control — the shape #535 and #539 both took.
func TestWrappingWithNoCheckerIsRefused(t *testing.T) {
	if _, err := NewRevoking(stubAuthn{}, nil); err == nil {
		t.Fatal("an authenticator was wrapped with a nil checker — the control would be present, " +
			"wired, and enforcing nothing")
	}
	if _, err := NewRevoking(nil, &stubChecker{}); err == nil {
		t.Fatal("a nil authenticator was wrapped")
	}
}

// THE STATUS CODES, END TO END THROUGH THE MIDDLEWARE. This is the half that
// actually reaches the caller, and until #532 every authenticator error left
// here as a 401.
func TestAuthAnswers503WhenItCannotJudgeAndNot401(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })

	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"revoked", ErrUnauthenticated, http.StatusUnauthorized},
		{"feed unusable", fmt.Errorf("%w: never fetched", ErrAuthUnavailable), http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := Auth(stubAuthn{err: tc.err}, "", nil)(next)
			req := httptest.NewRequest(http.MethodGet, "/v1/orders", nil)
			req.Header.Set("Authorization", "Bearer x")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("%s answered %d, want %d", tc.name, rec.Code, tc.want)
			}
		})
	}
}
