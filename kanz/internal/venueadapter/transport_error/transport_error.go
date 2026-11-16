// Package transporterr projects REST transport failures onto safe diagnostics.
package transporterr

import (
	"context"
	"errors"
)

// Redact discards transport text and URLs while preserving the context identity
// and timeout/temporary classification used by callers. The original error is
// never retained: wrapping it would expose signed requests through Unwrap or As
// even if this error's own text were safe. Arbitrary cause identities are not
// preserved; business/status errors must stay outside this boundary.
func Redact(err error) error {
	if err == nil {
		return nil
	}
	f := &failure{}
	switch {
	case errors.Is(err, context.Canceled):
		f.cause = context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		f.cause = context.DeadlineExceeded
	}
	var classified networkClassification
	if errors.As(err, &classified) {
		return &networkFailure{failure: f, timeout: classified.Timeout(), temporary: classified.Temporary()}
	}
	return f
}

// This structural net.Error contract preserves legacy classification, including
// Temporary, for callers. The flag is diagnostic compatibility, not retry policy.
type networkClassification interface {
	error
	Timeout() bool
	Temporary() bool
}

type failure struct {
	cause error // only the two safe context sentinels, or nil
}

func (e *failure) Error() string {
	if e.cause == context.Canceled {
		return "REST transport canceled"
	}
	return "REST transport failed"
}

func (e *failure) Unwrap() error { return e.cause }

type networkFailure struct {
	*failure
	timeout, temporary bool
}

func (e *networkFailure) Error() string {
	if e.timeout && e.cause != context.Canceled {
		return "REST transport timed out"
	}
	return e.failure.Error()
}

func (e *networkFailure) Timeout() bool   { return e.timeout }
func (e *networkFailure) Temporary() bool { return e.temporary }
