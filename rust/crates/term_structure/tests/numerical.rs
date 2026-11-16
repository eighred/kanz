mod support;
use kanz_term_structure::{
    Compounding::*, Curve, Error, Interpolation::*, MAX_COUPONS, MAX_PILLARS,
};
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
    id: String,
    mode: u8,
    comp: u8,
    interp: u8,
    tenors: Vec<String>,
    rates: Vec<String>,
    queries: Vec<String>,
    forwards: Vec<[String; 2]>,
    values: Vec<String>,
}
fn parse(v: Vec<String>) -> Vec<f64> {
    v.into_iter().map(|s| s.parse().unwrap()).collect()
}

#[test]
fn dense_equations_and_high_precision_reference() {
    let reference: Reference = serde_json::from_str(include_str!("reference.json")).unwrap();
    assert_eq!(reference.method, "decimal_dense_par_80_110_v1");
    assert_eq!(reference.cases.len(), 10);
    for c in reference.cases {
        let ts = parse(c.tenors);
        let rs = parse(c.rates);
        let qs = parse(c.queries);
        let forwards: Vec<_> = c
            .forwards
            .into_iter()
            .map(|p| p.map(|s| s.parse().unwrap()))
            .collect();
        let curve = support::build(c.mode, c.comp, c.interp, &ts, &rs).unwrap();
        let actual = support::evaluate(&curve, &qs, &forwards, &ts).unwrap();
        support::compare(&actual, &parse(c.values), ts.len(), qs.len())
            .unwrap_or_else(|e| panic!("{}: {e}", c.id));
    }
}

#[test]
fn exact_negative_par_equations_and_tiny_rates() {
    let c = Curve::bootstrap(&[1.0, 2.0], &[-0.01, -0.01], LogLinearDiscount).unwrap();
    for (t, expected) in [(1.0, 100.0 / 99.0), (2.0, 10000.0 / 9801.0)] {
        assert!((c.discount(t).unwrap() - expected).abs() < 2e-15);
    }
    for c in [
        Curve::new_zero(&[1.0, 2.0], &[1e-20, 1e-20], Annual, LinearZero).unwrap(),
        Curve::bootstrap(&[1.0, 2.0], &[1e-20, 1e-20], LogLinearDiscount).unwrap(),
    ] {
        assert!((c.zero(2.0).unwrap() / 1e-20 - 1.0).abs() < 1e-15);
        assert!((c.par_rate(&[1.0, 2.0]).unwrap() / 1e-20 - 1.0).abs() < 1e-15);
    }
}

#[test]
fn interval_forward_is_stable_and_telescopes() {
    for interp in [LinearZero, LogLinearDiscount] {
        let c = Curve::new_zero(&[1.0, 3.0], &[0.02, 0.04], Continuous, interp).unwrap();
        let next = f64::from_bits(2.0_f64.to_bits() + 1);
        assert!((c.forward(2.0, next).unwrap() - 0.05).abs() < 5e-16);
        let all = c.forward(0.0, 5.0).unwrap() * 5.0;
        let parts = c.forward(0.0, 1.0).unwrap()
            + c.forward(1.0, 3.0).unwrap() * 2.0
            + c.forward(3.0, 5.0).unwrap() * 2.0;
        assert!((all - parts).abs() < 1e-15);
    }
}

#[test]
fn missing_invalid_and_over_budget_inputs_refuse() {
    for bad in [f64::NAN, f64::INFINITY, f64::NEG_INFINITY] {
        assert!(Curve::new_zero(&[bad], &[0.03], Continuous, LinearZero).is_err());
        assert!(Curve::bootstrap(&[1.0], &[bad], LinearZero).is_err());
    }
    assert!(Curve::new_zero(&[], &[], Continuous, LinearZero).is_err());
    assert!(Curve::new_zero(&[1.0], &[-1.0], Annual, LinearZero).is_err());
    assert!(Curve::new_zero(&[1.0], &[-2.0], SemiAnnual, LinearZero).is_err());
    assert!(Curve::bootstrap(&[1.0], &[-1.0], LinearZero).is_err());
    assert!(Curve::bootstrap(&[1e308], &[-5e-309], LinearZero).is_err());
    assert!(matches!(
        Curve::new_zero(
            &vec![1.0; MAX_PILLARS + 1],
            &vec![0.03; MAX_PILLARS + 1],
            Continuous,
            LinearZero
        ),
        Err(Error::CapacityExceeded)
    ));
    let c = Curve::new_zero(&[1.0], &[0.03], Continuous, LinearZero).unwrap();
    assert_eq!(
        c.par_rate(&vec![1.0; MAX_COUPONS + 1]),
        Err(Error::CapacityExceeded)
    );
    for out in [
        c.zero(f64::NAN),
        c.discount(f64::INFINITY),
        c.discount(1e308),
        c.forward(2.0, 1.0),
        c.par_rate(&[]),
        c.par_rate(&[1.0, 1.0]),
    ] {
        assert!(out.is_err());
    }
}
