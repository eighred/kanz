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
impl RealisedVolume for Tape {
    fn volume(&self, instrument: &str, venue: &str, from: i128, to: i128) -> Option<BigRational> {
        self.0
            .get(instrument)
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
    let result = projection::measure(
        &parent,
        &children,
        tape.as_ref().map(|t| t as &dyn RealisedVolume),
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
                tape.volume("instrument", "venue", i, i + 1),
                Some(BigRational::from_integer(BigInt::from(i)))
            );
        }
        tape.insert("instrument".into(), "other".into(), 0, 1, None)
            .unwrap();
        assert!(tape
            .insert("instrument".into(), "other".into(), 0, 1, None)
            .is_err());
        assert_eq!(tape.volume("instrument", "other", 0, 1), None);
        assert_eq!(tape.volume("missing", "venue", 0, 1), None);
        assert_eq!(tape.volume("instrument", "venue", 0, 2), None);
        assert_eq!(
            tape.volume("instrument", "venue", 0, 1),
            Some(BigRational::zero())
        );
    }
}
