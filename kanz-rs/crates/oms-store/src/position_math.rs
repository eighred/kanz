//! Exact weighted-average-cost model shared by this position owner and its projections.
use crate::Error;
use kanz_order_core::{
    decimal,
    proto::{common::v1::Money, domain::v1::PositionState, order::v1::Fill},
    rational,
};
use num_bigint::BigInt;
use num_rational::BigRational as Rat;
use num_traits::Zero;
use std::str::FromStr;
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub(crate) struct Lot(kanz_order_core::cost_basis::Lot);
impl std::ops::Deref for Lot {
    type Target = kanz_order_core::cost_basis::Lot;
    fn deref(&self) -> &Self::Target {
        &self.0
    }
}
pub(crate) fn bound(v: &Rat) -> Result<(), Error> {
    kanz_order_core::cost_basis::bound(v).map_err(math_error)
}
fn math_error(value: kanz_order_core::cost_basis::Error) -> Error {
    match value {
        kanz_order_core::cost_basis::Error::Capacity => Error::Capacity,
        kanz_order_core::cost_basis::Error::InvalidRational => Error::InvalidInput,
    }
}
pub(crate) fn parse(s: &str) -> Result<Rat, Error> {
    if s.len() > 4096 {
        return Err(Error::Capacity);
    }
    // Persisted Go RatString is decimal integer or numerator/denominator, not arbitrary big.Rat input grammar.
    let (n, d) = s.split_once('/').unwrap_or((s, "1"));
    let integer = |x: &str| -> Result<BigInt, Error> {
        let digits = x.strip_prefix('-').unwrap_or(x);
        if digits.is_empty() || !digits.bytes().all(|c| c.is_ascii_digit()) {
            return Err(Error::InvalidInput);
        }
        BigInt::from_str(x).map_err(|_| Error::InvalidInput)
    };
    let n = integer(n)?;
    let d = integer(d)?;
    if d <= BigInt::zero() {
        return Err(Error::InvalidInput);
    }
    let v = Rat::new(n, d);
    bound(&v)?;
    Ok(v)
}
pub(crate) fn text(v: &Rat) -> String {
    if v.is_integer() {
        v.numer().to_string()
    } else {
        format!("{}/{}", v.numer(), v.denom())
    }
}
impl Lot {
    pub fn from_text(q: &str, a: &str, r: &str) -> Result<Self, Error> {
        Ok(Self(kanz_order_core::cost_basis::Lot {
            qty: parse(q)?,
            avg: parse(a)?,
            realized: parse(r)?,
        }))
    }
    pub fn fold(&mut self, fill: &Fill) -> Result<(), Error> {
        let mut q = rational(fill.quantity.as_ref()).ok_or(Error::InvalidInput)?;
        if fill.side == 2 {
            q = -q;
        }
        let p = rational(fill.price.as_ref()).ok_or(Error::InvalidInput)?;
        self.0.fold(&q, &p).map_err(math_error)
    }
}
pub(crate) fn aggregate(lots: &[Lot]) -> Result<Lot, Error> {
    kanz_order_core::cost_basis::aggregate(lots.iter().map(|l| &l.0))
        .map(Lot)
        .map_err(math_error)
}
pub(crate) fn money(v: Rat, ccy: &str) -> Result<Money, Error> {
    bound(&v)?;
    Ok(Money {
        amount: Some(decimal(v).ok_or(Error::Capacity)?),
        currency_code: ccy.to_owned(),
    })
}
pub(crate) fn state(
    l: &Lot,
    portfolio: &str,
    venue: &str,
    instrument: &str,
    price: &Rat,
    at: prost_types::Timestamp,
    ccy: &str,
) -> Result<PositionState, Error> {
    Ok(PositionState {
        portfolio_id: portfolio.into(),
        venue: venue.into(),
        instrument_id: instrument.into(),
        quantity: Some(decimal(l.qty.clone()).ok_or(Error::Capacity)?),
        average_price: Some(decimal(l.avg.clone()).ok_or(Error::Capacity)?),
        market_value: Some(money(price * &l.qty, ccy)?),
        realized_pnl: Some(money(l.realized.clone(), ccy)?),
        unrealized_pnl: Some(money((price - &l.avg) * &l.qty, ccy)?),
        as_of: Some(at),
    })
}
#[cfg(test)]
mod tests {
    use super::*;
    use kanz_order_core::proto::common::v1::Decimal;
    fn fill(q: i64, p: i64, side: i32) -> Fill {
        Fill {
            quantity: Some(Decimal {
                coefficient: q,
                exponent: 0,
            }),
            price: Some(Decimal {
                coefficient: p,
                exponent: 0,
            }),
            side,
            ..Default::default()
        }
    }
    #[test]
    fn independent_buy_reduce_reverse_goldens() {
        let mut l = Lot::default();
        l.fold(&fill(2, 10, 1)).unwrap();
        l.fold(&fill(1, 13, 1)).unwrap();
        assert_eq!(
            (text(&l.qty), text(&l.avg), text(&l.realized)),
            ("3".into(), "11".into(), "0".into())
        );
        l.fold(&fill(1, 15, 2)).unwrap();
        assert_eq!(text(&l.realized), "4");
        l.fold(&fill(3, 8, 2)).unwrap();
        assert_eq!(
            (text(&l.qty), text(&l.avg), text(&l.realized)),
            ("-1".into(), "8".into(), "-2".into())
        );
    }
    #[test]
    fn hedge_keeps_absolute_cost_weight() {
        let a = Lot::from_text("10", "20", "3").unwrap();
        let b = Lot::from_text("-10", "30", "4").unwrap();
        let l = aggregate(&[a, b]).unwrap();
        assert_eq!(
            (text(&l.qty), text(&l.avg), text(&l.realized)),
            ("0".into(), "25".into(), "7".into())
        );
    }
    #[test]
    fn corrupt_or_unbounded_rational_refuses() {
        for x in ["NaN", "1/0", "1/-2", "1/2/3", ""] {
            assert!(parse(x).is_err())
        }
        assert!(parse(&"9".repeat(4097)).is_err());
    }
}
