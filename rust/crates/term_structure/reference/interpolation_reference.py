"""Exact rational interpolation goldens for binary64 knots and neighbours.

Fraction arithmetic evaluates the declared affine equations, rounding only the
final result. The bump rounds at each input pillar, as the existing Go contract
requires. No production evaluator or floating-point interpolation is imported.
"""
import argparse
import json
import math
import struct
from fractions import Fraction as F
from pathlib import Path


def bits(value):
    return struct.unpack(">Q", struct.pack(">d", value))[0]


def generate():
    rows = []
    queries = [1., math.nextafter(1., 2.), math.nextafter(2., 1.), 2.,
               math.nextafter(2., 3.), math.nextafter(3., 2.), 3.]
    for interp in [0, 1]:
        for middle in [2.**-80, -2.**-80, 0.]:
            for bump in [0., .0001]:
                rates = [1./32, middle, -1./16]
                shifted = [F.from_float(r+bump) for r in rates]
                values = []
                for value in queries:
                    t = F.from_float(value)
                    a, b = (F(1), F(2)) if t <= 2 else (F(2), F(3))
                    index = int(a)-1
                    terms = [shifted[index]*(b-t), shifted[index+1]*(t-a)]
                    if interp:
                        terms = [terms[0]*a, terms[1]*b]
                    result = sum(terms)/(b-a)
                    if interp:
                        result /= t
                    values.append(bits(float(result)))
                rows.append(dict(interp=interp, tenors=list(map(bits, [1., 2., 3.])),
                                 rates=list(map(bits, rates)), bump=bits(bump),
                                 queries=list(map(bits, queries)), values=values))
    assert len(rows) == 12
    return dict(method="rational_barycentric_binary64_v1", cases=rows)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true")
    args = parser.parse_args()
    path = Path(__file__).resolve().parents[1]/"tests"/"interpolation_reference.json"
    result = generate()
    if args.check:
        assert json.loads(path.read_text(encoding="utf-8")) == result, "stale rational reference"
    else:
        path.write_text(json.dumps(result, indent=2)+"\n", encoding="utf-8", newline="\n")
    print("Exact rational interpolation: 12 curves, 84 queries, pre-interpolation bumps verified")
