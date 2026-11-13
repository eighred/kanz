package transporterr

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"testing"
)

type classifiedFailure struct{ timeout, temporary bool }

func (*classifiedFailure) Error() string     { return "dummy-peer-canary" }
func (e *classifiedFailure) Timeout() bool   { return e.timeout }
func (e *classifiedFailure) Temporary() bool { return e.temporary }

func TestRedactDiscardsEveryUnsafeCause(t *testing.T) {
	for _, flags := range []struct{ timeout, temporary bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		t.Run(fmt.Sprint(flags), func(t *testing.T) {
			original := &classifiedFailure{flags.timeout, flags.temporary}
			raw := fmt.Errorf("dummy-header-canary: %w", &url.Error{Op: "GET", URL: "https://fixture.invalid?signature=dummy-signature-canary", Err: original})
			got := Redact(raw)
			var network net.Error
			var classified networkClassification
			if !errors.As(got, &network) || !errors.As(got, &classified) || network.Timeout() != flags.timeout || classified.Temporary() != flags.temporary {
				t.Fatal("transport classification changed")
			}
			var unsafe *url.Error
			if errors.As(got, &unsafe) || errors.Is(got, original) {
				t.Fatal("unsafe underlying cause was retained")
			}
			assertSafeChain(t, got)
		})
	}
}

func TestRedactPreservesOnlySafeContextIdentity(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			got := Redact(fmt.Errorf("dummy-peer-canary: %w", cause))
			if !errors.Is(got, cause) {
				t.Fatal("context identity lost")
			}
			assertSafeChain(t, got)
		})
	}
	if Redact(nil) != nil {
		t.Fatal("nil error became a failure")
	}
}

func TestRedactPreservesOrdinaryErrorClass(t *testing.T) {
	got := Redact(errors.New("dummy-peer-canary"))
	var network net.Error
	if errors.As(got, &network) {
		t.Fatal("ordinary failure became a net.Error")
	}
	assertSafeChain(t, got)
}

func assertSafeChain(t *testing.T, err error) {
	t.Helper()
	for depth := 0; err != nil; depth++ {
		if depth > 4 {
			t.Fatal("unexpected error chain")
		}
		text := fmt.Sprintf("%v | %+v | %#v", err, err, err)
		if strings.Contains(text, "canary") || strings.Contains(text, "signature=") || strings.Contains(text, "fixture.invalid") {
			t.Fatal("projected error chain exposes transport material")
		}
		err = errors.Unwrap(err)
	}
}
