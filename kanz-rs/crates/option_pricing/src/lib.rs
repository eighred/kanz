#![forbid(unsafe_code)]
//! Pure, non-authoritative compatibility candidate for Go's European BSM analytics.
//! No terms, market-data, freshness, portfolio scaling, or financial-record authority.

use std::f64::consts::{PI, SQRT_2};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Right {
    Call,
    Put,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Exercise {
    European,
    American,
}

/// Already-resolved binary64 working inputs, not a canonical money/quantity wire.
/// Years, rates and volatility use the existing Go pricing library's annual units.
#[derive(Clone, Copy, Debug)]
pub struct Input {
    pub right: Right,
    pub exercise: Exercise,
    pub spot: f64,
    pub strike: f64,
    pub years: f64,
    pub rate: f64,
    pub yield_rate: f64,
    pub volatility: f64,
}

/// Per-unit sensitivities: vega/rho per 1.00, theta per year TO expiry.
/// Go retains dollar-Greek scaling and canonical Decimal emission.
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct Output {
    pub price: f64,
    pub delta: f64,
    pub gamma: f64,
    pub vega: f64,
    pub theta: f64,
    pub rho: f64,
}

impl Output {
    pub fn values(self) -> [f64; 6] {
        [
            self.price, self.delta, self.gamma, self.vega, self.theta, self.rho,
        ]
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Error {
    UnsupportedModel,
    NonFiniteInput,
    NonFiniteOutput,
}

fn cdf(x: f64) -> f64 {
    0.5 * libm::erfc(-x / SQRT_2)
}

/// Match Go pricing.PriceGreeks's European family, including its explicitly
/// documented legacy spot-intrinsic/zero-Greeks fallback. That fallback is not
/// a theoretical zero-volatility limit or permission to admit missing inputs:
/// the production Go owner resolves terms and rejects missing/expired inputs.
pub fn evaluate(i: Input) -> Result<Output, Error> {
    if i.exercise != Exercise::European {
        return Err(Error::UnsupportedModel);
    }
    if ![
        i.spot,
        i.strike,
        i.years,
        i.rate,
        i.yield_rate,
        i.volatility,
    ]
    .into_iter()
    .all(f64::is_finite)
    {
        return Err(Error::NonFiniteInput);
    }
    let result = if i.years <= 0.0 || i.volatility <= 0.0 || i.spot <= 0.0 || i.strike <= 0.0 {
        let difference = match i.right {
            Right::Call => i.spot - i.strike,
            Right::Put => i.strike - i.spot,
        };
        Output {
            price: difference.max(0.0),
            delta: 0.0,
            gamma: 0.0,
            vega: 0.0,
            theta: 0.0,
            rho: 0.0,
        }
    } else {
        let sqrt_t = i.years.sqrt();
        let vol = i.volatility * sqrt_t;
        let d1 = ((i.spot / i.strike).ln()
            + (i.rate - i.yield_rate + 0.5 * i.volatility * i.volatility) * i.years)
            / vol;
        let d2 = d1 - vol;
        let df = (-i.rate * i.years).exp();
        let dq = (-i.yield_rate * i.years).exp();
        let pdf = (-0.5 * d1 * d1).exp() / (2.0 * PI).sqrt();
        let decay = -(i.spot * dq * pdf * i.volatility) / (2.0 * sqrt_t);
        let gamma = dq * pdf / (i.spot * i.volatility * sqrt_t);
        let vega = i.spot * dq * pdf * sqrt_t;
        match i.right {
            Right::Call => Output {
                price: i.spot * dq * cdf(d1) - i.strike * df * cdf(d2),
                delta: dq * cdf(d1),
                gamma,
                vega,
                theta: -(decay - i.rate * i.strike * df * cdf(d2)
                    + i.yield_rate * i.spot * dq * cdf(d1)),
                rho: i.strike * i.years * df * cdf(d2),
            },
            Right::Put => Output {
                price: i.strike * df * cdf(-d2) - i.spot * dq * cdf(-d1),
                delta: dq * (cdf(d1) - 1.0),
                gamma,
                vega,
                theta: -(decay + i.rate * i.strike * df * cdf(-d2)
                    - i.yield_rate * i.spot * dq * cdf(-d1)),
                rho: -i.strike * i.years * df * cdf(-d2),
            },
        }
    };
    if !result.values().into_iter().all(f64::is_finite) {
        return Err(Error::NonFiniteOutput);
    }
    Ok(result)
}
