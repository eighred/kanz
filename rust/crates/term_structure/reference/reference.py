"""Independent test oracle: dense par-equation elimination, Decimal 80/110.

Uses barycentric interpolation and high-precision log-discount differences,
not production prefix bootstrapping or piecewise-forward integration.
"""
import argparse
import json
from decimal import Decimal as D, localcontext
from pathlib import Path

CASES = [
    dict(id="annual", mode=0, comp=1, interp=0, tenors=["1","2"], rates=[".05",".05"], queries=["0",".5","1","3"], forwards=[["0","3"]]),
    dict(id="semi_negative", mode=0, comp=2, interp=1, tenors=["1","3"], rates=["-.01","-.005"], queries=[".5","1","2","3","4"], forwards=[["1.25","2.75"]]),
    dict(id="linear_adjacent", mode=0, comp=0, interp=0, tenors=["1","3"], rates=[".02",".04"], queries=["1","2","3"], forwards=[["2","2.0000000000000004"]]),
    dict(id="log_adjacent", mode=0, comp=0, interp=1, tenors=["1","3"], rates=[".02",".04"], queries=["1","2","3"], forwards=[["2","2.0000000000000004"]]),
    dict(id="cross_segments", mode=0, comp=0, interp=0, tenors=[".25","1","3","10"], rates=[".03",".035","-.01",".005"], queries=[".1",".25",".5","2","10","15"], forwards=[["0","15"]]),
    dict(id="negative_par", mode=1, comp=0, interp=1, tenors=["1","2"], rates=["-.01","-.01"], queries=["0","1","1.5","2","10"], forwards=[["0","3"]]),
    dict(id="irregular_par", mode=1, comp=0, interp=0, tenors=[".25","1","2.5","5"], rates=[".03",".031",".035",".04"], queries=[".1",".75","2","4","5","10"], forwards=[[".2","8"]]),
    dict(id="tiny_annual", mode=0, comp=1, interp=0, tenors=["1","2"], rates=["1e-20","1e-20"], queries=["1","2"], forwards=[["1","2"]]),
    dict(id="tiny_par", mode=1, comp=0, interp=1, tenors=["1","2"], rates=["1e-20","1e-20"], queries=["1","1.5","2"], forwards=[["1","2"]]),
    dict(id="long_zero", mode=0, comp=0, interp=1, tenors=["1","50"], rates=[".7",".7"], queries=["1","50","100"], forwards=[[".5","100"]]),
]


def oracle(c, precision):
    with localcontext() as ctx:
        ctx.prec = precision
        ts, rs = list(map(D,c["tenors"])), list(map(D,c["rates"]))
        n = len(ts)
        if c["mode"] == 1:
            # Solve all coupon-price equations simultaneously, independently of
            # the production O(n) recurrence and its retained prefix annuity.
            matrix = []
            for i, r in enumerate(rs):
                row = [D(0)] * (n+1)
                for j in range(i+1): row[j] = r*(ts[j]-(ts[j-1] if j else 0)) + (1 if j==i else 0)
                row[-1] = D(1)
                matrix.append(row)
            for k in range(n):
                pivot = max(range(k,n), key=lambda j:abs(matrix[j][k]))
                matrix[k], matrix[pivot] = matrix[pivot], matrix[k]
                d = matrix[k][k]
                assert d != 0
                matrix[k] = [v/d for v in matrix[k]]
                for j in range(n):
                    if j != k:
                        factor = matrix[j][k]
                        matrix[j] = [a-factor*b for a,b in zip(matrix[j],matrix[k])]
            zs = [-matrix[i][-1].ln()/ts[i] for i in range(n)]
        else:
            zs = [r if c["comp"]==0 else (1+r).ln() if c["comp"]==1 else 2*(1+r/2).ln() for r in rs]

        def zero(t, bump=D(0)):
            if t <= ts[0]: return zs[0]+bump
            if t >= ts[-1]: return zs[-1]+bump
            i = next(i for i in range(1,n) if ts[i]>=t)
            a,b,za,zb = ts[i-1],ts[i],zs[i-1]+bump,zs[i]+bump
            if c["interp"] == 0: return (za*(b-t)+zb*(t-a))/(b-a)
            return (za*a*(b-t)+zb*b*(t-a))/((b-a)*t)

        def discount(t):return (-zero(t)*t).exp() if t>0 else D(1)
        values = ts + zs
        for t in map(D,c["queries"]): values += [zero(t),discount(t),zero(t,D(".0001"))]
        for a,b in c["forwards"]:
            a,b=D(a),D(b)
            values.append((zero(b)*b-zero(a)*a)/(b-a))
        annuity = sum((t-(ts[i-1] if i else 0))*discount(t) for i,t in enumerate(ts))
        values.append((1-discount(ts[-1]))/annuity)
        return values


def generate():
    rows=[]
    for c in CASES:
        low,high = oracle(c,80),oracle(c,110)
        assert all(abs(a-b)<=D("1e-45")*(1+abs(b)) for a,b in zip(low,high)),c["id"]
        with localcontext() as ctx:
            ctx.prec=40
            rows.append(dict(c,values=[str(+v) for v in high]))
    return dict(method="decimal_dense_par_80_110_v1",cases=rows)


if __name__ == "__main__":
    p=argparse.ArgumentParser(description=__doc__);p.add_argument("--check",action="store_true");args=p.parse_args()
    path=Path(__file__).resolve().parents[1]/"tests"/"reference.json"
    result=generate()
    if args.check: assert json.loads(path.read_text(encoding="utf-8"))==result,"stale reference"
    else: path.write_text(json.dumps(result,indent=2)+"\n",encoding="utf-8",newline="\n")
    print("Decimal term structure: 10 references, dense par solve, precision 80/110 verified")
