#![forbid(unsafe_code)]
//! Pure, bounded shadow analytics. No network, stores or admission authority.
pub mod decimal;
pub mod proto {
    pub use kanz_decimal_compat::common;
    pub mod reference {
        pub mod v1 {
            include!(concat!(env!("OUT_DIR"), "/reference.v1.rs"));
        }
    }
    pub mod domain {
        pub mod v1 {
            include!(concat!(env!("OUT_DIR"), "/domain.v1.rs"));
        }
    }
    pub mod quant {
        pub mod v1 {
            include!(concat!(env!("OUT_DIR"), "/quant.v1.rs"));
        }
    }
}
use prost_types::Timestamp;
use proto::quant::v1::{BondRiskRequest, BondRiskResult, BondRiskStatus, BondRiskValues};
const MAX_HORIZON_NS: i128 = 36_600 * 86_400 * 1_000_000_000;
type Failure = BondRiskStatus;

fn stamp(value: Option<&Timestamp>) -> Result<i128, Failure> {
    let t = value.ok_or(Failure::InvalidInput)?;
    if !(-62_135_596_800..=253_402_300_799).contains(&t.seconds)
        || !(0..1_000_000_000).contains(&t.nanos)
    {
        return Err(Failure::InvalidInput);
    }
    Ok(i128::from(t.seconds) * 1_000_000_000 + i128::from(t.nanos))
}
fn id(value: &str) -> bool {
    !value.is_empty() && value.len() <= 256 && !value.chars().any(char::is_control)
}

pub fn evaluate(request: &BondRiskRequest, version: &str) -> BondRiskResult {
    let (status, values) = match calculate(request) {
        Ok(values) => (Failure::Ok, Some(values)),
        Err(status) => (status, None),
    };
    BondRiskResult {
        capture_id: request.capture_id.clone(),
        capture_digest: request.capture_digest.clone(),
        status: status as i32,
        values,
        producer_version: version.to_owned(),
        tenant_id: request.tenant_id.clone(),
    }
}

fn calculate(r: &BondRiskRequest) -> Result<BondRiskValues, Failure> {
    if !id(&r.capture_id)
        || !id(&r.instrument_id)
        || !id(&r.tenant_id)
        || !id(&r.source_revision)
        || r.capture_digest.len() != 32
        || r.payments.len() > 1201
        || r.curve_points.is_empty()
        || r.curve_points.len() > 256
    {
        return Err(Failure::InvalidInput);
    }
    if r.schedule_convention != "go_bond_schedule_v1" || !matches!(r.interpolation, 1 | 2) {
        return Err(Failure::UnsupportedConvention);
    }
    let asof = stamp(r.as_of.as_ref())?;
    let captured = stamp(r.captured_at.as_ref())?;
    if captured < asof {
        return Err(Failure::InvalidInput);
    }
    if let Some(t) = r.curve_as_of.as_ref() {
        if stamp(Some(t))? > asof {
            return Err(Failure::InvalidInput);
        }
    }
    if r.curve_id.as_ref().is_some_and(|v| !id(v)) {
        return Err(Failure::InvalidInput);
    }
    let terms = r.terms.as_ref().ok_or(Failure::InvalidInput)?;
    let face = decimal::to_f64(terms.face_value.as_ref().ok_or(Failure::InvalidInput)?)
        .ok_or(Failure::InvalidInput)?;
    let coupon = decimal::to_f64(terms.coupon_rate.as_ref().ok_or(Failure::InvalidInput)?)
        .ok_or(Failure::InvalidInput)?;
    let issue = stamp(terms.issue_date.as_ref())?;
    let maturity = stamp(terms.maturity_date.as_ref())?;
    if face <= 0.0
        || coupon < 0.0
        || maturity <= issue
        || maturity - issue > MAX_HORIZON_NS
        || (maturity - asof).abs() > MAX_HORIZON_NS
        || terms.currency_code.len() != 3
        || !terms.currency_code.bytes().all(|v| v.is_ascii_uppercase())
    {
        return Err(Failure::InvalidInput);
    }
    if !matches!(terms.day_count, 1..=4) {
        return Err(Failure::UnsupportedConvention);
    }
    let frequency = match terms.coupon_frequency {
        1 => 1.0,
        2 => 2.0,
        3 => 4.0,
        4 => 12.0,
        _ if coupon == 0.0 => 1.0,
        _ => return Err(Failure::UnsupportedConvention),
    };
    let mut previous = 0.0;
    for point in &r.curve_points {
        let z = point.continuous_zero.ok_or(Failure::InvalidInput)?;
        if !point.tenor_years.is_finite()
            || point.tenor_years <= previous
            || point.tenor_years > 100.0
            || !z.is_finite()
        {
            return Err(Failure::InvalidInput);
        }
        previous = point.tenor_years;
    }
    if asof >= maturity {
        if !r.payments.is_empty() {
            return Err(Failure::InvalidInput);
        }
        return values(0.0, 0.0, 0.0, 0.0);
    }
    if r.payments.is_empty() || (coupon == 0.0 && r.payments.len() != 1) {
        return Err(Failure::InvalidInput);
    }
    let coupon_amount = if coupon == 0.0 {
        0.0
    } else {
        face * coupon / frequency
    };
    if !coupon_amount.is_finite() {
        return Err(Failure::NumericalFailure);
    }
    let mut prior = asof;
    let mut prices = [0.0; 3];
    for (index, payment) in r.payments.iter().enumerate() {
        let date = stamp(payment.payment_date.as_ref())?;
        if date <= prior
            || date > maturity
            || date <= issue
            || payment.coupon != (coupon > 0.0)
            || payment.redemption != (index + 1 == r.payments.len())
            || (payment.redemption && date != maturity)
        {
            return Err(Failure::InvalidInput);
        }
        prior = date;
        // Same division order as Go time.Duration.Hours()/24/365.
        let ns = date - asof;
        let hours =
            (ns / 3_600_000_000_000) as f64 + (ns % 3_600_000_000_000) as f64 / 3_600_000_000_000.0;
        let t = hours / 24.0 / 365.0;
        let amount = coupon_amount + if payment.redemption { face } else { 0.0 };
        for (slot, bump) in [0.0, 1e-4, -1e-4].iter().enumerate() {
            let z = zero(r, t, *bump)?;
            let df = (-z * t).exp();
            if !df.is_finite() || df <= 0.0 {
                return Err(Failure::NumericalFailure);
            }
            prices[slot] += amount * df;
        }
    }
    let [base, up, down] = prices;
    if !prices.iter().all(|v| v.is_finite()) || base <= 0.0 {
        return Err(Failure::NumericalFailure);
    }
    values(
        base,
        (up - down).abs() / 2.0,
        -(up - down) / (2.0 * base * 1e-4),
        (up + down - 2.0 * base) / (base * 1e-4 * 1e-4),
    )
}

