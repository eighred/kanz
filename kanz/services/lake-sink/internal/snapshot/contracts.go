// Package snapshot owns finite, authorized views of the active landing run.
// It is not an archive completeness or financial computation authority.
package snapshot

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	lakepb "github.com/eighred/kanz/kanz-schemas-go/lake/v1"
	"github.com/eighred/kanz/pkg/auth"
	"google.golang.org/protobuf/proto"
)

const (
	MaxCaptureBytes              = 64 << 20
	MaxParquetBytes              = 96 << 20
	MaxResponseBytes             = 1 << 20
	MaxRows                      = 100000
	MaxFiles                     = 64
	MaxPageRows                  = 1000
	MaxControlBytes              = 64 << 10
	ProtocolVersion              = 1
	SchemaVersion                = 1
	ActionCreate     auth.Action = "lake.snapshot.create"
	ActionRead       auth.Action = "lake.snapshot.read"
	ResourceSnapshot             = "lake_snapshot"
)

var (
	ErrInvalid  = errors.New("snapshot: invalid input")
	ErrDenied   = errors.New("snapshot: access denied")
	ErrNotFound = errors.New("snapshot: unavailable")
	ErrCapacity = errors.New("snapshot: capacity exceeded")
	ErrConflict = errors.New("snapshot: request conflicts")
	ErrCustody  = errors.New("snapshot: invalid custody")
	idPattern   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{15,127}$`)
)

func boundedText(s string, maximum int) bool {
	return s != "" && len(s) <= maximum && utf8.ValidString(s) && strings.TrimSpace(s) == s && !strings.ContainsAny(s, "\x00\r\n")
}

// Caller reconstructs only a gateway-delegated principal. The owning gRPC
// listener must first authenticate the exact gateway SPIFFE peer. This helper
// neither authenticates callers nor substitutes a service principal on failure.
func Caller(c *lakepb.GatewayCaller) (*auth.Principal, error) {
	if c == nil || len(c.ProtoReflect().GetUnknown()) != 0 || !boundedText(c.Subject, 256) || !boundedText(c.Tenant, 256) || len(c.Roles) > 64 {
		return nil, ErrDenied
	}
	// v1 is explicitly tenant-wide. A restricted caller cannot obtain broader
	// access merely because generic rows have no portfolio attribution.
	if len(c.Portfolios) != 0 {
		return nil, ErrDenied
	}
	seen := make(map[string]bool, len(c.Roles))
	for _, role := range c.Roles {
		if !boundedText(role, 128) || seen[role] {
			return nil, ErrDenied
		}
		seen[role] = true
	}
	return &auth.Principal{Subject: c.Subject, Tenant: c.Tenant, Roles: append([]string(nil), c.Roles...), Portfolios: append([]string(nil), c.Portfolios...)}, nil
}

// Authorize checks actual stored resource ownership, not a tenant supplied in
// business input. Every metadata read, including FAILED resources, uses it.
func Authorize(ctx context.Context, a auth.Authorizer, p *auth.Principal, action auth.Action, id, owner string) error {
	if a == nil || p == nil || !boundedText(p.Subject, 256) || !boundedText(owner, 256) || p.Tenant != owner || len(p.Portfolios) != 0 {
		return ErrNotFound
	}
	if action != ActionCreate && action != ActionRead {
		return ErrDenied
	}
	d := a.Authorize(ctx, auth.Request{Principal: p, Action: action, Resource: auth.Resource{Type: ResourceSnapshot, ID: id, Tenant: owner}})
	if !d.Allow {
		if d.Code.IsIsolation() {
			return ErrNotFound
		}
		return ErrDenied
	}
	return nil
}

func ValidateSelection(s *lakepb.SnapshotSelection) error {
	if s == nil || len(s.ProtoReflect().GetUnknown()) != 0 || s.EventTimeFrom == nil || s.EventTimeUntil == nil || len(s.EventTimeFrom.ProtoReflect().GetUnknown()) != 0 || len(s.EventTimeUntil.ProtoReflect().GetUnknown()) != 0 || s.EventTimeFrom.CheckValid() != nil || s.EventTimeUntil.CheckValid() != nil || !s.EventTimeUntil.AsTime().After(s.EventTimeFrom.AsTime()) || len(s.EventTypes) == 0 || len(s.EventTypes) > 64 {
		return ErrInvalid
	}
	seen := make(map[string]bool, len(s.EventTypes))
	for _, typ := range s.EventTypes {
		if !boundedText(typ, 256) || seen[typ] {
			return ErrInvalid
		}
		seen[typ] = true
	}
	return nil
}

func ValidateCreate(input *lakepb.CreateSnapshotInput) error {
	if input == nil || len(input.ProtoReflect().GetUnknown()) != 0 || !idPattern.MatchString(input.RequestId) || proto.Size(input) > MaxControlBytes {
		return ErrInvalid
	}
	return ValidateSelection(input.Selection)
}

func ValidateQuery(q *lakepb.SnapshotQuery) error {
	if q == nil || len(q.ProtoReflect().GetUnknown()) != 0 || q.PageSize == 0 || q.PageSize > MaxPageRows {
		return ErrInvalid
	}
	switch q.Operation {
	case lakepb.SnapshotQueryOperation_SNAPSHOT_QUERY_OPERATION_LIST_EVENTS, lakepb.SnapshotQueryOperation_SNAPSHOT_QUERY_OPERATION_DECODE_FAILURES:
		return nil
	case lakepb.SnapshotQueryOperation_SNAPSHOT_QUERY_OPERATION_COUNT_BY_EVENT_TYPE:
		if q.AfterOrdinal == nil {
			return nil
		}
	}
	return ErrInvalid
}
