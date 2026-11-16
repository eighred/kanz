use kanz_option_pricing::{american::evaluate, Error, Exercise, Input, Right};
use serde::Deserialize;

fn input(right: Right) -> Input {
    Input {
        right,
        exercise: Exercise::American,
        spot: 100.0,
        strike: 100.0,
        years: 1.0,
        rate: 0.05,
        yield_rate: 0.0,
        volatility: 0.2,
    }
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Reference {
    method: String,
    steps: usize,
    cases: Vec<Case>,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Case {
    id: String,
    right: String,
    inputs: [String; 6],
    values: [String; 6],
}

// Fixed before comparison. 8192 eps of input scale covers the 200-level
// binary64 power/induction path; derivative budgets propagate that price
// budget through the actual fixed bump denominators, including cancellation.
// This is a compatibility budget, not an economic error or convergence bound.
fn budgets(i: Input) -> [f64; 6] {
    let p = 8192.0 * f64::EPSILON * i.spot.abs().max(i.strike.abs()).max(1.0);
    let h = i.spot * 0.01;
    [
        p,
        p / h,
        4.0 * p / (h * h),
        p / 0.001,
        p / 0.0001,
        p / 0.0001,
    ]
}

#[test]
fn all_fields_match_high_precision_stopping_reference() {
    let r: Reference = serde_json::from_str(include_str!("american_reference.json")).unwrap();
    assert_eq!(r.method, "decimal_snell_80_110_v1");
    assert_eq!(r.steps, 200);
    assert_eq!(r.cases.len(), 8);
    for c in r.cases {
        let [spot, strike, years, rate, yield_rate, volatility] =
            c.inputs.map(|v| v.parse::<f64>().unwrap());
        let right = match c.right.as_str() {
            "call" => Right::Call,
            "put" => Right::Put,
            _ => panic!("unknown right"),
        };
        let i = Input {
            spot,
            strike,
            years,
            rate,
            yield_rate,
            volatility,
            ..input(right)
        };
        let got = evaluate(i).unwrap().values();
        let expected = c.values.map(|v| v.parse::<f64>().unwrap());
        for n in 0..6 {
            assert!(
                got[n].is_finite()
                    && expected[n].is_finite()
                    && (got[n] - expected[n]).abs() <= budgets(i)[n],
                "{} field {n}: {} vs {} budget {}",
                c.id,
                got[n],
                expected[n],
                budgets(i)[n]
            );
        }
    }
}

#[test]
fn immediate_exercise_is_affine_and_has_zero_time_risk() {
    // Independent financial identity: exercise now pays K-S, with delta=-1;
    // all other sensitivities are zero while every bump stays in that region.
    let out = evaluate(Input {
        spot: 60.0,
        ..input(Right::Put)
    })
    .unwrap();
    for (got, expected) in out
        .values()
        .into_iter()
        .zip([40.0, -1.0, 0.0, 0.0, 0.0, 0.0])
    {
        assert!((got - expected).abs() < 1e-10);
    }
}

#[test]
fn impossible_tree_or_single_failed_bump_refuses_whole_candidate() {
    assert_eq!(
        evaluate(Input {
            spot: 60.0,
            rate: 1.0,
            volatility: 0.001,
            ..input(Right::Put)
        }),
        Err(Error::InvalidTree)
    );
    assert_eq!(
        evaluate(Input {
            rate: -0.02,
            volatility: 0.001415,
            ..input(Right::Put)
        }),
        Err(Error::InvalidTree)
    );
    assert_eq!(
        evaluate(Input {
            volatility: 1e-320,
            years: 1e-320,
            ..input(Right::Put)
        }),
        Err(Error::InvalidTree)
    );
    assert_eq!(
        evaluate(Input {
            exercise: Exercise::European,
            ..input(Right::Put)
        }),
        Err(Error::UnsupportedModel)
    );
    for field in 0..6 {
        for bad in [f64::NAN, f64::INFINITY, f64::NEG_INFINITY] {
            let mut i = input(Right::Call);
            match field {
                0 => i.spot = bad,
                1 => i.strike = bad,
                2 => i.years = bad,
                3 => i.rate = bad,
                4 => i.yield_rate = bad,
                _ => i.volatility = bad,
            }
            assert_eq!(evaluate(i), Err(Error::NonFiniteInput));
        }
    }
}
