package curve

import (
	"time"

	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"
	"google.golang.org/protobuf/proto"
)

// CapturedInput retains the configured input, even when no usable quote existed.
// ResolvedRate is the exact numerical value handed to the authoritative solver.
type CapturedInput struct {
	InstrumentID string
	Kind         QuoteKind
	Tenor, Span  float64
	Observation  *marketpb.MarketDataEvent
	ResolvedRate *float64
}

// InputCapture describes one immutable latest-cache cut, not historical-as-of proof.
type InputCapture struct {
	CapturedAt time.Time
	Inputs     []CapturedInput
}

// CalibrationCapture is a detached, completed authoritative calculation for a
// shadow observer. Neither its slices nor observations alias the installed curve.
type CalibrationCapture struct {
	Currency      string
	RequestedAsOf time.Time
	CapturedAt    time.Time
	Interp        Interpolation
	Strip         Strip
	Tenors, Zeros []float64
}

func (s Strip) clone() Strip {
	out := s
	out.Quotes = append([]RateQuote(nil), s.Quotes...)
	out.Coverage = s.Coverage.clone()
	if s.Capture != nil {
		capture := *s.Capture
		capture.Inputs = append([]CapturedInput(nil), s.Capture.Inputs...)
		for i := range capture.Inputs {
			input := &capture.Inputs[i]
			if input.Observation != nil {
				input.Observation = proto.Clone(input.Observation).(*marketpb.MarketDataEvent)
			}
			if input.ResolvedRate != nil {
				value := *input.ResolvedRate
				input.ResolvedRate = &value
			}
		}
		out.Capture = &capture
	}
	return out
}
