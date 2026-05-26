package auth

import (
	"context"
	"errors"
	"testing"
)

func TestVerifyIssuer(t *testing.T) {
	p := &Principal{Subject: "akif", Tenant: "acme"}
	delegated := &Principal{
		Subject: "akif", Tenant: "acme",
		Claims: map[string]any{ClaimIssuers: []any{"strategy:momentum-v2"}},
	}

	cases := []struct {
		name    string
		p       *Principal
		issuer  string
		wantErr error
	}{
		{"self as user", p, "user:akif", nil},
		{"self as operator (any type prefix)", p, "operator:akif", nil},
		{"self no type prefix", p, "akif", nil},
		{"forged other id", p, "user:eve", ErrForgedIssuer},
		{"delegated strategy allowed", delegated, "strategy:momentum-v2", nil},
		{"delegated not granted", p, "strategy:momentum-v2", ErrForgedIssuer},
		{"empty issuer", p, "", nil /* sentinel below */},
		{"nil principal", nil, "user:akif", ErrUnauthenticated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := VerifyIssuer(tc.p, tc.issuer)
			switch {
			case tc.name == "empty issuer":
				if err == nil {
					t.Fatal("empty issuer must be rejected")
				}
			case tc.wantErr == nil:
				if err != nil {
					t.Fatalf("want allow, got %v", err)
				}
			default:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("got %v, want %v", err, tc.wantErr)
				}
			}
		})
	}
}

func TestVerifyCommandIssuer_Context(t *testing.T) {
	// No principal on ctx ⇒ cannot issue.
	if err := VerifyCommandIssuer(context.Background(), "user:akif"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("got %v, want ErrUnauthenticated", err)
	}
	ctx := WithPrincipal(context.Background(), &Principal{Subject: "akif", Tenant: "acme"})
	if err := VerifyCommandIssuer(ctx, "user:akif"); err != nil {
		t.Fatalf("self-issue should pass: %v", err)
	}
	if err := VerifyCommandIssuer(ctx, "user:eve"); !errors.Is(err, ErrForgedIssuer) {
		t.Fatalf("got %v, want ErrForgedIssuer", err)
	}
}