fn zero(r: &BondRiskRequest, t: f64, bump: f64) -> Result<f64, Failure> {
    let points = &r.curve_points;
    let at = points.partition_point(|p| p.tenor_years < t);
    let value = if at == 0 {
        points[0].continuous_zero.unwrap() + bump
    } else if at == points.len() {
        points[at - 1].continuous_zero.unwrap() + bump
    } else {
        let a = &points[at - 1];
        let b = &points[at];
        let za = a.continuous_zero.unwrap() + bump;
        let zb = b.continuous_zero.unwrap() + bump;
        let w = (t - a.tenor_years) / (b.tenor_years - a.tenor_years);
        if r.interpolation == 2 {
            let lo = -za * a.tenor_years;
            let hi = -zb * b.tenor_years;
            -(lo + w * (hi - lo)) / t
        } else {
            za + w * (zb - za)
        }
    };
    value
        .is_finite()
        .then_some(value)
        .ok_or(Failure::NumericalFailure)
}
fn values(price: f64, dv01: f64, duration: f64, convexity: f64) -> Result<BondRiskValues, Failure> {
    if !duration.is_finite() || !convexity.is_finite() {
        return Err(Failure::NumericalFailure);
    }
    Ok(BondRiskValues {
        dirty_price: Some(decimal::from_f64(price).ok_or(Failure::NumericalFailure)?),
        dv01: Some(decimal::from_f64(dv01).ok_or(Failure::NumericalFailure)?),
        effective_duration: Some(duration),
        convexity: Some(convexity),
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use proto::{
        common::v1::Decimal,
        quant::v1::{BondPayment, ResolvedCurvePoint},
        reference::v1::BondTerms,
    };
    fn ts(seconds: i64) -> Option<Timestamp> {
        Some(Timestamp { seconds, nanos: 0 })
    }
    fn request(coupon: i64) -> BondRiskRequest {
        BondRiskRequest {
            capture_id: "capture".into(),
            capture_digest: vec![7; 32],
            instrument_id: "bond".into(),
            tenant_id: "tenant-a".into(),
            source_revision: "go-sha".into(),
            as_of: ts(1_700_000_000),
            captured_at: ts(1_700_000_001),
            terms: Some(BondTerms {
                face_value: Some(Decimal {
                    coefficient: 1000,
                    exponent: 0,
                }),
                coupon_rate: Some(Decimal {
                    coefficient: coupon,
                    exponent: -2,
                }),
                coupon_frequency: 1,
                issue_date: ts(1_700_000_000),
                maturity_date: ts(1_763_072_000),
                day_count: 2,
                currency_code: "USD".into(),
                issuer_id: String::new(),
            }),
            payments: if coupon == 0 {
                vec![BondPayment {
                    payment_date: ts(1_763_072_000),
                    coupon: false,
                    redemption: true,
                }]
            } else {
                vec![
                    BondPayment {
                        payment_date: ts(1_731_536_000),
                        coupon: true,
                        redemption: false,
                    },
                    BondPayment {
                        payment_date: ts(1_763_072_000),
                        coupon: true,
                        redemption: true,
                    },
                ]
            },
            curve_points: vec![
                ResolvedCurvePoint {
                    tenor_years: 1.0,
                    continuous_zero: Some(0.05),
                },
                ResolvedCurvePoint {
                    tenor_years: 2.0,
                    continuous_zero: Some(0.05),
                },
            ],
            interpolation: 1,
            schedule_convention: "go_bond_schedule_v1".into(),
            ..Default::default()
        }
    }
    #[test]
    fn independent_zero_and_coupon_payment_moments() {
        let h = 1e-4_f64;
        for coupon in [0, 5] {
            let r = request(coupon);
            let result = evaluate(&r, "rust-sha");
            assert_eq!(result.status, Failure::Ok as i32);
            let v = result.values.unwrap();
            let flows = if coupon == 0 {
                vec![(2.0, 1000.0)]
            } else {
                vec![(1.0, 50.0), (2.0, 1050.0)]
            };
            let mut price = 0.0;
            let mut dv = 0.0;
            let mut convex = 0.0;
            for (t, amount) in flows {
                let pv = amount * (-0.05_f64 * t).exp();
                price += pv;
                dv += pv * (h * t).sinh();
                convex += pv * 4.0 * (h * t / 2.0).sinh().powi(2) / (h * h);
            }
            assert!(
                (decimal::to_f64(v.dirty_price.as_ref().unwrap()).unwrap() - price).abs() < 1e-9
            );
            assert!((decimal::to_f64(v.dv01.as_ref().unwrap()).unwrap() - dv).abs() < 1e-9);
            assert!((v.effective_duration.unwrap() - dv / (price * h)).abs() < 1e-7);
            assert!((v.convexity.unwrap() - convex / price).abs() < 2e-5);
        }
    }
    #[test]
    fn invalid_is_not_measured_zero() {
        for mutation in 0..8 {
            let mut r = request(0);
            match mutation {
                0 => r.terms.as_mut().unwrap().face_value = None,
                1 => r.curve_points[0].continuous_zero = None,
                2 => r.curve_points[0].continuous_zero = Some(f64::NAN),
                3 => r.curve_points[1].tenor_years = 1.0,
                4 => r.payments.clear(),
                5 => r.payments[0].redemption = false,
                6 => {
                    r.terms
                        .as_mut()
                        .unwrap()
                        .face_value
                        .as_mut()
                        .unwrap()
                        .exponent = i32::MAX
                }
                _ => r.tenant_id.clear(),
            }
            let out = evaluate(&r, "test");
            assert_eq!(out.status, Failure::InvalidInput as i32);
            assert!(out.values.is_none());
        }
        let mut r = request(0);
        r.as_of = r.terms.as_ref().unwrap().maturity_date;
        r.captured_at = r.as_of;
        r.payments.clear();
        let out = evaluate(&r, "test");
        assert_eq!(out.status, Failure::Ok as i32);
        assert_eq!(out.values.unwrap().dirty_price.unwrap().coefficient, 0);
    }
    #[test]
    fn negative_rates_supported_and_numerical_failure_refuses() {
        let mut r = request(0);
        for p in &mut r.curve_points {
            p.continuous_zero = Some(-0.05);
        }
        assert!(
            decimal::to_f64(
                evaluate(&r, "test")
                    .values
                    .unwrap()
                    .dirty_price
                    .as_ref()
                    .unwrap()
            )
            .unwrap()
                > 1000.0
        );
        for p in &mut r.curve_points {
            p.continuous_zero = Some(-1000.0);
        }
        let out = evaluate(&r, "test");
        assert_eq!(out.status, Failure::NumericalFailure as i32);
        assert!(out.values.is_none());
        r.interpolation = 0;
        assert_eq!(
            evaluate(&r, "test").status,
            Failure::UnsupportedConvention as i32
        );
    }
}
