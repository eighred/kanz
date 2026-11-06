use crate::proto::common::v1::Decimal;
use num_bigint::BigInt;
use num_rational::BigRational;
use num_traits::{Signed, ToPrimitive, Zero};

/// Absent means legacy aggregate zero, never an admission-state default.
pub fn rational(value: Option<&Decimal>) -> Option<BigRational> {
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
/// Checked existing Go ToProtoScaled conversion shared with position outputs.
pub fn decimal(value: BigRational) -> Option<Decimal> {
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
