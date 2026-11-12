//! Mixed quote compatibility; Go still owns inputs, publication and authority.
use crate::{measured, Curve, Error, Interpolation, Pillar, MAX_COUPONS, MAX_PILLARS};

const MAX_EVALUATIONS: usize = 256;

#[derive(Clone, Copy, Debug)]
pub enum QuoteKind {
    Deposit,
    Future,
    Swap,
}

#[derive(Clone, Copy, Debug)]
pub struct RateQuote {
    pub kind: QuoteKind,
    pub tenor: f64,
    pub span: f64,
    pub value: f64,
}

impl RateQuote {
    fn span(self) -> f64 {
        if self.span > 0.0 {
            self.span
        } else {
            0.25
        }
    }

    fn end(self) -> f64 {
        match self.kind {
            QuoteKind::Future => self.tenor + self.span(),
            _ => self.tenor,
        }
    }

    fn valid(self) -> bool {
        [self.tenor, self.span, self.value, self.end()]
            .into_iter()
            .all(f64::is_finite)
            && self.tenor >= 0.0
            && (matches!(self.kind, QuoteKind::Future) || self.tenor > 0.0)
            && self.span >= 0.0
            && (!matches!(self.kind, QuoteKind::Future) || self.end() > self.tenor)
            && (!matches!(self.kind, QuoteKind::Swap) || self.tenor <= MAX_COUPONS as f64)
    }
}

fn coupons(tenor: f64) -> Vec<f64> {
    let mut times = Vec::new();
    let mut t = 1.0;
    while t < tenor - 1e-9 {
        times.push(t);
        t += 1.0;
    }
    times.push(tenor);
    times
}

fn residual_fits(got: f64, want: f64) -> bool {
    got.is_finite() && want.is_finite() && (got - want).abs() <= 128.0 * f64::EPSILON * want.abs()
}

impl Curve {
    pub fn calibrate(quotes: &[RateQuote], interpolation: Interpolation) -> Result<Self, Error> {
        if quotes.len() > MAX_PILLARS {
            return Err(Error::CapacityExceeded);
        }
        if quotes.is_empty() || !quotes.iter().all(|q| q.valid()) {
            return Err(Error::InvalidInput);
        }
        let mut quotes = quotes.to_vec();
        quotes.sort_by(|a, b| a.end().total_cmp(&b.end()));
        let mut curve = Self {
            points: Vec::with_capacity(quotes.len()),
            interpolation,
        };
        let mut prev = 0.0;
        for &q in &quotes {
            let end = q.end();
            if end <= prev {
                return Err(Error::InvalidInput);
            }
            let zero = match q.kind {
                QuoteKind::Deposit | QuoteKind::Future => {
                    let period = if matches!(q.kind, QuoteKind::Future) {
                        q.span()
                    } else {
                        q.tenor
                    };
                    let u = q.value * period;
                    if !u.is_finite() || u <= -1.0 {
                        return Err(Error::NumericalFailure);
                    }
                    let accrual = libm::log1p(u);
                    if matches!(q.kind, QuoteKind::Deposit) {
                        accrual / end
                    } else {
                        let last = curve.points.last().ok_or(Error::InvalidInput)?;
                        if q.tenor <= prev {
                            (curve.zero(q.tenor)? * q.tenor + accrual) / end
                        } else {
                            // Both endpoints belong to the extended segment;
                            // solve its forward constraint with normalized weights.
                            let forward = accrual / (end - q.tenor);
                            match interpolation {
                                Interpolation::LogLinearDiscount => {
                                    (prev / end) * last.zero + ((end - prev) / end) * forward
                                }
                                Interpolation::LinearZero => {
                                    let alpha = q.tenor / (end - prev);
                                    let inverse = 1.0 / (1.0 + alpha);
                                    (alpha * inverse) * last.zero + inverse * forward
                                }
                            }
                        }
                    }
                }
                QuoteKind::Swap => solve_swap(&curve, q, MAX_EVALUATIONS)?,
            };
            measured(zero)?;
            let df = measured((-zero * end).exp())?;
            if df <= 0.0 {
                return Err(Error::NumericalFailure);
            }
            curve.points.push(Pillar { tenor: end, zero });
            prev = end;
        }
        for &q in &quotes {
            if !residual_fits(curve.quote_rate(q)?, q.value) {
                return Err(Error::NumericalFailure);
            }
        }
        Ok(curve)
    }

