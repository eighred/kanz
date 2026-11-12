#![forbid(unsafe_code)]
//! Bounded scalar zero/par term structures. No market, record or runtime owner.

pub const MAX_PILLARS: usize = 256;
pub const MAX_COUPONS: usize = 4096;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Interpolation {
    LinearZero,
    LogLinearDiscount,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Compounding {
    Continuous,
    Annual,
    SemiAnnual,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Error {
    InvalidInput,
    InvalidQuery,
    NumericalFailure,
    CapacityExceeded,
}

#[derive(Clone, Copy, Debug)]
struct Pillar {
    tenor: f64,
    zero: f64,
}

#[derive(Clone, Debug)]
pub struct Curve {
    points: Vec<Pillar>,
    interpolation: Interpolation,
}

fn measured(v: f64) -> Result<f64, Error> {
    v.is_finite().then_some(v).ok_or(Error::NumericalFailure)
}

fn sorted(tenors: &[f64], rates: &[f64]) -> Result<Vec<(f64, f64)>, Error> {
    if tenors.len() > MAX_PILLARS {
        return Err(Error::CapacityExceeded);
    }
    if tenors.is_empty()
        || tenors.len() != rates.len()
        || !tenors.iter().chain(rates).all(|v| v.is_finite())
    {
        return Err(Error::InvalidInput);
    }
    let mut points: Vec<_> = tenors.iter().copied().zip(rates.iter().copied()).collect();
    points.sort_by(|a, b| a.0.total_cmp(&b.0));
    let mut prev = 0.0;
    for (t, _) in &points {
        if *t <= prev {
            return Err(Error::InvalidInput);
        }
        prev = *t;
    }
    Ok(points)
}

impl Curve {
    pub fn new_zero(
        tenors: &[f64],
        rates: &[f64],
        compounding: Compounding,
        interpolation: Interpolation,
    ) -> Result<Self, Error> {
        let mut points = Vec::new();
        for (tenor, rate) in sorted(tenors, rates)? {
            let zero = match compounding {
                Compounding::Continuous => rate,
                Compounding::Annual if rate > -1.0 => libm::log1p(rate),
                Compounding::SemiAnnual if rate > -2.0 => 2.0 * libm::log1p(rate / 2.0),
                _ => return Err(Error::InvalidInput),
            };
            points.push(Pillar {
                tenor,
                zero: measured(zero)?,
            });
        }
        Ok(Self {
            points,
            interpolation,
        })
    }

    /// Sequential par equations using the declared pillar dates as coupons.
    /// This is not a mixed deposit/future/swap or calendar convention resolver.
    pub fn bootstrap(
        tenors: &[f64],
        par_rates: &[f64],
        interpolation: Interpolation,
    ) -> Result<Self, Error> {
        let quotes = sorted(tenors, par_rates)?;
        let mut points = Vec::with_capacity(quotes.len());
        let mut prev = 0.0;
        let mut annuity = 0.0;
        for (tenor, rate) in quotes {
            let tau = tenor - prev;
            let numerator = 1.0 - rate * annuity;
            let denominator = 1.0 + rate * tau;
            let df = numerator / denominator;
            if ![annuity, numerator, denominator, df]
                .into_iter()
                .all(f64::is_finite)
                || numerator <= 0.0
                || denominator <= 0.0
                || df <= 0.0
            {
                return Err(Error::NumericalFailure);
            }
            // Preserve a nonzero rate even when its binary64 discount rounds
            // to 1. Negative rates may legitimately give a discount above 1.
            let ln_df = libm::log1p(-rate * annuity) - libm::log1p(rate * tau);
            points.push(Pillar {
                tenor,
                zero: measured(-ln_df / tenor)?,
            });
            annuity += tau * df;
            measured(annuity)?;
            prev = tenor;
        }
        Ok(Self {
            points,
            interpolation,
        })
    }

    pub fn zero(&self, t: f64) -> Result<f64, Error> {
        self.zero_with_bump(t, 0.0)
    }

    /// The bump is applied before interpolation, matching Go's shifted pillars.
    /// Bond shadow shares this implementation; its own bounds/contracts remain.
    pub fn zero_with_bump(&self, t: f64, bump: f64) -> Result<f64, Error> {
        if !t.is_finite() || !bump.is_finite() {
            return Err(Error::InvalidQuery);
        }
        let at = self.points.partition_point(|p| p.tenor < t);
        let value = if at == 0 {
            self.points[0].zero + bump
        } else if at == self.points.len() {
            self.points[at - 1].zero + bump
        } else {
            let a = self.points[at - 1];
            let b = self.points[at];
            let za = a.zero + bump;
            let zb = b.zero + bump;
            let w = (t - a.tenor) / (b.tenor - a.tenor);
            match self.interpolation {
                Interpolation::LinearZero => za + w * (zb - za),
                Interpolation::LogLinearDiscount => {
                    let lo = -za * a.tenor;
                    let hi = -zb * b.tenor;
                    -(lo + w * (hi - lo)) / t
                }
            }
        };
        measured(value)
    }

    pub fn discount(&self, t: f64) -> Result<f64, Error> {
        if !t.is_finite() {
            return Err(Error::InvalidQuery);
        }
        if t <= 0.0 {
            return Ok(1.0);
        }
        let df = measured((-self.zero(t)? * t).exp())?;
        if df <= 0.0 {
            return Err(Error::NumericalFailure);
        }
        Ok(df)
    }

    pub fn forward(&self, from: f64, to: f64) -> Result<f64, Error> {
        if !from.is_finite() || !to.is_finite() || to <= from {
            return Err(Error::InvalidQuery);
        }
        let span = measured(to - from)?;
        let mut total = 0.0;
        let mut start = from;
        while start < to {
            let at = self.points.partition_point(|p| p.tenor <= start);
            let mut end = to;
            let forward = if at == 0 {
                end = end.min(self.points[0].tenor);
                self.points[0].zero
            } else if at == self.points.len() {
                self.points[at - 1].zero
            } else {
                end = end.min(self.points[at].tenor);
                let a = self.points[at - 1];
                let b = self.points[at];
                match self.interpolation {
                    Interpolation::LogLinearDiscount => {
                        (b.zero * b.tenor - a.zero * a.tenor) / (b.tenor - a.tenor)
                    }
                    Interpolation::LinearZero => {
                        a.zero
                            + (start + (end - a.tenor)) * ((b.zero - a.zero) / (b.tenor - a.tenor))
                    }
                }
            };
            if end <= start {
                return Err(Error::NumericalFailure);
            }
            total += measured(forward)? * ((end - start) / span);
            start = end;
        }
        measured(total)
    }

    pub fn par_rate(&self, coupons: &[f64]) -> Result<f64, Error> {
        if coupons.len() > MAX_COUPONS {
            return Err(Error::CapacityExceeded);
        }
        if coupons.is_empty() {
            return Err(Error::InvalidQuery);
        }
        let mut annuity = 0.0;
        let mut prev = 0.0;
        for &t in coupons {
            if !t.is_finite() || t <= prev {
                return Err(Error::InvalidQuery);
            }
            annuity += (t - prev) * self.discount(t)?;
            prev = t;
        }
        if !annuity.is_finite() || annuity <= 0.0 {
            return Err(Error::NumericalFailure);
        }
        measured(-libm::expm1(-self.zero(prev)? * prev) / annuity)
    }

    pub fn tenors(&self) -> Vec<f64> {
        self.points.iter().map(|p| p.tenor).collect()
    }
    pub fn zeros(&self) -> Vec<f64> {
        self.points.iter().map(|p| p.zero).collect()
    }
}
