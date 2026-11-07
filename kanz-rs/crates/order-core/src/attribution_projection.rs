//! Pure terminal attribution projection. Persistence, tenant-scoped child
//! selection, metric emission and outbox delivery remain the orchestration owner.
use crate::attribution::{
    self, Attribution, Participation, ParticipationInterval, ParticipationQuality, Slice,
};
use crate::proto::{
    common::v1::Decimal,
    order::v1::{ExecutionAttributionRecorded, OrderState},
};
use crate::{aggregate, rational, schedule_identity, Canonical};
use kanz_proto_wire::{Schema, MAX_BYTES};
use num_rational::BigRational as Rat;
use prost::Message;
use prost_types::Timestamp;
use std::collections::BTreeSet;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Error {
    Capacity,
    InvalidInput,
}
impl From<kanz_proto_wire::Error> for Error {
    fn from(error: kanz_proto_wire::Error) -> Self {
        match error {
            kanz_proto_wire::Error::Capacity => Self::Capacity,
            _ => Self::InvalidInput,
        }
    }
}

/// Same observational seam as Go tca.RealisedVolume. None means unknown, never
/// zero. Implementations must describe this exact instrument/venue/interval.
/// There is no production binding here and this interface grants no permission.
pub trait RealisedVolume {
    fn volume(
        &self,
        instrument: &str,
        venue: &str,
        from_nanos: i128,
        to_nanos: i128,
    ) -> Option<Rat>;
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Outcome {
    Ignored,
    NoArrival,
    NoFills,
    MissingFillPrice,
    Unrepresentable,
    Captured,
    CapturedOutsideCanonicalDomain,
}
pub struct Measurement {
    pub outcome: Outcome,
    pub attribution: Option<Attribution>,
    pub participation: Option<Participation>,
    pub payload: Option<Canonical<ExecutionAttributionRecorded>>,
}
impl Measurement {
    fn omitted(outcome: Outcome) -> Self {
        Self {
            outcome,
            attribution: None,
            participation: None,
            payload: None,
        }
    }
}

fn timestamp(value: &Timestamp) -> Result<i128, Error> {
    if !(-62_135_596_800..=253_402_300_799).contains(&value.seconds)
        || !(0..1_000_000_000).contains(&value.nanos)
    {
        return Err(Error::InvalidInput);
    }
    Ok(i128::from(value.seconds) * 1_000_000_000 + i128::from(value.nanos))
}
fn optional_number(value: Option<&Decimal>) -> Result<Option<Rat>, Error> {
    value
        .map(|n| rational(Some(n)).ok_or(Error::InvalidInput))
        .transpose()
}
fn slice(st: &OrderState) -> Result<Slice, Error> {
    Ok(Slice {
        filled: optional_number(st.filled_quantity.as_ref())?,
        average: optional_number(st.average_fill_price.as_ref())?,
        release: optional_number(st.release_price.as_ref())?,
        bid: optional_number(st.release_bid.as_ref())?,
        ask: optional_number(st.release_ask.as_ref())?,
    })
}

/// Inputs are one canonical decision and the complete ListByParent result, in
/// its existing order. This pure owner does not establish database completeness.
/// It refuses duplicate/foreign child identities instead of silently measuring
/// a caller-assembled partial or multiply counted collection. The existing Rust
/// whole-result query's 100000-row/1MiB bounds apply before any calculation.
pub fn measure(
    parent: &Canonical<OrderState>,
    children: &[Canonical<OrderState>],
    source: Option<&dyn RealisedVolume>,
    now: Timestamp,
) -> Result<Measurement, Error> {
    let st = parent.value();
    if !st.parent_order_id.is_empty() || !aggregate::is_terminal(st) {
        return Ok(Measurement::omitted(Outcome::Ignored));
    }
    timestamp(&now)?;
    if children.len() > 100_000 {
        return Err(Error::Capacity);
    }
    let mut bytes = 0usize;
    let mut seen = BTreeSet::new();
    for child in children {
        bytes = bytes
            .checked_add(child.wire().len())
            .ok_or(Error::Capacity)?;
        if bytes > MAX_BYTES {
            return Err(Error::Capacity);
        }
        if child.value().parent_order_id != st.order_id
            || !seen.insert(child.value().order_id.as_str())
        {
            return Err(Error::InvalidInput);
        }
    }
    let slices = if st.execution_schedule.is_some() {
        children
            .iter()
            .map(|s| slice(s.value()))
            .collect::<Result<Vec<_>, _>>()?
    } else {
        vec![slice(st)?]
    };
    let arrival = optional_number(st.arrival_price.as_ref())?;
    let a = match attribution::attribute(st.side, arrival.as_ref(), &slices) {
        Ok(a) => a,
        Err(attribution::Error::Capacity) => return Err(Error::Capacity),
        Err(attribution::Error::NoArrival) => return Ok(Measurement::omitted(Outcome::NoArrival)),
        Err(attribution::Error::NoFills) => return Ok(Measurement::omitted(Outcome::NoFills)),
        Err(attribution::Error::MissingFillPrice) => {
            return Ok(Measurement::omitted(Outcome::MissingFillPrice))
        }
    };
    let mut intervals = Vec::new();
    if let Some(schedule) = &st.execution_schedule {
        // Go Timestamp.AsTime(nil) is the Unix epoch. No default current time.
        let zero = Timestamp {
            seconds: 0,
            nanos: 0,
        };
        let start = timestamp(schedule.window_start.as_ref().unwrap_or(&zero))?;
        let end = timestamp(schedule.window_end.as_ref().unwrap_or(&zero))?;
        let count = i32::try_from(schedule.slice_count).map_err(|_| Error::Capacity)?;
        // Existing admission owns the usable schedule domain. This pure reader
        // cannot allocate a caller-sized identity map without an explicit bound.
        if count > 100_000 {
            return Err(Error::Capacity);
        }
        let identities = schedule_identity::ChildIds::new(&st.order_id);
        let index: std::collections::BTreeMap<_, _> = (0..count)
            .map(|i| (identities.at(i64::from(i)), i))
            .collect();
        for child in children {
            let c = child.value();
            let Some(i) = index.get(&c.order_id).copied() else {
                continue;
            };
            let Some((from, to)) =
                schedule_identity::interval(start, end, i64::from(count), i64::from(i))
            else {
                continue;
            };
            let filled = optional_number(c.filled_quantity.as_ref())?;
            use num_traits::Signed;
            let volume = if to > from && filled.as_ref().is_some_and(|q| q.is_positive()) {
                source.and_then(|s| s.volume(&st.instrument_id, &c.venue, from, to))
            } else {
                None
            };
            // Decimal inputs stay bounded by canonical +/-64. A tape returns
            // rationals, so bound its representation before multiplying it.
            if volume
                .as_ref()
                .is_some_and(|v| v.numer().bits() > 4096 || v.denom().bits() > 4096)
            {
                return Err(Error::Capacity);
            }
            intervals.push(ParticipationInterval {
                index: i,
                venue: c.venue.clone(),
                from_nanos: from,
                to_nanos: to,
                filled,
                volume,
            });
        }
    }
    let p = attribution::participation(&intervals).map_err(|_| Error::Capacity)?;
    let payload = payload(parent, &a, &p, now)?;
    let outcome = if let Some(candidate) = &payload {
        // Re-encode only the already-decoded known fields for the domain walk;
        // retained wrong-wire unknown fields are not interpreted as Decimals.
        let known = candidate.value().encode_to_vec();
        let dynamic = prost_reflect::DynamicMessage::decode(
            ExecutionAttributionRecorded::descriptor()?,
            known.as_slice(),
        )
        .map_err(|_| Error::InvalidInput)?;
        if crate::decimal_domain(&dynamic, 0).is_ok() {
            Outcome::Captured
        } else {
            Outcome::CapturedOutsideCanonicalDomain
        }
    } else {
        Outcome::Unrepresentable
    };
    Ok(Measurement {
        outcome,
        attribution: Some(a),
        participation: Some(p),
        payload,
    })
}

fn payload(
    parent: &Canonical<OrderState>,
    a: &Attribution,
    p: &Participation,
    now: Timestamp,
) -> Result<Option<Canonical<ExecutionAttributionRecorded>>, Error> {
    let decimal = |value: Rat| -> Result<Option<Decimal>, Error> {
        if value.numer().bits() > 4096 || value.denom().bits() > 4096 {
            return Err(Error::Capacity);
        }
        Ok(crate::arithmetic::attribution_go_wire_decimal(value))
    };
    let st = parent.value();
    let (Some(qty), Some(avg), Some(arrival), Some(total)) = (
        decimal(a.quantity.clone())?,
        decimal(a.average.clone())?,
        decimal(a.arrival.clone())?,
        decimal(a.shortfall_bps.clone())?,
    ) else {
        return Ok(None);
    };
    let mut value = ExecutionAttributionRecorded {
        order_id: st.order_id.clone(),
        portfolio_id: st.portfolio_id.clone(),
        instrument_id: st.instrument_id.clone(),
        side: st.side,
        terminal_status: st.status,
        ordered_quantity: st.ordered_quantity,
        filled_quantity: Some(qty),
        average_fill_price: Some(avg),
        arrival_price: Some(arrival),
        arrival_at: st.arrival_at,
        shortfall_bps: Some(total),
        quality: 2,
        benchmark: 1,
        slices: i32::try_from(a.slices).map_err(|_| Error::Capacity)?,
        measured_slices: i32::try_from(a.measured).map_err(|_| Error::Capacity)?,
        measured_at: Some(now),
        participation_quality: match p.quality {
            ParticipationQuality::NotWorked => 1,
            ParticipationQuality::Measured => 2,
            ParticipationQuality::Partial => 3,
            ParticipationQuality::Unobservable => 4,
        },
        participation_intervals: i32::try_from(p.intervals).map_err(|_| Error::Capacity)?,
        measured_participation_intervals: i32::try_from(p.measured).map_err(|_| Error::Capacity)?,
        max_slice_participation_index: -1,
        execution_algo: st.execution_schedule.as_ref().map_or(0, |s| s.algo),
        ..Default::default()
    };
    let cap = optional_number(
        st.execution_schedule
            .as_ref()
            .and_then(|s| s.max_participation_rate.as_ref()),
    )?;
    value.participation_cap = match cap {
        Some(value) => decimal(value)?,
        None => None,
    };
    if let (Some(overall), Some(maximum)) = (&p.overall, &p.maximum) {
        match (decimal(overall.clone())?, decimal(maximum.clone())?) {
            (Some(overall), Some(maximum)) => {
                value.realised_participation_rate = Some(overall);
                value.max_slice_participation_rate = Some(maximum);
                value.max_slice_participation_index = p.maximum_index;
            }
            _ => value.participation_quality = 4,
        }
    }
    if let Some(d) = &a.decomposition {
        let (Some(spread), Some(impact), Some(timing)) = (
            decimal(d.spread_bps.clone())?,
            decimal(d.impact_bps.clone())?,
            decimal(d.timing_bps.clone())?,
        ) else {
            return Ok(None);
        };
        value.spread_bps = Some(spread);
        value.impact_bps = Some(impact);
        value.timing_bps = Some(timing);
        value.quality = 1;
    }
    // These two fields are copied by Go, so retain their original normalized
    // nested unknown bytes. All computed fields intentionally receive new wire.
    value.ordered_quantity = None;
    value.arrival_at = None;
    let mut raw = value.encode_to_vec();
    for (name, number) in [("ordered_quantity", 6), ("arrival_at", 10)] {
        if let Some(message) = parent.message_wire(name)? {
            if raw
                .len()
                .checked_add(message.len() + 10)
                .is_none_or(|n| n > MAX_BYTES)
            {
                return Err(Error::Capacity);
            }
            kanz_proto_wire::put_message(number, &message, &mut raw);
        }
    }
    Ok(Some(Canonical::decode(&raw)?))
}
