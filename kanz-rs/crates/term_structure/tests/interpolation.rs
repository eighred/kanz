use kanz_term_structure::{Compounding, Curve, Interpolation, QuoteKind, RateQuote};
use serde::Deserialize;

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Reference {
    method: String,
    cases: Vec<Case>,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Case {
    interp: u8,
    tenors: Vec<u64>,
    rates: Vec<u64>,
    bump: u64,
    queries: Vec<u64>,
    values: Vec<u64>,
}

#[test]
fn rational_knots_and_adjacent_queries_preserve_small_rates() {
    let data: Reference =
        serde_json::from_str(include_str!("interpolation_reference.json")).unwrap();
    assert_eq!(data.method, "rational_barycentric_binary64_v1");
    assert_eq!(data.cases.len(), 12);
    let mut count = 0;
    for row in data.cases {
        let tenors: Vec<_> = row.tenors.into_iter().map(f64::from_bits).collect();
        let rates: Vec<_> = row.rates.into_iter().map(f64::from_bits).collect();
        let interp = match row.interp {
            0 => Interpolation::LinearZero,
            1 => Interpolation::LogLinearDiscount,
            _ => panic!("unknown interpolation"),
        };
        let curve = Curve::new_zero(&tenors, &rates, Compounding::Continuous, interp).unwrap();
        let bump = f64::from_bits(row.bump);
        assert_eq!(
            curve.zero_with_bump(2.0, bump).unwrap().to_bits(),
            (rates[1] + bump).to_bits()
        );
        assert_eq!(row.queries.len(), 7);
        assert_eq!(row.values.len(), 7);
        for (at, want) in row.queries.into_iter().zip(row.values) {
            let want = f64::from_bits(want);
            let at = f64::from_bits(at);
            let got = curve.zero_with_bump(at, bump).unwrap();
            assert!(
                (got - want).abs() <= 16.0 * f64::EPSILON * want.abs(),
                "{interp:?} bump={bump} at={at}: {got} vs {want}"
            );
            count += 1;
        }
    }
    assert_eq!(count, 84);
    // Known non-dyadic knots must not round-trip through z*t/t, either.
    for interp in [Interpolation::LinearZero, Interpolation::LogLinearDiscount] {
        for (at, rate, bump) in [(1.3, 1e-100, 0.0), (2.7, 0.0, 1e-4)] {
            let curve = Curve::new_zero(
                &[0.25, at, 5.0],
                &[0.03, rate, 0.04],
                Compounding::Continuous,
                interp,
            )
            .unwrap();
            assert_eq!(
                curve.zero_with_bump(at, bump).unwrap().to_bits(),
                (rate + bump).to_bits()
            );
        }
    }
}

#[test]
fn mixed_small_interior_deposit_reprices_without_erasure() {
    for interp in [Interpolation::LinearZero, Interpolation::LogLinearDiscount] {
        for tiny in [-1e-20_f64, 0.0, 1e-20] {
            let quotes: Vec<_> = [(1.0, 0.03), (2.0, tiny), (3.0, 0.04)]
                .into_iter()
                .map(|(tenor, value)| RateQuote {
                    kind: QuoteKind::Deposit,
                    tenor,
                    span: 0.0,
                    value,
                })
                .collect();
            let curve = Curve::calibrate(&quotes, interp).unwrap();
            for q in quotes {
                let got = curve.quote_rate(q).unwrap();
                assert!((got - q.value).abs() <= 128.0 * f64::EPSILON * q.value.abs());
            }
        }
    }
}
