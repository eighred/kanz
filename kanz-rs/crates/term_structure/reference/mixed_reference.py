"""Independent Decimal price equations, damped Newton in log(discount).

160/200 digits resolve sub-epsilon quotes without the production log1p,
ParRate objective, bisection, root bracket or termination branches.
"""
import argparse
import json
from decimal import Decimal as D, localcontext
from pathlib import Path


def quote(kind, tenor, value, span="0"):
    return dict(kind=kind, tenor=tenor, span=span, value=value)


CASES = []
for interp in [0, 1]:
    for name, quotes in [
        ("tiny_deposit", [quote(0,"1","1e-20")]),
        ("tiny_future", [quote(0,"1","1e-100"),quote(1,"1","1e-100","1")]),
        ("tiny_swap", [quote(2,"1","-1e-20"),quote(2,"2","-1e-20")]),
        ("zero", [quote(0,".25","0"),quote(1,".25","0",".25"),quote(2,"3.5","0")]),
        ("irregular", [quote(2,"3.5",".039"),quote(0,".125",".03"),quote(1,".125",".033",".375"),quote(2,"1.25",".036")]),
        ("negative", [quote(0,".25","-.03"),quote(1,".25","-.033",".25"),quote(2,"2","-.036"),quote(2,"5","-.039")]),
    ]:
        CASES.append(dict(id=f"{name}/{interp}",interp=interp,quotes=quotes,queries=[".1","1","7"]))


def oracle(case, precision):
    with localcontext() as ctx:
        ctx.prec = precision
        def span(q): return D(q["span"]) if D(q["span"]) else D(".25")
        def end(q): return D(q["tenor"]) + (span(q) if q["kind"]==1 else 0)
        def dates(t):
            # Match the pre-existing resolved annual/stub schedule contract.
            out=[]; n=D(1)
            while n<t-D("1e-9"): out.append(n);n+=1
            return out+[t]
        ts=[];zs=[]
        def zero(t):
            if t<=ts[0]: return zs[0]
            if t>=ts[-1]: return zs[-1]
            i=next(i for i in range(1,len(ts)) if ts[i]>=t)
            a,b,za,zb=ts[i-1],ts[i],zs[i-1],zs[i]
            if case["interp"]==0: return (za*(b-t)+zb*(t-a))/(b-a)
            return (za*a*(b-t)+zb*b*(t-a))/(t*(b-a))
        def df(t): return (-zero(t)*t).exp() if t>0 else D(1)
        for q in sorted(case["quotes"],key=end):
            t,r=end(q),D(q["value"])
            if q["kind"]==0: z=-(1/(1+r*t)).ln()/t
            elif q["kind"]==1: z=-(df(D(q["tenor"]))/(1+r*span(q))).ln()/t
            else:
                ts.append(t);zs.append(D(0))
                schedule=dates(t)
                def balance(log_discount):
                    zs[-1]=-log_discount/t
                    annuity=sum((v-(schedule[i-1] if i else 0))*df(v) for i,v in enumerate(schedule))
                    return log_discount.exp()+r*annuity-1
                x=-r*t
                for _ in range(32):
                    f=balance(x)
                    # At the precision floor a Newton step can no longer
                    # improve a rounded PV. Final quote-relative repricing and
                    # the separate 160/200 refinement still have to pass.
                    if abs(f)<D("1e-145"): break
                    h=D("1e-40")
                    derivative=(balance(x+h)-balance(x-h))/(2*h)
                    assert derivative!=0
                    step=f/derivative
                    for _ in range(64):
                        next_x=x-step
                        if abs(balance(next_x))<abs(f): break
                        step/=2
                    else: raise AssertionError("Newton failed to improve price equation")
                    x=next_x
                    if abs(step)<D("1e-140"): break
                else: raise AssertionError("Newton exhausted")
                assert abs(balance(x))<D("1e-140")
                z=-x/t
                ts.pop();zs.pop()
            ts.append(t);zs.append(z)
        out=ts+zs
        for q in case["quotes"]:
            t,r=end(q),D(q["value"])
            if q["kind"]==0: value=(1/df(t)-1)/t
            elif q["kind"]==1: value=(df(D(q["tenor"]))/df(t)-1)/span(q)
            else:
                schedule=dates(t)
                annuity=sum((v-(schedule[i-1] if i else 0))*df(v) for i,v in enumerate(schedule))
                value=(1-df(t))/annuity
            assert abs(value-r)<=D("1e-45")*abs(r),case["id"]
            out.append(value)
        for t in map(D,case["queries"]):out.extend([zero(t),df(t)])
        return out


def generate():
    rows=[]
    for case in CASES:
        low,high=oracle(case,160),oracle(case,200)
        assert all(abs(a-b)<=D("1e-45")*abs(b) for a,b in zip(low,high)),case["id"]
        with localcontext() as ctx:
            ctx.prec=40
            rows.append(dict(case,values=[str(+v) for v in high]))
    return dict(method="decimal_price_newton_160_200_v1",cases=rows)


if __name__=="__main__":
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument("--check",action="store_true");args=parser.parse_args()
    path=Path(__file__).resolve().parents[1]/"tests"/"mixed_reference.json"
    result=generate()
    if args.check: assert json.loads(path.read_text(encoding="utf-8"))==result,"stale mixed reference"
    else:path.write_text(json.dumps(result,indent=2)+"\n",encoding="utf-8",newline="\n")
    print("Decimal mixed calibration: 12 price-equation Newton references, precision 160/200 verified")
