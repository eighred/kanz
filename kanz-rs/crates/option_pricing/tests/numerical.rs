use kanz_option_pricing::{evaluate, Error, Exercise, Input, Right};
use std::f64::consts::PI;

fn input(
    right: Right,
    spot: f64,
    strike: f64,
    years: f64,
    rate: f64,
    yield_rate: f64,
    volatility: f64,
) -> Input {
    Input {
        right,
        exercise: Exercise::European,
        spot,
        strike,
        years,
        rate,
        yield_rate,
        volatility,
    }
}
fn close(got: f64, expected: f64, tolerance: f64) {
    assert!(
        got.is_finite() && (got - expected).abs() <= tolerance,
        "got {got}, expected {expected}, tolerance {tolerance}"
    );
}

#[test]
fn published_mathworks_prices() {
    // MathWorks blsprice documentation, R2026b, checked 2026-11-12.
    // Its four-decimal examples justify 0.00005 price units, not extra digits.
    for (spot, strike, years, rate, yield_rate, volatility, call, put) in [
        (100.0, 95.0, 0.25, 0.1, 0.0, 0.5, 13.6953, 6.3497),
        (910.0, 980.0, 0.25, 0.02, 0.025, 0.25, 19.6863, 90.4683),
    ] {
        for (right, expected) in [(Right::Call, call), (Right::Put, put)] {
            close(
                evaluate(input(
                    right, spot, strike, years, rate, yield_rate, volatility,
                ))
                .unwrap()
                .price,
                expected,
                0.00005,
            );
        }
    }
}

// Independent risk-neutral payoff integration. No CDF or production price/Greek
// helper occurs here. Split at the payoff kink, then Simpson-integrate density
// times the discounted terminal payoff on [-12,12]. Refinement is checked below.
fn integral(i: Input, steps: usize) -> f64 {
    let vol = i.volatility * i.years.sqrt();
    let drift = (i.rate - i.yield_rate - 0.5 * i.volatility * i.volatility) * i.years;
    let boundary = ((i.strike / i.spot).ln() - drift) / vol;
    let (lo, hi) = match i.right {
        Right::Call => (boundary.max(-12.0), 12.0),
        Right::Put => (-12.0, boundary.min(12.0)),
    };
    if lo >= hi {
        return 0.0;
    }
    let width = (hi - lo) / steps as f64;
    let mut total = 0.0;
    for n in 0..=steps {
        let z = lo + n as f64 * width;
        let terminal = i.spot * (drift + vol * z).exp();
        let payoff = match i.right {
            Right::Call => terminal - i.strike,
            Right::Put => i.strike - terminal,
        }
        .max(0.0);
        let weight = if n == 0 || n == steps {
            1.0
        } else if n % 2 == 0 {
            2.0
        } else {
            4.0
        };
        total += weight * payoff * (-0.5 * z * z).exp() / (2.0 * PI).sqrt();
    }
    (-i.rate * i.years).exp() * total * width / 3.0
}

#[test]
fn price_and_greeks_match_independent_payoff_integration() {
    for right in [Right::Call, Right::Put] {
        for i in [
            input(right, 100.0, 95.0, 0.25, 0.1, 0.0, 0.5),
            input(right, 100.0, 100.0, 1.0, -0.02, 0.03, 0.2),
        ] {
            let g = evaluate(i).unwrap();
            let p = integral(i, 16384);
            close(integral(i, 8192), p, 2e-10);
            close(g.price, p, 5e-9);
            let mut plus = i;
            let mut minus = i;
            plus.spot += 0.01;
            minus.spot -= 0.01;
            let up = integral(plus, 16384);
            let down = integral(minus, 16384);
            close(g.delta, (up - down) / 0.02, 2e-6);
            close(g.gamma, (up - 2.0 * p + down) / 0.0001, 2e-6);
            plus = i;
            minus = i;
            plus.volatility += 0.0001;
            minus.volatility -= 0.0001;
            close(
                g.vega,
                (integral(plus, 16384) - integral(minus, 16384)) / 0.0002,
                2e-4,
            );
            plus = i;
            minus = i;
            plus.years += 0.0001;
            minus.years -= 0.0001;
            close(
                g.theta,
                (integral(plus, 16384) - integral(minus, 16384)) / 0.0002,
                2e-4,
            );
            plus = i;
            minus = i;
            plus.rate += 0.00001;
            minus.rate -= 0.00001;
            close(
                g.rho,
                (integral(plus, 16384) - integral(minus, 16384)) / 0.00002,
                2e-4,
            );
        }
    }
}

#[test]
fn put_call_price_and_sensitivity_parity() {
    for rate in [-0.1, 0.0, 0.1] {
        for yield_rate in [-0.02, 0.0, 0.03] {
            let i = input(Right::Call, 100.0, 95.0, 0.5, rate, yield_rate, 0.2);
            let c = evaluate(i).unwrap();
            let p = evaluate(Input {
                right: Right::Put,
                ..i
            })
            .unwrap();
            let s = i.spot * (-yield_rate * i.years).exp();
            let k = i.strike * (-rate * i.years).exp();
            close(c.price - p.price, s - k, 1e-12);
            close(c.delta - p.delta, (-yield_rate * i.years).exp(), 1e-14);
            close(c.gamma, p.gamma, 1e-14);
            close(c.vega, p.vega, 1e-12);
            close(c.rho - p.rho, i.years * k, 1e-12);
            close(c.theta - p.theta, -yield_rate * s + rate * k, 1e-12);
        }
    }
}

#[test]
fn legacy_degenerate_convention_is_explicit() {
    let base = input(Right::Call, 100.0, 95.0, 1.0, 0.1, 0.02, 0.2);
    for i in [
        Input { years: 0.0, ..base },
        Input {
            years: -1.0,
            ..base
        },
        Input {
            volatility: 0.0,
            ..base
        },
        Input {
            volatility: -0.2,
            ..base
        },
    ] {
        assert_eq!(
            evaluate(i).unwrap().values(),
            [5.0, 0.0, 0.0, 0.0, 0.0, 0.0]
        );
    }
}

#[test]
fn unsupported_and_unavailable_results_never_become_zero_candidates() {
    let base = input(Right::Call, 100.0, 95.0, 1.0, 0.05, 0.0, 0.2);
    assert_eq!(
        evaluate(Input {
            exercise: Exercise::American,
            ..base
        }),
        Err(Error::UnsupportedModel)
    );
    for bad in [f64::NAN, f64::INFINITY, f64::NEG_INFINITY] {
        assert_eq!(
            evaluate(Input { spot: bad, ..base }),
            Err(Error::NonFiniteInput)
        );
        assert_eq!(
            evaluate(Input {
                volatility: bad,
                ..base
            }),
            Err(Error::NonFiniteInput)
        );
    }
    assert_eq!(
        evaluate(Input {
            yield_rate: -1000.0,
            ..base
        }),
        Err(Error::NonFiniteOutput)
    );
}
