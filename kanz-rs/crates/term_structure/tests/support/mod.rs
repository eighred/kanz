use kanz_term_structure::{Compounding, Curve, Error, Interpolation};

pub fn build(mode: u8, comp: u8, interp: u8, ts: &[f64], rs: &[f64]) -> Result<Curve, Error> {
    let interpolation = match interp {
        0 => Interpolation::LinearZero,
        1 => Interpolation::LogLinearDiscount,
        _ => return Err(Error::InvalidInput),
    };
    match mode {
        0 => Curve::new_zero(
            ts,
            rs,
            match comp {
                0 => Compounding::Continuous,
                1 => Compounding::Annual,
                2 => Compounding::SemiAnnual,
                _ => return Err(Error::InvalidInput),
            },
            interpolation,
        ),
        1 => Curve::bootstrap(ts, rs, interpolation),
        _ => Err(Error::InvalidInput),
    }
}

pub fn evaluate(
    c: &Curve,
    queries: &[f64],
    forwards: &[[f64; 2]],
    coupons: &[f64],
) -> Result<Vec<f64>, Error> {
    let mut out = c.tenors();
    out.extend(c.zeros());
    for &t in queries {
        out.extend([c.zero(t)?, c.discount(t)?, c.zero_with_bump(t, 1e-4)?]);
    }
    for f in forwards {
        out.push(c.forward(f[0], f[1])?);
    }
    out.push(c.par_rate(coupons)?);
    Ok(out)
}

pub fn compare(
    actual: &[f64],
    expected: &[f64],
    pillars: usize,
    queries: usize,
) -> Result<(), String> {
    if actual.len() != expected.len() || actual.len() < 2 * pillars + 3 * queries + 1 {
        return Err("result inventory differs".into());
    }
    let scale = expected[pillars..2 * pillars]
        .iter()
        .fold(0.0_f64, |a, b| a.max(b.abs()));
    for (n, (&a, &b)) in actual.iter().zip(expected).enumerate() {
        // Fixed before comparison: exact tenors; a relative exp/log budget for
        // discount factors; rate units use pillar scale plus the known bump.
        // n-dependence covers accumulation, not economic error or rounding.
        let discount =
            n >= 2 * pillars && n < 2 * pillars + 3 * queries && (n - 2 * pillars) % 3 == 1;
        let bumped =
            n >= 2 * pillars && n < 2 * pillars + 3 * queries && (n - 2 * pillars) % 3 == 2;
        let units = if discount {
            b.abs()
        } else {
            scale.max(b.abs()).max(if bumped { 1e-4 } else { 0.0 })
        };
        let budget = 1024.0 * f64::EPSILON * (pillars as f64) * units + 64.0 * f64::MIN_POSITIVE;
        if !a.is_finite()
            || !b.is_finite()
            || !budget.is_finite()
            || (n < pillars && a.to_bits() != b.to_bits())
            || (n >= pillars && (a - b).abs() > budget)
        {
            return Err(format!("field {n}: {a} vs {b}, budget {budget}"));
        }
    }
    Ok(())
}
