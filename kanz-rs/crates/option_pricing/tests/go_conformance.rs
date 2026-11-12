use kanz_option_pricing::{evaluate, Error, Exercise, Input, Right};
mod support;
use std::{collections::BTreeMap, fs::File, io::Read};

fn verify(raw: &[u8], expected_source: &str) -> Result<BTreeMap<String, usize>, String> {
    let corpus = support::capture(raw, expected_source, "go_bsm_legacy_v1", 8162)?;
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
                // Fixed before comparison: cancellation-aware price bound in
                // discounted input units; other Greeks use their natural units.
                // These are binary64 analytic comparisons, never money rounding.
                let price_scale = (input.spot * (-input.yield_rate * input.years).exp()).abs()
                    + (input.strike * (-input.rate * input.years).exp()).abs();
                for n in 0..6 {
                    let budget = if n == 0 {
                        64.0 * f64::EPSILON * price_scale.max(1.0)
                            + 16.0 * f64::EPSILON * expected[n].abs()
                    } else {
                        128.0 * f64::EPSILON * (1.0 + expected[n].abs())
                    };
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
            | ("nonfinite_output", None, Err(Error::NonFiniteOutput)) => (),
            _ => return Err(format!("{}: outcome or candidate presence differs", c.id)),
        }
        *counts.entry(c.outcome).or_default() += 1;
    }
    let required = BTreeMap::from([
        ("value".to_owned(), 8120),
        ("nonfinite_input".to_owned(), 36),
        ("nonfinite_output".to_owned(), 4),
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
    let path = std::env::var("KANZ_OPTION_PRICING_CORPUS").expect("fresh Go corpus required");
    let source = std::env::var("KANZ_OPTION_PRICING_SOURCE").expect("exact source required");
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
    altered["case_count"] = serde_json::json!(8161);
    assert!(verify(&serde_json::to_vec(&altered).unwrap(), &source).is_err());
    println!("fresh Go BSM source={source} cases=8162 outcomes={counts:?} negative_controls=4");
}

#[test]
fn missing_or_malformed_capture_refuses() {
    let source = "a".repeat(40);
    for raw in [b"".as_slice(), b"{}", b"{\"schema\":1}"] {
        assert!(verify(raw, &source).is_err());
    }
    assert!(verify(&[], "not-a-revision").is_err());
}
