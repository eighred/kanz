use kanz_option_pricing::{american::evaluate, Error, Exercise, Input, Right};
mod support;
use std::{collections::BTreeMap, fs::File, io::Read};

fn verify(raw: &[u8], expected_source: &str) -> Result<BTreeMap<String, usize>, String> {
    let corpus = support::capture(raw, expected_source, "go_american_crr_refusal_v1", 502)?;
    let mut counts = BTreeMap::new();
    for c in corpus.cases {
        let right = match c.right {
            0 => Right::Call,
            1 => Right::Put,
            _ => return Err("unknown right".into()),
        };
        let exercise = match c.exercise {
            0 => Exercise::European,
            1 => Exercise::American,
            _ => return Err("unknown exercise".into()),
        };
        let v = c.input_bits.map(f64::from_bits);
        let input = Input {
            right,
            exercise,
            spot: v[0],
            strike: v[1],
            years: v[2],
            rate: v[3],
            yield_rate: v[4],
            volatility: v[5],
        };
        let got = evaluate(input);
        match (c.outcome.as_str(), c.values_bits, got) {
            ("value", Some(bits), Ok(value)) => {
                let expected = bits.map(f64::from_bits);
                let actual = value.values();
                // Fixed before comparison: the 200-level binary64 budget,
                // propagated through the existing Go bump denominators.
                let p = 8192.0 * f64::EPSILON * input.spot.abs().max(input.strike.abs()).max(1.0);
                let h = input.spot * 0.01;
                let b = if input.years <= 0.0
                    || input.volatility <= 0.0
                    || input.spot <= 0.0
                    || input.strike <= 0.0
                {
                    [p; 6]
                } else {
                    [
                        p,
                        p / h,
                        4.0 * p / (h * h),
                        p / 0.001,
                        p / 0.0001,
                        p / 0.0001,
                    ]
                };
                for n in 0..6 {
                    let budget = b[n];
                    if !expected[n].is_finite()
                        || !actual[n].is_finite()
                        || !budget.is_finite()
                        || (actual[n] - expected[n]).abs() > budget
                    {
                        return Err(format!(
                            "{} field {n}: got {}, want {}, budget {budget}",
                            c.id, actual[n], expected[n]
                        ));
                    }
                }
            }
            ("unsupported_model", None, Err(Error::UnsupportedModel))
            | ("nonfinite_input", None, Err(Error::NonFiniteInput))
            | (
                "unavailable",
                None,
                Err(Error::InvalidTree | Error::NonFiniteInput | Error::NonFiniteOutput),
            ) => (),
            _ => return Err(format!("{}: outcome or candidate presence differs", c.id)),
        }
        *counts.entry(c.outcome).or_default() += 1;
    }
    let required = BTreeMap::from([
        ("value".to_owned(), 452),
        ("nonfinite_input".to_owned(), 36),
        ("unavailable".to_owned(), 12),
        ("unsupported_model".to_owned(), 2),
    ]);
    if counts != required {
        return Err("outcome inventory differs".into());
    }
    Ok(counts)
}

// Ordinary unit invocations do not pretend an absent Go oracle passed.
// The dedicated CI job MUST run this ignored test explicitly with a fresh file.
#[test]
#[ignore = "requires the fresh Go oracle and exact source SHA"]
fn fresh_go_oracle_is_required() {
    let path = std::env::var("KANZ_AMERICAN_OPTION_CORPUS").expect("fresh Go corpus required");
    let source = std::env::var("KANZ_AMERICAN_OPTION_SOURCE").expect("exact source required");
    let mut raw = Vec::new();
    File::open(path)
        .unwrap()
        .take(8 * 1024 * 1024 + 1)
        .read_to_end(&mut raw)
        .unwrap();
    let counts = verify(&raw, &source).unwrap();
    let mut altered: serde_json::Value = serde_json::from_slice(&raw).unwrap();
    altered["source_sha"] = serde_json::json!("0".repeat(40));
    assert!(verify(&serde_json::to_vec(&altered).unwrap(), &source).is_err());
    altered["source_sha"] = serde_json::json!(source);
    let first_delta = altered["cases"][0]["values_bits"][1].clone();
    altered["cases"][0]["values_bits"][1] = serde_json::json!(0);
    assert!(verify(&serde_json::to_vec(&altered).unwrap(), &source).is_err());
    altered["cases"][0]["values_bits"][1] = first_delta;
    altered["cases"][1]["id"] = altered["cases"][0]["id"].clone();
    assert!(verify(&serde_json::to_vec(&altered).unwrap(), &source).is_err());
    altered = serde_json::from_slice(&raw).unwrap();
    altered["cases"].as_array_mut().unwrap().pop();
    altered["case_count"] = serde_json::json!(501);
    assert!(verify(&serde_json::to_vec(&altered).unwrap(), &source).is_err());
    println!(
        "fresh Go American CRR source={source} cases=502 outcomes={counts:?} negative_controls=4"
    );
}

#[test]
fn missing_or_malformed_capture_refuses() {
    let source = "a".repeat(40);
    for raw in [b"".as_slice(), b"{}", b"{\"schema\":1}"] {
        assert!(verify(raw, &source).is_err());
    }
    assert!(verify(&[], "not-a-revision").is_err());
}
