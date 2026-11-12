"""Test-only Decimal Snell oracle; no production pricing helper or dependency.

Whole-lattice memoized recursion and log-space node construction differ from the
binary64 rolling-row/power implementation. Recompute at 80/110 digits before
retaining 40 significant digits. This qualifies the discrete 200-step method,
not continuous-time convergence, model promotion or a financial record.
"""
import argparse
import functools
import json
from decimal import Decimal as D, localcontext
from pathlib import Path


CASES = [
    ("call_no_yield", "call", ["100", "100", "1", ".05", "0", ".2"]),
    ("put_atm", "put", ["100", "100", "1", ".05", "0", ".2"]),
    ("call_dividend_exercise", "call", ["100", "85", ".25", ".01", ".1", ".15"]),
    ("put_immediate", "put", ["60", "100", "1", ".05", "0", ".2"]),
    ("call_negative_rate", "call", ["100", "85", ".25", "-.05", "0", ".15"]),
    ("near_expiry_theta_shift", "put", ["100", "100", ".00001", "0", "0", ".2"]),
    ("vol_bump_crosses_zero", "call", ["100", "100", "1", "0", "0", ".0005"]),
    ("negative_yield", "put", ["100", "105", "2", "-.02", "-.03", ".35"]),
]


def oracle(right, inputs, precision):
    with localcontext() as ctx:
        ctx.prec = precision
        s, k, t, r, q, vol = map(D, inputs)

        def price(spot, time, rate, sigma):
            sign = 1 if right == "call" else -1
            if sigma <= 0 or time <= 0:
                return max(sign * (spot - k), D(0))
            dt = time / 200
            step = sigma * dt.sqrt()
            up, down = step.exp(), (-step).exp()
            probability = (((rate - q) * dt).exp() - down) / (up - down)
            assert 0 <= probability <= 1
            discount = (-rate * dt).exp()
            nodes = {n: spot * (n * step).exp() for n in range(-200, 201)}

            @functools.lru_cache(None)
            def stopping(level, ups):
                exercise = max(sign * (nodes[2 * ups - level] - k), D(0))
                if level == 200:
                    return exercise
                continuation = discount * (
                    probability * stopping(level + 1, ups + 1)
                    + (1 - probability) * stopping(level + 1, ups)
                )
                return max(exercise, continuation)

            value = stopping(0, 0)
            stopping.cache_clear()
            return value

        h, hv, ht, hr = s / 100, D(".001"), D(".0001"), D(".0001")
        mid = price(s, t, r, vol)
        up, down = price(s + h, t, r, vol), price(s - h, t, r, vol)
        tt = max(t, 2 * ht) if t <= ht else t
        return [mid, (up - down) / (2 * h), (up - 2 * mid + down) / h**2,
                (price(s, t, r, vol + hv) - price(s, t, r, vol - hv)) / (2 * hv),
                (price(s, tt + ht, r, vol) - price(s, tt - ht, r, vol)) / (2 * ht),
                (price(s, t, r + hr, vol) - price(s, t, r - hr, vol)) / (2 * hr)]


def generate():
    rows = []
    for name, right, inputs in CASES:
        low, high = oracle(right, inputs, 80), oracle(right, inputs, 110)
        assert all(abs(a - b) < D("1e-45") for a, b in zip(low, high)), name
        with localcontext() as ctx:
            ctx.prec = 40
            values = [str(+v) if abs(v) >= D("1e-45") else "0" for v in high]
        rows.append(dict(id=name, right=right, inputs=inputs, values=values))
    return dict(method="decimal_snell_80_110_v1", steps=200, cases=rows)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true")
    args = parser.parse_args()
    path = Path(__file__).resolve().parents[1] / "tests" / "american_reference.json"
    result = generate()
    if args.check:
        assert json.loads(path.read_text(encoding="utf-8")) == result, "stale numerical reference"
    else:
        path.write_text(json.dumps(result, indent=2) + "\n", encoding="utf-8")
    print("Decimal Snell: 8 cases, 6 fields, 200 steps, precision 80/110 verified")
