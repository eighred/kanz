package filing

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/eighred/kanz/internal/dec"
	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	"github.com/google/uuid"
)

// PublicationScope describes an observed filing request, not permission to file
// or an approval of its dataset. The service obtains Tenant from the trusted
// principal and requires it to match its configured broker/storage tenant.
type PublicationScope struct {
	Tenant, Portfolio, ReportID, InputDigest string
	AsOf, RecordedAt                         time.Time
}

func (s PublicationScope) Validate() error {
	if !BoundedIdentifier(s.Tenant) || !BoundedIdentifier(s.Portfolio) || !UUIDv7(s.ReportID) || s.AsOf.IsZero() || s.RecordedAt.IsZero() || !Digest(s.InputDigest) {
		return errors.New("filing: incomplete publication scope")
	}
	return nil
}

func BoundedIdentifier(s string) bool {
	return s != "" && len(s) <= 256 && utf8.ValidString(s) && strings.TrimSpace(s) == s && !strings.ContainsAny(s, "\x00\r\n")
}

func UUIDv7(s string) bool {
	u, err := uuid.Parse(s)
	return err == nil && u.Version() == 7 && u.Variant() == uuid.RFC4122 && u.String() == s
}

func Digest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// FiledDecimal projects the number the existing renderer and signature actually
// carry. It does not round the underlying analytic a second time or claim that
// the filed eight-place value was the unrounded model output.
func FiledDecimal(li LineItem) (*commonpb.Decimal, error) {
	if li.Value == nil {
		return nil, errors.New("filing: missing filed value")
	}
	d, ok := dec.ParseProtoExact(dec.Str(li.Value))
	if !ok {
		return nil, errors.New("filing: filed value is not exactly representable on the wire")
	}
	return d, nil
}
