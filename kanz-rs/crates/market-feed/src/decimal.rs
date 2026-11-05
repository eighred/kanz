//! Binance decimal strings only; not the broader Go big.Rat literal grammar.
use crate::proto::common::v1::Decimal;
use num_bigint::BigInt;
use num_rational::BigRational;
use num_traits::{Signed, ToPrimitive, Zero};

/// Bound allocation before constructing powers: <=128 ASCII bytes, explicit
/// exponent -64..=64, effective exponent -128..=64. No whitespace or fractions.
pub fn parse(text: &str) -> Option<Decimal> {
    if text.is_empty() || text.len() > 128 || !text.is_ascii() {
        return None;
    }
    let mut parts = text.split(['e', 'E']);
    let mantissa = parts.next()?;
    let exponent = match parts.next() {
        Some(s) => {
            let digits = s.strip_prefix(['+', '-']).unwrap_or(s);
            if digits.is_empty() || !digits.bytes().all(|b| b.is_ascii_digit()) {
                return None;
            }
            s.parse::<i32>().ok()?
        }
        None => 0,
    };
    if parts.next().is_some() || !(-64..=64).contains(&exponent) {
        return None;
    }
    let negative = mantissa.starts_with('-');
    let unsigned = mantissa.strip_prefix(['+', '-']).unwrap_or(mantissa);
    let mut digits = String::new();
    let mut dot = false;
    let mut places = 0_i32;
    for b in unsigned.bytes() {
        if b == b'.' && !dot {
            dot = true;
        } else if b.is_ascii_digit() {
            digits.push(char::from(b));
            if dot {
                places += 1;
            }
        } else {
            return None;
        }
    }
    if digits.is_empty() {
        return None;
    }
    let effective = exponent.checked_sub(places)?;
    if !(-128..=64).contains(&effective) {
        return None;
    }
    let mut numerator = BigInt::parse_bytes(digits.as_bytes(), 10)?;
    if negative {
        numerator = -numerator;
    }
    let power = BigInt::from(10).pow(effective.unsigned_abs());
    let rational = if effective >= 0 {
        BigRational::from_integer(numerator * power)
    } else {
        BigRational::new(numerator, power)
    };
    scaled(&rational)
}

fn rounded(n: &BigInt, d: &BigInt) -> BigInt {
    let mut q = n / d;
    let remainder = n % d;
    if remainder.abs() * 2 >= *d {
        q += if n.is_negative() { -1 } else { 1 };
    }
    q
}

/// Matches Go ToProtoScaled, including repeated half-away rounding on overflow.
pub(crate) fn scaled(value: &BigRational) -> Option<Decimal> {
    let mut coefficient = rounded(&(value.numer() * BigInt::from(100_000_000)), value.denom());
    let mut exponent = -8_i32;
    loop {
        if let Some(coefficient) = coefficient.to_i64() {
            return Some(Decimal {
                coefficient,
                exponent,
            });
        }
        exponent = exponent.checked_add(1)?;
        coefficient = rounded(&coefficient, &BigInt::from(10));
    }
}

pub(crate) fn rational(value: Option<&Decimal>) -> Option<BigRational> {
    let Some(value) = value else {
        return Some(BigRational::zero());
    };
    // Covers every output of the bounded text parser (including 128-digit
    // integer mantissas with a positive exponent), without unbounded powers.
    if !(-256..=256).contains(&value.exponent) {
        return None;
    }
    let power = BigInt::from(10).pow(value.exponent.unsigned_abs());
    let coefficient = BigInt::from(value.coefficient);
    Some(if value.exponent >= 0 {
        BigRational::from_integer(coefficient * power)
    } else {
        BigRational::new(coefficient, power)
    })
}

pub fn compare(a: &Decimal, b: &Decimal) -> Option<std::cmp::Ordering> {
    Some(rational(Some(a))?.cmp(&rational(Some(b))?))
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn independent_goldens() {
        for (text, coefficient, exponent) in [
            ("50000.25", 5000025000000, -8),
            ("0.00001234", 1234, -8),
            ("1000000000000", 1000000000000000000, -6),
            ("-0", 0, -8),
            ("0.0000000049", 0, -8),
            ("0.000000005", 1, -8),
            ("-0.000000005", -1, -8),
            ("92233720368.54775807", i64::MAX, -8),
            ("92233720368.54775808", 922337203685477581, -7),
            ("-92233720368.54775808", i64::MIN, -8),
            ("1e2", 10000000000, -8),
            ("+.5", 50000000, -8),
            ("1.", 100000000, -8),
            // The first divide rounds 49 to 5; the next rounds that tie up.
            ("1000000000000.00000049", 1000000000000000001, -6),
        ] {
            assert_eq!(
                parse(text),
                Some(Decimal {
                    coefficient,
                    exponent
                }),
                "{text}"
            );
        }
    }
    #[test]
    fn rejects_unsupported_and_unbounded_input() {
        for text in [
            "", " ", " 1", "1/3", "0x10", "1_000", "NaN", "Inf", ".", "1e", "1e65", "1e-65",
            "1e1e1", "1..0", "--1",
        ] {
            assert!(parse(text).is_none(), "{text}");
        }
        assert!(parse(&"1".repeat(129)).is_none());
    }
    #[test]
    fn largest_accepted_mantissa_remains_a_usable_book_key() {
        let value = parse(&format!("{}e64", "9".repeat(125))).unwrap();
        assert!(rational(Some(&value)).is_some());
        assert_eq!(compare(&value, &value), Some(std::cmp::Ordering::Equal));
    }
}
