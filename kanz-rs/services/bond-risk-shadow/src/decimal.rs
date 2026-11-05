use kanz_decimal_compat::common::v1::Decimal;
use num_bigint::BigInt;
use num_rational::BigRational;
use num_traits::ToPrimitive;

/// Exact rational then correctly rounded once, matching Go big.Rat.Float64.
/// num-rational 0.4.2 ratio_to_f64 uses quotient/remainder round-to-even.
pub fn to_f64(value: &Decimal) -> Option<f64> {
    if !kanz_decimal_compat::in_domain(Some(value)) {
        return None;
    }
    let power = BigInt::from(10).pow(value.exponent.unsigned_abs());
    let n = BigInt::from(value.coefficient);
    let rational = if value.exponent >= 0 {
        BigRational::from_integer(n * power)
    } else {
        BigRational::new(n, power)
    };
    rational.to_f64().filter(|v| v.is_finite())
}

/// Seventeen significant decimal digits round-trip a finite binary64. Money
/// remains canonical Decimal on the wire; refuse rather than scale-round it.
pub fn from_f64(value: f64) -> Option<Decimal> {
    if !value.is_finite() {
        return None;
    }
    if value == 0.0 {
        return Some(Decimal {
            coefficient: 0,
            exponent: 0,
        });
    }
    let text = format!("{value:.16e}");
    let (mantissa, exponent) = text.split_once('e')?;
    let mut coefficient = mantissa.replace('.', "").parse::<i64>().ok()?;
    let mut exponent = exponent.parse::<i32>().ok()?.checked_sub(16)?;
    while coefficient % 10 == 0 {
        coefficient /= 10;
        exponent += 1;
    }
    let result = Decimal {
        coefficient,
        exponent,
    };
    (to_f64(&result)?.to_bits() == value.to_bits()).then_some(result)
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn single_rounding_and_checked_roundtrip() {
        for (c, e, want) in [
            (100, -6, 0.0001_f64),
            (123456789, -8, 1.23456789),
            (7, -25, 7e-25),
            (i64::MIN, 0, -9223372036854775808.0),
        ] {
            assert_eq!(
                to_f64(&Decimal {
                    coefficient: c,
                    exponent: e
                })
                .unwrap()
                .to_bits(),
                want.to_bits()
            );
            assert_eq!(
                to_f64(&from_f64(want).unwrap()).unwrap().to_bits(),
                want.to_bits()
            );
        }
        assert!(from_f64(f64::NAN).is_none());
        assert!(from_f64(f64::INFINITY).is_none());
        assert!(to_f64(&Decimal {
            coefficient: 1,
            exponent: 65
        })
        .is_none());
        assert!(from_f64(1e100).is_none());
    }
}
