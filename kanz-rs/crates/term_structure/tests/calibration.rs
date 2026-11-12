use kanz_term_structure::{Curve, Error, Interpolation, QuoteKind, RateQuote, MAX_PILLARS};
use serde::Deserialize;
use std::collections::{BTreeMap, BTreeSet};

fn interpolation(i: u8) -> Result<Interpolation, Error> {
    match i {
        0 => Ok(Interpolation::LinearZero),
        1 => Ok(Interpolation::LogLinearDiscount),
        _ => Err(Error::InvalidInput),
    }
}
fn kind(i: i32) -> Result<QuoteKind, Error> {
    match i {
        0 => Ok(QuoteKind::Deposit),
        1 => Ok(QuoteKind::Future),
        2 => Ok(QuoteKind::Swap),
        _ => Err(Error::InvalidInput),
    }
}
fn evaluate(c: &Curve, quotes: &[RateQuote], queries: &[f64]) -> Result<Vec<f64>, Error> {
    let mut out = c.tenors();
    out.extend(c.zeros());
    for &q in quotes {
        out.push(c.quote_rate(q)?);
    }
    for &t in queries {
        out.extend([c.zero(t)?, c.discount(t)?]);
    }
    Ok(out)
}
fn compare(actual: &[f64], expected: &[f64], pillars: usize) -> Result<(), String> {
    if actual.len() != expected.len() {
        return Err("field inventory differs".into());
    }
    for (i, (&a, &b)) in actual.iter().zip(expected).enumerate() {
        // Fixed rate/discount relative budget, with no absolute floor that
        // would bless a zero in place of the sub-epsilon reference.
        let budget = 2048.0 * f64::EPSILON * pillars as f64 * b.abs();
        if !a.is_finite()
            || !b.is_finite()
            || !budget.is_finite()
            || (i < pillars && a.to_bits() != b.to_bits())
            || (i >= pillars && (a - b).abs() > budget)
        {
            return Err(format!("field {i}: {a} vs {b}, budget {budget}"));
        }
    }
    Ok(())
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Reference {
    method: String,
    cases: Vec<ReferenceCase>,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ReferenceCase {
    id: String,
    interp: u8,
    quotes: Vec<ReferenceQuote>,
    queries: Vec<String>,
    values: Vec<String>,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ReferenceQuote {
    kind: i32,
    tenor: String,
    span: String,
    value: String,
}

#[test]
fn independent_price_equations_and_tiny_rates() {
    let data: Reference = serde_json::from_str(include_str!("mixed_reference.json")).unwrap();
    assert_eq!(data.method, "decimal_price_newton_160_200_v2");
    assert_eq!(data.cases.len(), 30);
    let mut ids = BTreeSet::new();
    for row in data.cases {
        assert!(ids.insert(row.id.clone()));
        let qs: Vec<_> = row
            .quotes
            .into_iter()
            .map(|q| RateQuote {
                kind: kind(q.kind).unwrap(),
                tenor: q.tenor.parse().unwrap(),
                span: q.span.parse().unwrap(),
                value: q.value.parse().unwrap(),
            })
            .collect();
        let c = Curve::calibrate(&qs, interpolation(row.interp).unwrap()).unwrap();
        let queries: Vec<f64> = row.queries.iter().map(|s| s.parse().unwrap()).collect();
        let expected: Vec<f64> = row.values.iter().map(|s| s.parse().unwrap()).collect();
        compare(&evaluate(&c, &qs, &queries).unwrap(), &expected, qs.len())
            .unwrap_or_else(|e| panic!("{}: {e}", row.id));
    }
}

#[test]
fn invalid_and_over_budget_calibration_refuses() {
    let q = RateQuote {
        kind: QuoteKind::Deposit,
        tenor: 1.0,
        span: 0.0,
        value: 0.03,
    };
    assert!(matches!(
        Curve::calibrate(&vec![q; MAX_PILLARS + 1], Interpolation::LinearZero),
        Err(Error::CapacityExceeded)
    ));
    for qs in [
        vec![],
        vec![RateQuote {
            value: f64::NAN,
            ..q
        }],
        vec![q, q],
        vec![RateQuote {
            kind: QuoteKind::Future,
            ..q
        }],
        vec![RateQuote {
            kind: QuoteKind::Swap,
            tenor: 4097.0,
            ..q
        }],
        vec![
            q,
            RateQuote {
                kind: QuoteKind::Future,
                tenor: 1e16,
                span: 0.25,
                value: 0.03,
            },
        ],
    ] {
        assert!(Curve::calibrate(&qs, Interpolation::LinearZero).is_err());
    }
}

#[test]
fn gapped_future_satisfies_rational_discount_identities() {
    for i in [0, 1] {
        let qs = [
            RateQuote {
                kind: QuoteKind::Deposit,
                tenor: 0.25,
                span: 0.0,
                value: 0.03,
            },
            RateQuote {
                kind: QuoteKind::Future,
                tenor: 1.0,
                span: 0.25,
                value: 0.035,
            },
        ];
        let c = Curve::calibrate(&qs, interpolation(i).unwrap()).unwrap();
        for q in qs {
            assert!(
                (c.quote_rate(q).unwrap() - q.value).abs() <= 128.0 * f64::EPSILON * q.value.abs()
            );
        }
        let df = c.discount(1.25).unwrap();
        // Direct rational consequences of DF(.25)=400/403 and the future
        // factor 807/800, independent of production's normalized weights.
        let (got, want) = if i == 0 {
            (df * df, (320000.0_f64 / 325221.0).powi(5))
        } else {
            (df, (400.0 / 403.0) * (800.0_f64 / 807.0).powi(4))
        };
        assert!((got - want).abs() <= 32.0 * f64::EPSILON * want);
    }
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Corpus {
    schema: u8,
    method: String,
    source_sha: String,
    case_count: usize,
    cases: Vec<Case>,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Case {
    id: String,
    interp: u8,
    quotes: Vec<Quote>,
    queries: Vec<u64>,
    outcome: String,
    values: Option<Vec<u64>>,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Quote {
    kind: i32,
    tenor: u64,
    span: u64,
    value: u64,
}

fn verify(raw: &[u8], source: &str) -> Result<BTreeMap<String, usize>, String> {
    if raw.len() > 8 * 1024 * 1024
        || source.len() != 40
        || !source
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
    {
        return Err("invalid capture source/bounds".into());
    }
    let data: Corpus = serde_json::from_slice(raw).map_err(|_| "malformed capture")?;
    if data.schema != 1
        || data.method != "go_mixed_v3"
        || data.source_sha != source
        || data.case_count != 306
        || data.cases.len() != 306
    {
        return Err("capture identity/inventory mismatch".into());
    }
    let mut ids = BTreeSet::new();
    let mut counts = BTreeMap::new();
    for row in data.cases {
        if row.id.is_empty()
            || row.id.len() > 128
            || !ids.insert(row.id.clone())
            || row.quotes.len() > 256
            || row.queries.len() != 6
        {
            return Err("case identity/bounds refused".into());
        }
        let qs: Result<Vec<_>, _> = row
            .quotes
            .iter()
            .map(|q| {
                Ok(RateQuote {
                    kind: kind(q.kind)?,
                    tenor: f64::from_bits(q.tenor),
                    span: f64::from_bits(q.span),
                    value: f64::from_bits(q.value),
                })
            })
            .collect();
        let built =
            qs.and_then(|qs| Curve::calibrate(&qs, interpolation(row.interp)?).map(|c| (qs, c)));
        match (row.outcome.as_str(), row.values, built) {
            ("refused", None, Err(_)) => (),
            ("value", Some(bits), Ok((qs, c))) => {
                if bits.len() != 3 * qs.len() + 12 {
                    return Err("result inventory differs".into());
                }
                let queries: Vec<_> = row.queries.into_iter().map(f64::from_bits).collect();
                let actual = evaluate(&c, &qs, &queries).map_err(|_| "candidate unavailable")?;
                let expected: Vec<_> = bits.into_iter().map(f64::from_bits).collect();
                compare(&actual, &expected, qs.len()).map_err(|e| format!("{}: {e}", row.id))?;
            }
            _ => return Err(format!("{}: outcome differs", row.id)),
        }
        *counts.entry(row.outcome).or_insert(0) += 1;
    }
    if counts != BTreeMap::from([("refused".into(), 32), ("value".into(), 274)]) {
        return Err("outcome inventory differs".into());
    }
    Ok(counts)
}

#[test]
fn missing_or_malformed_mixed_capture_refuses() {
    for raw in [b"".as_slice(), b"{}", b"null"] {
        assert!(verify(raw, "1f871549d8069a53e1c1d3db3a536c9c025d7bae").is_err());
    }
}

#[test]
#[ignore = "requires fresh actual Go capture and exact source; CI invokes explicitly"]
fn fresh_mixed_go_oracle_is_required() {
    let source = std::env::var("KANZ_MIXED_SOURCE").expect("KANZ_MIXED_SOURCE required");
    let path = std::env::var("KANZ_MIXED_CORPUS").expect("KANZ_MIXED_CORPUS required");
    let raw = std::fs::read(path).expect("fresh actual Go capture required");
    let counts = verify(&raw, &source).unwrap();
    let value: serde_json::Value = serde_json::from_slice(&raw).unwrap();
    for mutation in 0..6 {
        let mut broken = value.clone();
        match mutation {
            0 => broken["source_sha"] = "0000000000000000000000000000000000000000".into(),
            1 => {
                let n = broken["cases"][0]["quotes"].as_array().unwrap().len();
                let bits = broken["cases"][0]["values"][n].as_u64().unwrap();
                broken["cases"][0]["values"][n] = (f64::from_bits(bits) * 1.01).to_bits().into();
            }
            2 => broken["cases"][1]["id"] = broken["cases"][0]["id"].clone(),
            3 => {
                broken["cases"].as_array_mut().unwrap().pop();
            }
            4 => broken["method"] = "go_mixed_v1".into(),
            _ => broken["method"] = "go_mixed_v2".into(),
        }
        assert!(
            verify(&serde_json::to_vec(&broken).unwrap(), &source).is_err(),
            "negative control {mutation} accepted"
        );
    }
    println!("fresh mixed Go source={source} cases=306 outcomes={counts:?} negative_controls=6");
}
