//! Existing Go TCA arithmetic. This measures execution; it grants no permission.
//! Intermediate values are rational. Wire projection separately retains Go's
//! eight-place quantization and magnitude-preserving rescaling.
use num_rational::BigRational as Rat;
use num_traits::{One, Signed, Zero};

#[derive(Debug, Clone)]
pub struct Slice {
    pub filled: Option<Rat>,
    pub average: Option<Rat>,
    pub release: Option<Rat>,
    pub bid: Option<Rat>,
    pub ask: Option<Rat>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Error {
    NoArrival,
    NoFills,
    MissingFillPrice,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Attribution {
    pub quantity: Rat,
    pub notional: Rat,
    pub average: Rat,
    pub arrival: Rat,
    pub shortfall_bps: Rat,
    /// Absent together means TOTAL_ONLY, never three fabricated zero costs.
    pub decomposition: Option<Decomposition>,
    pub slices: usize,
    pub measured: usize,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Decomposition {
    pub spread_bps: Rat,
    pub impact_bps: Rat,
    pub timing_bps: Rat,
}

/// Matches Go Attribute, including its existing non-SELL sign convention.
/// This is not an order-side admission validator. Fees are not in this measure.
pub fn attribute(side: i32, arrival: Option<&Rat>, slices: &[Slice]) -> Result<Attribution, Error> {
    let arrival = arrival
        .filter(|n| n.is_positive())
        .ok_or(Error::NoArrival)?;
    let sign = if side == 2 { -Rat::one() } else { Rat::one() };
    let mut quantity = Rat::zero();
    let mut notional = Rat::zero();
    let mut spread_cost = Rat::zero();
    let mut drift_cost = Rat::zero();
    let mut traded = 0;
    let mut measured = 0;
    for slice in slices {
        let Some(q) = slice.filled.as_ref().filter(|n| n.is_positive()) else {
            continue;
        };
        let price = slice
            .average
            .as_ref()
            .filter(|n| n.is_positive())
            .ok_or(Error::MissingFillPrice)?;
        traded += 1;
        quantity += q;
        notional += q * price;
        if let (Some(mark), Some(bid), Some(ask)) = (&slice.release, &slice.bid, &slice.ask) {
            if mark.is_positive() && bid.is_positive() && ask > bid {
                measured += 1;
                spread_cost += q * (ask - bid) / Rat::from_integer(2.into());
                drift_cost += q * &sign * (mark - arrival);
            }
        }
    }
    if quantity.is_zero() {
        return Err(Error::NoFills);
    }
    let average = &notional / &quantity;
    let denominator = &quantity * arrival;
    let bps = |cost: Rat| cost / &denominator * Rat::from_integer(10_000.into());
    let shortfall_bps = bps((&average - arrival) * &sign * &quantity);
    let decomposition = if measured == traded {
        let spread_bps = bps(spread_cost);
        let timing_bps = bps(drift_cost);
        let impact_bps = &shortfall_bps - &spread_bps - &timing_bps;
        Some(Decomposition {
            spread_bps,
            impact_bps,
            timing_bps,
        })
    } else {
        None
    };
    Ok(Attribution {
        quantity,
        notional,
        average,
        arrival: arrival.clone(),
        shortfall_bps,
        decomposition,
        slices: traded,
        measured,
    })
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ParticipationQuality {
    NotWorked,
    Unobservable,
    Partial,
    Measured,
}

/// One already matched canonical interval and its optional observed tape volume.
/// Unknown/nonpositive volume remains unknown; no admission decision uses it.
#[derive(Debug, Clone)]
pub struct ParticipationInterval {
    pub index: i32,
    pub venue: String,
    pub from_nanos: i128,
    pub to_nanos: i128,
    pub filled: Option<Rat>,
    pub volume: Option<Rat>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Participation {
    pub quality: ParticipationQuality,
    pub intervals: usize,
    pub measured: usize,
    pub overall: Option<Rat>,
    pub maximum: Option<Rat>,
    pub maximum_index: i32,
    pub maximum_venue: String,
}

impl Participation {
    /// Preserve the Go predicate, including caps above one. Admission owns the
    /// acceptable cap domain; this measurement does not silently strengthen it.
    pub fn exceeds(&self, cap: Option<&Rat>) -> bool {
        matches!((cap, &self.maximum), (Some(c), Some(m)) if c.is_positive() && m > c)
    }
}

pub fn participation(intervals: &[ParticipationInterval]) -> Participation {
    let mut out = Participation {
        quality: ParticipationQuality::NotWorked,
        intervals: 0,
        measured: 0,
        overall: None,
        maximum: None,
        maximum_index: -1,
        maximum_venue: String::new(),
    };
    let mut filled = Rat::zero();
    let mut printed = Rat::zero();
    for interval in intervals {
        if interval.to_nanos <= interval.from_nanos {
            continue;
        }
        let Some(q) = interval.filled.as_ref().filter(|n| n.is_positive()) else {
            continue;
        };
        out.intervals += 1;
        let Some(volume) = interval.volume.as_ref().filter(|n| n.is_positive()) else {
            continue;
        };
        out.measured += 1;
        filled += q;
        printed += volume;
        let rate = q / volume;
        if out.maximum.as_ref().is_none_or(|old| &rate > old) {
            out.maximum = Some(rate);
            out.maximum_index = interval.index;
            out.maximum_venue.clone_from(&interval.venue);
        }
    }
    out.quality = if out.intervals == 0 {
        ParticipationQuality::NotWorked
    } else if out.measured == 0 {
        ParticipationQuality::Unobservable
    } else if out.intervals == out.measured {
        ParticipationQuality::Measured
    } else {
        ParticipationQuality::Partial
    };
    if out.measured != 0 {
        out.overall = Some(filled / printed);
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    fn n(v: i64) -> Rat {
        Rat::from_integer(v.into())
    }
    #[test]
    fn observed_legs_conserve_signed_arrival_cost() {
        let slices = [Slice {
            filled: Some(n(3)),
            average: Some(n(103)),
            release: Some(n(101)),
            bid: Some(n(100)),
            ask: Some(n(102)),
        }];
        let buy = attribute(1, Some(&n(100)), &slices).unwrap();
        let legs = buy.decomposition.unwrap();
        // Arrival notional300; price cost9; spread3; drift3; residual3.
        assert_eq!(buy.shortfall_bps, n(300));
        assert_eq!(legs.spread_bps, n(100));
        assert_eq!(legs.timing_bps, n(100));
        assert_eq!(legs.impact_bps, n(100));
        let sell = attribute(2, Some(&n(100)), &slices).unwrap();
        let legs = sell.decomposition.unwrap();
        assert_eq!(sell.shortfall_bps, n(-300));
        assert_eq!(legs.spread_bps, n(100));
        assert_eq!(legs.timing_bps, n(-100));
        assert_eq!(legs.impact_bps, n(-300));
    }
    #[test]
    fn absent_quotes_are_not_zero_legs_and_wire_rounding_is_separate() {
        let slices = [Slice {
            filled: Some(n(3)),
            average: Some(Rat::new(4.into(), 3.into())),
            release: None,
            bid: None,
            ask: None,
        }];
        let a = attribute(1, Some(&n(1)), &slices).unwrap();
        assert_eq!(a.shortfall_bps, Rat::new(10_000.into(), 3.into()));
        assert!(a.decomposition.is_none());
        let wire = crate::decimal(a.average).unwrap();
        assert_eq!((wire.coefficient, wire.exponent), (133_333_333, -8));
        assert_eq!(attribute(1, None, &slices), Err(Error::NoArrival));
    }
    #[test]
    fn partial_tape_excludes_both_sums_and_preserves_first_maximum() {
        let rows = [
            ParticipationInterval {
                index: 2,
                venue: "a".into(),
                from_nanos: 0,
                to_nanos: 1,
                filled: Some(n(2)),
                volume: Some(n(10)),
            },
            ParticipationInterval {
                index: 7,
                venue: "b".into(),
                from_nanos: 1,
                to_nanos: 2,
                filled: Some(n(100)),
                volume: None,
            },
            ParticipationInterval {
                index: 8,
                venue: "c".into(),
                from_nanos: 2,
                to_nanos: 3,
                filled: Some(n(4)),
                volume: Some(n(20)),
            },
        ];
        let p = participation(&rows);
        assert_eq!(p.quality, ParticipationQuality::Partial);
        assert_eq!((p.intervals, p.measured, p.maximum_index), (3, 2, 2));
        assert_eq!(p.overall, Some(Rat::new(1.into(), 5.into())));
        assert!(!p.exceeds(Some(&Rat::new(1.into(), 5.into()))));
        assert!(p.exceeds(Some(&Rat::new(1.into(), 10.into()))));
    }
}
