package execution

// Declaration decorators belong to the router's admission checks. Returning
// their narrow embedded Venue interface hides the adapter's optional query,
// cancellation and identity methods. Select the original adapter only after
// routing; do not manufacture optional methods on an adapter that lacks them.
func operationalVenue(v Venue) Venue {
	for {
		next, ok := declaredVenue(v)
		if !ok {
			return v
		}
		v = next
	}
}

func declaredVenue(v Venue) (Venue, bool) {
	switch declared := v.(type) {
	case orderTypeAware:
		return declared.Venue, true
	case timeInForceAware:
		return declared.Venue, true
	case marginModeAware:
		return declared.Venue, true
	default:
		return nil, false
	}
}

// A later declaration overrides only its own dimension. In particular a margin
// declaration must not make an inner order-type or time-in-force refusal vanish.
func venueCapability[T any](v Venue) (T, bool) {
	for {
		if capability, ok := v.(T); ok {
			return capability, true
		}
		next, ok := declaredVenue(v)
		if !ok {
			var absent T
			return absent, false
		}
		v = next
	}
}
