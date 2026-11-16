//! One-shot pure qualification protocol. No listener, persistence or authority.
use base64::{engine::general_purpose::STANDARD, Engine};
use kanz_order_core::{
    attribution_projection::{self as projection, Error, Outcome, RealisedVolume},
    proto::order::v1::OrderState,
    schedule_identity, Canonical,
};
use num_bigint::BigInt;
use num_rational::BigRational;
use num_traits::Zero;
use serde::Deserialize;
use serde_json::{json, Value};
use std::collections::{btree_map::Entry, BTreeMap};
use std::io::{Read, Write};

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct At {
    seconds: i64,
    nanos: i32,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Observation {
    instrument: String,
    venue: String,
    from_nanos: String,
    to_nanos: String,
    numerator: Option<String>,
    denominator: Option<String>,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Input {
    operation: String,
    #[serde(default)]
    state: String,
    #[serde(default)]
    children: Vec<String>,
    at: Option<At>,
    tape: Option<Vec<Observation>>,
    #[serde(default)]
    parent_id: String,
    #[serde(default)]
    index: i64,
    #[serde(default)]
    slices: i64,
    from_nanos: Option<String>,
    to_nanos: Option<String>,
}
type Intervals = BTreeMap<(i128, i128), Option<BigRational>>;
// Nested ordered maps allow borrowed string lookup: a large instrument/venue
// key is never cloned for each child interval lookup.
#[derive(Default)]
struct Tape(BTreeMap<String, BTreeMap<String, Intervals>>);
impl Tape {
    fn for_instrument<'a>(&'a self, instrument: &'a str) -> InstrumentTape<'a> {
        InstrumentTape {
            tape: self,
            instrument,
            venues: self.0.get(instrument),
        }
    }
    fn insert(
        &mut self,
        instrument: String,
        venue: String,
        from: i128,
        to: i128,
        value: Option<BigRational>,
    ) -> Result<(), Error> {
        match self
            .0
            .entry(instrument)
            .or_default()
            .entry(venue)
            .or_default()
            .entry((from, to))
        {
            Entry::Vacant(entry) => {
                entry.insert(value);
                Ok(())
            }
            Entry::Occupied(_) => Err(Error::InvalidInput),
        }
    }
}
struct InstrumentTape<'a> {
    tape: &'a Tape,
    instrument: &'a str,
    venues: Option<&'a BTreeMap<String, Intervals>>,
}
impl RealisedVolume for InstrumentTape<'_> {
    fn volume(&self, instrument: &str, venue: &str, from: i128, to: i128) -> Option<BigRational> {
        // The projection passes the same immutable parent slice every time.
        // Pointer+length equality avoids comparing its potentially large bytes
        // per child. Other slices retain exact value lookup, including unknowns.
        let venues = if std::ptr::eq(instrument, self.instrument) {
            self.venues
        } else {
            self.tape.0.get(instrument)
        };
        venues
            .and_then(|venues| venues.get(venue))
            .and_then(|intervals| intervals.get(&(from, to)))
            .and_then(Clone::clone)
    }
}
fn integer<T: std::str::FromStr>(s: &str) -> Result<T, Error> {
    if s.len() > 1300 {
        return Err(Error::Capacity);
    }
    s.parse().map_err(|_| Error::InvalidInput)
}
fn decode(raw: &str) -> Result<Canonical<OrderState>, Error> {
    if raw.len() > 1_398_104 {
        return Err(Error::Capacity);
    }
    Ok(Canonical::decode(
        &STANDARD.decode(raw).map_err(|_| Error::InvalidInput)?,
    )?)
}
fn canonical_nanos(raw: &str) -> Result<i128, Error> {
    let value = integer::<i128>(raw)?;
    let seconds = value.div_euclid(1_000_000_000);
    if !(-62_135_596_800..=253_402_300_799).contains(&seconds) {
        return Err(Error::InvalidInput);
    }
    Ok(value)
}
fn evaluate(input: Input) -> Result<Value, Error> {
    if input.operation == "Schedule" {
        let from = canonical_nanos(input.from_nanos.as_deref().ok_or(Error::InvalidInput)?)?;
        let to = canonical_nanos(input.to_nanos.as_deref().ok_or(Error::InvalidInput)?)?;
        return Ok(
            json!({"child_id":schedule_identity::child_id(&input.parent_id,input.index),"interval":schedule_identity::interval(from,to,input.slices,input.index).map(|(f,t)|[f.to_string(),t.to_string()])}),
        );
    }
    if input.operation != "Measure" {
        return Err(Error::InvalidInput);
    }
    let parent = decode(&input.state)?;
    let children = input
        .children
        .iter()
        .map(|s| decode(s))
        .collect::<Result<Vec<_>, _>>()?;
    let tape = input
        .tape
        .map(|rows| {
            if rows.len() > 100_000 {
                return Err(Error::Capacity);
            }
            let mut out = Tape::default();
            for r in rows {
                let from = integer::<i128>(&r.from_nanos)?;
                let to = integer::<i128>(&r.to_nanos)?;
                let value = match (r.numerator, r.denominator) {
                    (None, None) => None,
                    (Some(n), Some(d)) => {
                        let n = integer::<BigInt>(&n)?;
                        let d = integer::<BigInt>(&d)?;
                        if d.is_zero() {
                            return Err(Error::InvalidInput);
                        }
                        if n.bits() > 4096 || d.bits() > 4096 {
                            return Err(Error::Capacity);
                        }
                        Some(BigRational::new(n, d))
                    }
                    _ => return Err(Error::InvalidInput),
                };
                out.insert(r.instrument, r.venue, from, to, value)?;
            }
            Ok(out)
        })
        .transpose()?;
    let at = input.at.ok_or(Error::InvalidInput)?;
    let view = tape
        .as_ref()
        .map(|t| t.for_instrument(&parent.value().instrument_id));
    let result = projection::measure(
        &parent,
        &children,
        view.as_ref().map(|t| t as &dyn RealisedVolume),
        prost_types::Timestamp {
            seconds: at.seconds,
            nanos: at.nanos,
        },
    )?;
    let outcome = match result.outcome {
        Outcome::Ignored => "Ignored",
        Outcome::NoArrival => "NoArrival",
        Outcome::NoFills => "NoFills",
        Outcome::MissingFillPrice => "MissingFillPrice",
        Outcome::Unrepresentable => "Unrepresentable",
        Outcome::Captured => "Captured",
        Outcome::CapturedOutsideCanonicalDomain => "CapturedOutsideCanonicalDomain",
    };
    let exact = result.attribution.map(|a| json!({"quantity":a.quantity.to_string(),"notional":a.notional.to_string(),"average":a.average.to_string(),"arrival":a.arrival.to_string(),"shortfall":a.shortfall_bps.to_string(),"legs":a.decomposition.map(|d|[d.spread_bps.to_string(),d.impact_bps.to_string(),d.timing_bps.to_string()]),"slices":a.slices,"measured":a.measured}));
    let part = result.participation.map(|p| json!({"overall":p.overall.map(|r|r.to_string()),"maximum":p.maximum.map(|r|r.to_string()),"index":p.maximum_index,"venue":p.maximum_venue,"intervals":p.intervals,"measured":p.measured}));
    Ok(
        json!({"outcome":outcome,"payload":result.payload.map(|p|STANDARD.encode(p.wire())),"attribution":exact,"participation":part}),
    )
}
fn main() {
    let version = env!("KANZ_BUILD_VERSION");
    if version.len() != 40 || !version.bytes().all(|b| b.is_ascii_hexdigit()) {
        std::process::exit(2);
    }
    let mut bytes = Vec::new();
    let result = std::io::stdin()
        .take(4 * 1024 * 1024 + 1)
        .read_to_end(&mut bytes)
        .map_err(|_| Error::InvalidInput)
        .and_then(|_| {
            if bytes.len() > 4 * 1024 * 1024 {
                return Err(Error::Capacity);
            }
            evaluate(serde_json::from_slice(&bytes).map_err(|_| Error::InvalidInput)?)
        });
    let value = match result {
        Ok(mut result) => {
            result["version"] = json!(version);
            result["status"] = json!("ok");
            result
        }
        Err(e) => {
            json!({"version":version,"status":"error","code":match e { Error::Capacity=>"Capacity",Error::InvalidInput=>"InvalidInput" }})
        }
    };
    let bytes = serde_json::to_vec(&value).expect("bounded closed JSON");
    if bytes.len() > 2 * 1024 * 1024 {
        std::process::exit(2);
    }
    if std::io::stdout().write_all(&bytes).is_err() {
        std::process::exit(2);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn long_instrument_many_small_children_uses_one_resolved_view() {
        use kanz_order_core::proto::{
            common::v1::Decimal,
            order::v1::{ExecutionSchedule, OrderStatus},
        };
        use prost::Message;
        let instrument = "instrument".repeat(45_000);
        let decimal = |coefficient| {
            Some(Decimal {
                coefficient,
                exponent: 0,
            })
        };
        let state = OrderState {
            order_id: "parent".into(),
            instrument_id: instrument.clone(),
            status: OrderStatus::Filled as i32,
            side: 1,
            ordered_quantity: decimal(1000),
            arrival_price: decimal(100),
            execution_schedule: Some(ExecutionSchedule {
                algo: 3,
                slice_count: 1000,
                window_start: Some(prost_types::Timestamp {
                    seconds: 0,
                    nanos: 0,
                }),
                window_end: Some(prost_types::Timestamp {
                    seconds: 1,
                    nanos: 0,
                }),
                ..Default::default()
            }),
            ..Default::default()
        };
        let parent = Canonical::<OrderState>::decode(&state.encode_to_vec()).unwrap();
        let ids = schedule_identity::ChildIds::new("parent");
        let children: Vec<_> = (0..1000)
            .map(|i| {
                let child = OrderState {
                    order_id: ids.at(i),
                    parent_order_id: "parent".into(),
                    venue: "v".into(),
                    filled_quantity: decimal(1),
                    average_fill_price: decimal(100),
                    ..Default::default()
                };
                Canonical::<OrderState>::decode(&child.encode_to_vec()).unwrap()
            })
            .collect();
        let intervals = (0..1000_i128)
            .map(|i| {
                (
                    (i * 1_000_000, (i + 1) * 1_000_000),
                    Some(BigRational::from_integer(BigInt::from(10))),
                )
            })
            .collect();
        let tape = Tape(BTreeMap::from([(
            instrument.clone(),
            BTreeMap::from([("v".into(), intervals)]),
        )]));
        let view = tape.for_instrument(&parent.value().instrument_id);
        let result = projection::measure(
            &parent,
            &children,
            Some(&view),
            prost_types::Timestamp {
                seconds: 1,
                nanos: 0,
            },
        )
        .unwrap();
        assert_eq!(result.outcome, Outcome::Captured);
        assert_eq!(result.participation.unwrap().measured, 1000);
        let other_allocation = instrument.clone();
        assert_eq!(
            view.volume(&other_allocation, "v", 0, 1_000_000),
            Some(BigRational::from_integer(BigInt::from(10)))
        );
        assert_eq!(view.volume("missing", "v", 0, 1_000_000), None);
    }
    #[test]
    fn ordered_tape_retains_exact_keys_unknown_zero_and_duplicate_refusal() {
        let mut tape = Tape::default();
        for i in 0..20_000_i128 {
            tape.insert(
                "instrument".into(),
                "venue".into(),
                i,
                i + 1,
                Some(BigRational::from_integer(BigInt::from(i))),
            )
            .unwrap();
        }
        for i in (0..20_000_i128).rev() {
            assert_eq!(
                tape.for_instrument("instrument")
                    .volume("instrument", "venue", i, i + 1),
                Some(BigRational::from_integer(BigInt::from(i)))
            );
        }
        tape.insert("instrument".into(), "other".into(), 0, 1, None)
            .unwrap();
        assert!(tape
            .insert("instrument".into(), "other".into(), 0, 1, None)
            .is_err());
        assert_eq!(
            tape.for_instrument("instrument")
                .volume("instrument", "other", 0, 1),
            None
        );
        assert_eq!(
            tape.for_instrument("instrument")
                .volume("missing", "venue", 0, 1),
            None
        );
        assert_eq!(
            tape.for_instrument("instrument")
                .volume("instrument", "venue", 0, 2),
            None
        );
        assert_eq!(
            tape.for_instrument("instrument")
                .volume("instrument", "venue", 0, 1),
            Some(BigRational::zero())
        );
    }
}
