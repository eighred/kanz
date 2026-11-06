#![forbid(unsafe_code)]
//! Diagnostic canonical fill and terminal folds. No storage, network, or execution authority.
pub mod proto {
    pub mod envelope {
        pub mod v1 {
            include!(concat!(env!("OUT_DIR"), "/envelope.v1.rs"));
        }
    }
    pub const FILE_DESCRIPTOR_SET: &[u8] =
        include_bytes!(concat!(env!("OUT_DIR"), "/oms-descriptors.bin"));
    pub use kanz_decimal_compat::common;
    pub mod command {
        pub mod v1 {
            include!(concat!(env!("OUT_DIR"), "/command.v1.rs"));
        }
    }
    pub mod market {
        pub mod v1 {
            include!(concat!(env!("OUT_DIR"), "/market.v1.rs"));
        }
    }
    pub mod order {
        pub mod v1 {
            include!(concat!(env!("OUT_DIR"), "/order.v1.rs"));
        }
    }
    pub mod oms {
        pub mod v1 {
            include!(concat!(env!("OUT_DIR"), "/oms.v1.rs"));
        }
    }
}
use num_bigint::BigInt;
use num_rational::BigRational;
use num_traits::{Signed, ToPrimitive, Zero};
use proto::order::v1::OrderStatus;
use proto::{
    common::v1::Decimal,
    oms::v1::{FillFoldRequest, FillFoldResult, FillFoldStatus},
};

fn rational(value: Option<&Decimal>) -> Option<BigRational> {
    let Some(d) = value else {
        return Some(BigRational::zero());
    };
    if !(-64..=64).contains(&d.exponent) {
        return None;
    }
    let pow = BigInt::from(10).pow(d.exponent.unsigned_abs());
    Some(if d.exponent >= 0 {
        BigRational::from_integer(BigInt::from(d.coefficient) * pow)
    } else {
        BigRational::new(BigInt::from(d.coefficient), pow)
    })
}
fn rounded(n: BigInt, d: &BigInt) -> BigInt {
    let mut q = &n / d;
    let rem = &n % d;
    if rem.abs() * 2 >= d.abs() {
        if n.is_negative() {
            q -= 1
        } else {
            q += 1
        }
    }
    q
}
// Go dec.ToProtoScaled: first round at 1e-8, then repeated half-away /10.
fn decimal(value: BigRational) -> Option<Decimal> {
    let mut q = rounded(value.numer() * BigInt::from(100_000_000), value.denom());
    let mut exponent = -8;
    loop {
        if let Some(coefficient) = q.to_i64() {
            return Some(Decimal {
                coefficient,
                exponent,
            });
        }
        q = rounded(q, &BigInt::from(10));
        exponent += 1;
        if exponent > 64 {
            return None;
        }
    }
}
fn timestamp(t: &prost_types::Timestamp) -> bool {
    (-62_135_596_800..=253_402_300_799).contains(&t.seconds)
        && (0..1_000_000_000).contains(&t.nanos)
}
fn id(s: &str) -> bool {
    !s.is_empty() && s.len() <= 256 && !s.chars().any(char::is_control)
}

