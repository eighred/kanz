//! Fixed-depth American CRR candidate; no runtime or financial authority.
use crate::{Error, Exercise, Input, Output, Right};

const STEPS: usize = 200;
const SPOT_BUMP: f64 = 0.01;
const VOL_BUMP: f64 = 0.001;
const TIME_BUMP: f64 = 0.0001;
const RATE_BUMP: f64 = 0.0001;

fn finite(i: Input) -> bool {
    [
        i.spot,
        i.strike,
        i.years,
        i.rate,
        i.yield_rate,
        i.volatility,
    ]
    .into_iter()
    .all(f64::is_finite)
}

fn intrinsic(i: Input, spot: f64) -> f64 {
    match i.right {
        Right::Call => (spot - i.strike).max(0.0),
        Right::Put => (i.strike - spot).max(0.0),
    }
}

fn degenerate(i: Input) -> bool {
    i.years <= 0.0 || i.volatility <= 0.0 || i.spot <= 0.0 || i.strike <= 0.0
}

fn price(i: Input, steps: usize) -> Result<f64, Error> {
    if !finite(i) {
        return Err(Error::NonFiniteInput);
    }
    if degenerate(i) {
        let p = intrinsic(i, i.spot);
        return p.is_finite().then_some(p).ok_or(Error::NonFiniteOutput);
    }
    let dt = i.years / steps as f64;
    let u = (i.volatility * dt.sqrt()).exp();
    let d = 1.0 / u;
    let disc = (-i.rate * dt).exp();
    let p = (((i.rate - i.yield_rate) * dt).exp() - d) / (u - d);
    if ![dt, u, d, disc, p].into_iter().all(f64::is_finite)
        || dt <= 0.0
        || u <= d
        || d <= 0.0
        || disc <= 0.0
        || !(0.0..=1.0).contains(&p)
    {
        return Err(Error::InvalidTree);
    }
    // Fixed-size storage bounds work independently of caller input. Only the
    // internal exact-tree unit control uses a smaller depth.
    let mut values = [0.0; STEPS + 1];
    let node = |level: usize, up: usize| {
        i.spot * libm::pow(u, up as f64) * libm::pow(d, (level - up) as f64)
    };
    for (up, value) in values.iter_mut().enumerate().take(steps + 1) {
        let s = node(steps, up);
        *value = intrinsic(i, s);
        if !s.is_finite() || !value.is_finite() {
            return Err(Error::NonFiniteOutput);
        }
    }
    for level in (0..steps).rev() {
        for up in 0..=level {
            let s = node(level, up);
            let continuation = disc * (p * values[up + 1] + (1.0 - p) * values[up]);
            if !s.is_finite() || !continuation.is_finite() {
                return Err(Error::NonFiniteOutput);
            }
            values[up] = continuation.max(intrinsic(i, s));
        }
    }
    Ok(values[0])
}

/// Match the served Go American family: 200 steps and its existing bumps.
/// A failed tree or bump refuses the entire candidate, without substituting
/// European analytics or fabricating zero sensitivities. Legacy finite
/// degenerate inputs remain spot intrinsic/zero Greeks, not admission defaults.
pub fn evaluate(i: Input) -> Result<Output, Error> {
    if i.exercise != Exercise::American {
        return Err(Error::UnsupportedModel);
    }
    if !finite(i) {
        return Err(Error::NonFiniteInput);
    }
    let mid = price(i, STEPS)?;
    if degenerate(i) {
        return Ok(Output {
            price: mid,
            delta: 0.0,
            gamma: 0.0,
            vega: 0.0,
            theta: 0.0,
            rho: 0.0,
        });
    }
    let h = i.spot * SPOT_BUMP;
    let up = price(
        Input {
            spot: i.spot + h,
            ..i
        },
        STEPS,
    )?;
    let down = price(
        Input {
            spot: i.spot - h,
            ..i
        },
        STEPS,
    )?;
    let v_up = price(
        Input {
            volatility: i.volatility + VOL_BUMP,
            ..i
        },
        STEPS,
    )?;
    let v_down = price(
        Input {
            volatility: i.volatility - VOL_BUMP,
            ..i
        },
        STEPS,
    )?;
    // Preserve the Go convention: very short maturities center theta at 2e-4
    // years, so this is not the local derivative at the requested maturity.
    let tt = if i.years - TIME_BUMP <= 0.0 {
        2.0 * TIME_BUMP
    } else {
        i.years
    };
    let t_up = price(
        Input {
            years: tt + TIME_BUMP,
            ..i
        },
        STEPS,
    )?;
    let t_down = price(
        Input {
            years: tt - TIME_BUMP,
            ..i
        },
        STEPS,
    )?;
    let r_up = price(
        Input {
            rate: i.rate + RATE_BUMP,
            ..i
        },
        STEPS,
    )?;
    let r_down = price(
        Input {
            rate: i.rate - RATE_BUMP,
            ..i
        },
        STEPS,
    )?;
    let result = Output {
        price: mid,
        delta: (up - down) / (2.0 * h),
        gamma: (up - 2.0 * mid + down) / (h * h),
        vega: (v_up - v_down) / (2.0 * VOL_BUMP),
        theta: (t_up - t_down) / (2.0 * TIME_BUMP),
        rho: (r_up - r_down) / (2.0 * RATE_BUMP),
    };
    if !result.values().into_iter().all(f64::is_finite) {
        return Err(Error::NonFiniteOutput);
    }
    Ok(result)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn exact_three_step_stopping_tree() {
        // u=2, d=1/2, p=1/2, discount=4/5. Explicit optimal rows:
        // [9,6,0,0] -> [8,12/5,0] -> [6,24/25] -> 348/125.
        let i = Input {
            right: Right::Put,
            exercise: Exercise::American,
            spot: 8.0,
            strike: 10.0,
            years: 3.0,
            rate: 1.25_f64.ln(),
            yield_rate: 0.0,
            volatility: 2.0_f64.ln(),
        };
        assert!((price(i, 3).unwrap() - 348.0 / 125.0).abs() < 1e-12);
    }
}