    /// Simple deposit/forward or annual par rate in annual rate fractions.
    pub fn quote_rate(&self, q: RateQuote) -> Result<f64, Error> {
        if !q.valid() {
            return Err(Error::InvalidQuery);
        }
        match q.kind {
            QuoteKind::Deposit => measured(libm::expm1(self.zero(q.tenor)? * q.tenor) / q.tenor),
            QuoteKind::Future => measured(
                libm::expm1(self.forward(q.tenor, q.end())? * (q.end() - q.tenor)) / q.span(),
            ),
            QuoteKind::Swap => self.par_rate(&coupons(q.tenor)),
        }
    }
}

fn solve_swap(curve: &Curve, q: RateQuote, mut budget: usize) -> Result<f64, Error> {
    let times = coupons(q.tenor);
    let mut trial = curve.clone();
    trial.points.push(Pillar {
        tenor: q.tenor,
        zero: 0.0,
    });
    let last = trial.points.len() - 1;
    let mut evaluate = |zero| -> Result<f64, Error> {
        if budget == 0 {
            return Err(Error::NumericalFailure);
        }
        budget -= 1;
        trial.points[last].zero = zero;
        measured(trial.par_rate(&times)? - q.value)
    };
    let f0 = evaluate(0.0)?;
    if q.value == 0.0 {
        return if f0 == 0.0 {
            Ok(0.0)
        } else {
            Err(Error::NumericalFailure)
        };
    }
    // Retain Go's existing zero-rate search domain. Bracketing from quote scale
    // resolves tiny rates without an absolute root-width success threshold.
    let bound: f64 = if q.value < 0.0 { -0.5 } else { 1.0 };
    let mut edge = (2.0 * q.value.abs()).min(bound.abs()).copysign(bound);
    let fe = loop {
        let f = evaluate(edge)?;
        if (bound > 0.0 && f >= 0.0) || (bound < 0.0 && f <= 0.0) {
            break f;
        }
        if edge == bound {
            return Err(Error::NumericalFailure);
        }
        edge = (2.0 * edge.abs()).min(bound.abs()).copysign(bound);
    };
    let (mut lo, mut hi) = (edge.min(0.0), edge.max(0.0));
    let (mut flo, mut fhi) = if edge < 0.0 { (fe, f0) } else { (f0, fe) };
    loop {
        let mid = lo + (hi - lo) / 2.0;
        if mid == lo || mid == hi {
            return Err(Error::NumericalFailure);
        }
        let f = evaluate(mid)?;
        if f < 0.0 {
            lo = mid;
            flo = f;
        } else {
            hi = mid;
            fhi = f;
        }
        let (best, residual) = if fhi.abs() < flo.abs() {
            (hi, fhi)
        } else {
            (lo, flo)
        };
        if hi - lo <= 64.0 * f64::EPSILON * lo.abs().max(hi.abs())
            && residual.abs() <= 64.0 * f64::EPSILON * q.value.abs()
            && best != 0.0
        {
            return Ok(best);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn exhausted_budget_never_returns_a_candidate() {
        let curve = Curve {
            points: vec![],
            interpolation: Interpolation::LinearZero,
        };
        let q = RateQuote {
            kind: QuoteKind::Swap,
            tenor: 3.5,
            span: 0.0,
            value: 0.03,
        };
        for budget in [0, 1, 5] {
            assert!(solve_swap(&curve, q, budget).is_err());
        }
        assert!(solve_swap(&curve, q, MAX_EVALUATIONS).is_ok());
    }
}
