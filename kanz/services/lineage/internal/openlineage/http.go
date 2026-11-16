package openlineage

import (
	"context"
	"errors"
	"net/http"
)

// A URL and an owner header do not make a canonical DataHub/OpenLineage URN
// tenant-isolated. A genuine scoped sink/storage contract remains under #1405.
// Refuse before network I/O, including direct/zero-value use outside config.
var ErrUnscopedSink = errors.New("openlineage: unscoped HTTP sink is not authorized")

type HTTPEmitter struct{}

func NewHTTPEmitter(_ string, _ *http.Client) (*HTTPEmitter, error) { return nil, ErrUnscopedSink }
func (*HTTPEmitter) Emit(context.Context, OwnedRunEvent) error      { return ErrUnscopedSink }