pub fn evaluate_terminal(
    r: &proto::oms::v1::TerminalFoldRequest,
    version: &str,
) -> proto::oms::v1::TerminalFoldResult {
    use proto::oms::v1::{TerminalFoldOperation, TerminalFoldResult, TerminalFoldStatus};
    let mut out = TerminalFoldResult {
        capture_id: r.capture_id.clone(),
        capture_digest: r.capture_digest.clone(),
        tenant_id: r.tenant_id.clone(),
        producer_version: version.into(),
        expected_version: r.expected_version,
        operation: r.operation,
        status: TerminalFoldStatus::UnsupportedInput as i32,
        ..Default::default()
    };
    if !id(&r.capture_id)
        || !id(&r.tenant_id)
        || !id(&r.source_revision)
        || r.capture_digest.len() != 32
        || r.expected_version.is_none_or(|v| v < 0)
        || !matches!(r.path.as_str(), "cancel" | "reconcile" | "schedule")
    {
        return out;
    }
    let target = match TerminalFoldOperation::try_from(r.operation) {
        Ok(TerminalFoldOperation::Cancel) => OrderStatus::Cancelled,
        Ok(TerminalFoldOperation::Expire) => OrderStatus::Expired,
        _ => return out,
    };
    if r.path != "reconcile"
        && !((r.path == "cancel" && target == OrderStatus::Cancelled)
            || (r.path == "schedule" && target == OrderStatus::Expired))
    {
        return out;
    }
    let (Some(state), Some(now), Some(captured)) = (&r.state, &r.event_time, &r.captured_at) else {
        return out;
    };
    if !timestamp(now) || !timestamp(captured) {
        return out;
    }
    let Ok(status) = OrderStatus::try_from(state.status) else {
        return out;
    };
    if matches!(
        status,
        OrderStatus::Filled | OrderStatus::Cancelled | OrderStatus::Rejected | OrderStatus::Expired
    ) {
        out.status = TerminalFoldStatus::Rejected as i32;
        out.rejection_code = "ORDER_TERMINAL".into();
        return out;
    }
    // No arithmetic, domain conversion or normalization: Go preserves every
    // optional Decimal, even absent leaves and extreme wire exponents.
    let mut candidate = state.clone();
    candidate.status = target as i32;
    candidate.as_of = Some(*now);
    out.unfilled_quantity = state.leaves_quantity;
    out.candidate = Some(candidate);
    out.status = TerminalFoldStatus::Ok as i32;
    out
}
pub fn evaluate(r: &FillFoldRequest, version: &str) -> FillFoldResult {
    let mut out = FillFoldResult {
        capture_id: r.capture_id.clone(),
        capture_digest: r.capture_digest.clone(),
        tenant_id: r.tenant_id.clone(),
        producer_version: version.into(),
        expected_version: r.expected_version,
        status: FillFoldStatus::UnsupportedInput as i32,
        ..Default::default()
    };
    if !id(&r.capture_id)
        || !id(&r.tenant_id)
        || !id(&r.source_revision)
        || r.capture_digest.len() != 32
        || r.expected_version.is_none_or(|v| v < 1)
        || !matches!(r.path.as_str(), "live" | "reconcile" | "recovery")
    {
        return out;
    }
    let (Some(state), Some(fill), Some(now), Some(captured)) =
        (&r.state, &r.fill, &r.event_time, &r.captured_at)
    else {
        return out;
    };
    if !timestamp(now) || !timestamp(captured) {
        return out;
    }
    // Proto enums outside the declared state machine are unsupported, not new states.
    if OrderStatus::try_from(state.status).is_err() {
        return out;
    }
    if matches!(
        OrderStatus::try_from(state.status),
        Ok(OrderStatus::Filled
            | OrderStatus::Cancelled
            | OrderStatus::Rejected
            | OrderStatus::Expired)
    ) {
        out.status = FillFoldStatus::Rejected as i32;
        out.rejection_code = "ORDER_TERMINAL".into();
        return out;
    }
    if fill.quantity.as_ref().is_none_or(|q| q.coefficient <= 0) {
        out.status = FillFoldStatus::Rejected as i32;
        out.rejection_code = "INVALID_FILL".into();
        return out;
    }
    let (Some(q), Some(leaves), Some(old), Some(ordered), Some(avg), Some(price)) = (
        rational(fill.quantity.as_ref()),
        rational(state.leaves_quantity.as_ref()),
        rational(state.filled_quantity.as_ref()),
        rational(state.ordered_quantity.as_ref()),
        rational(state.average_fill_price.as_ref()),
        rational(fill.price.as_ref()),
    ) else {
        return out;
    };
    if q > leaves {
        out.status = FillFoldStatus::Rejected as i32;
        out.rejection_code = "OVERFILL".into();
        return out;
    }
    let filled = &old + &q;
    let remaining = ordered - &filled;
    if filled.is_zero() {
        out.status = FillFoldStatus::NumericalFailure as i32;
        return out;
    }
    let weighted = (old * avg + q * price) / &filled;
    let (Some(filled_wire), Some(leaves_wire), Some(avg_wire)) = (
        decimal(filled),
        decimal(remaining.clone()),
        decimal(weighted),
    ) else {
        out.status = FillFoldStatus::NumericalFailure as i32;
        return out;
    };
    let mut candidate = state.clone();
    candidate.filled_quantity = Some(filled_wire);
    candidate.leaves_quantity = Some(leaves_wire);
    candidate.average_fill_price = Some(avg_wire);
    candidate.as_of = Some(*now);
    candidate.status = if remaining.is_zero() {
        OrderStatus::Filled as i32
    } else {
        OrderStatus::PartiallyFilled as i32
    };
    out.status = FillFoldStatus::Ok as i32;
    out.candidate = Some(candidate);
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use proto::order::v1::{Fill, OrderState};
    fn d(coefficient: i64, exponent: i32) -> Decimal {
        Decimal {
            coefficient,
            exponent,
        }
    }
    fn fixture() -> FillFoldRequest {
        FillFoldRequest {
            capture_id: "capture".into(),
            capture_digest: vec![1; 32],
            tenant_id: "tenant".into(),
            source_revision: "go".into(),
            expected_version: Some(7),
            path: "live".into(),
            event_time: Some(prost_types::Timestamp {
                seconds: 100,
                nanos: 123,
            }),
            captured_at: Some(prost_types::Timestamp {
                seconds: 101,
                nanos: 0,
            }),
            state: Some(OrderState {
                order_id: "order".into(),
                portfolio_id: "portfolio".into(),
                status: OrderStatus::Routed as i32,
                ordered_quantity: Some(d(10, 0)),
                leaves_quantity: Some(d(8, 0)),
                filled_quantity: Some(d(2, 0)),
                average_fill_price: Some(d(3, 0)),
                ..Default::default()
            }),
            fill: Some(Fill {
                quantity: Some(d(1, 0)),
                price: Some(d(6, 0)),
                ..Default::default()
            }),
        }
    }
    #[test]
    fn independent_weighted_fill() {
        let r = fixture();
        let out = evaluate(&r, "stamp");
        assert_eq!(out.status, FillFoldStatus::Ok as i32);
        let c = out.candidate.unwrap();
        assert_eq!(c.filled_quantity, Some(d(300_000_000, -8)));
        assert_eq!(c.leaves_quantity, Some(d(700_000_000, -8)));
        assert_eq!(c.average_fill_price, Some(d(400_000_000, -8)));
        assert_eq!(c.portfolio_id, "portfolio");
        assert_eq!(c.as_of, r.event_time);
        assert_eq!(r.state.unwrap().filled_quantity, Some(d(2, 0)));
    }
    #[test]
    fn finite_state_refusals() {
        for terminal in [
            OrderStatus::Filled,
            OrderStatus::Cancelled,
            OrderStatus::Rejected,
            OrderStatus::Expired,
        ] {
            let mut r = fixture();
            r.state.as_mut().unwrap().status = terminal as i32;
            let out = evaluate(&r, "stamp");
            assert_eq!(out.rejection_code, "ORDER_TERMINAL");
            assert!(out.candidate.is_none())
        }
        let mut r = fixture();
        r.fill.as_mut().unwrap().quantity = Some(d(9, 0));
        assert_eq!(evaluate(&r, "stamp").rejection_code, "OVERFILL");
        r.fill.as_mut().unwrap().quantity = None;
        assert_eq!(evaluate(&r, "stamp").rejection_code, "INVALID_FILL");
    }
    #[test]
    fn half_away_and_repeated_rescale() {
        assert_eq!(
            decimal(BigRational::new(BigInt::from(1), BigInt::from(200_000_000))),
            Some(d(1, -8))
        );
        assert_eq!(
            decimal(BigRational::new(
                BigInt::from(-1),
                BigInt::from(200_000_000)
            )),
            Some(d(-1, -8))
        );
        let n = BigInt::parse_bytes(b"100000000000000000449", 10).unwrap();
        assert_eq!(
            decimal(BigRational::from_integer(n)),
            Some(d(1_000_000_000_000_000_005, 2))
        );
    }
    #[test]
    fn absent_values_are_legacy_zero_only() {
        let mut r = fixture();
        r.state.as_mut().unwrap().average_fill_price = None;
        r.fill.as_mut().unwrap().price = None;
        assert_eq!(
            evaluate(&r, "stamp").candidate.unwrap().average_fill_price,
            Some(d(0, -8))
        );
    }
    #[test]
    fn missing_version_and_exponent_refuse() {
        let mut r = fixture();
        r.expected_version = None;
        assert_eq!(
            evaluate(&r, "stamp").status,
            FillFoldStatus::UnsupportedInput as i32
        );
        r.expected_version = Some(7);
        r.fill.as_mut().unwrap().price = Some(d(1, 65));
        assert!(evaluate(&r, "stamp").candidate.is_none());
    }
    #[test]
    fn terminal_preserves_optional_quantity_and_refuses_replay() {
        use proto::oms::v1::{TerminalFoldOperation, TerminalFoldRequest, TerminalFoldStatus};
        for operation in [TerminalFoldOperation::Cancel, TerminalFoldOperation::Expire] {
            for quantity in [
                None,
                Some(d(0, 0)),
                Some(d(-1, i32::MIN)),
                Some(d(i64::MAX, i32::MAX)),
            ] {
                let fill = fixture();
                let mut r = TerminalFoldRequest {
                    capture_id: fill.capture_id,
                    capture_digest: fill.capture_digest,
                    tenant_id: fill.tenant_id,
                    source_revision: fill.source_revision,
                    expected_version: fill.expected_version,
                    state: fill.state,
                    event_time: fill.event_time,
                    captured_at: fill.captured_at,
                    operation: operation as i32,
                    path: "reconcile".into(),
                };
                r.state.as_mut().unwrap().leaves_quantity = quantity;
                let original = r.clone();
                let result = evaluate_terminal(&r, "stamp");
                assert_eq!(result.status, TerminalFoldStatus::Ok as i32);
                assert_eq!(result.unfilled_quantity, quantity);
                assert_eq!(r, original);
                let candidate = result.candidate.unwrap();
                assert_eq!(
                    candidate.filled_quantity,
                    r.state.as_ref().unwrap().filled_quantity
                );
                assert_eq!(candidate.leaves_quantity, quantity);
                assert_eq!(candidate.as_of, r.event_time);
                r.state = Some(candidate);
                let replay = evaluate_terminal(&r, "stamp");
                assert_eq!(replay.status, TerminalFoldStatus::Rejected as i32);
                assert!(replay.candidate.is_none() && replay.unfilled_quantity.is_none());
            }
        }
    }
}
