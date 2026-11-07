# Browser exact-rendering kernel

The Vue adapter uses this kernel after validating its build-owned WASM asset,
with the existing TypeScript renderer as fallback. This library formats values, not prices,
portfolio state, risk decisions or commands. It has no dependencies or host I/O.

Decimal input is the existing coefficient string plus exponent, with an explicit
128-byte ASCII limit including sign and exponent -64 through 64. This covers all
canonical common.v1.Decimal sint64 values. The previous TypeScript renderer also
accepted unbounded, noncanonical digit strings: this is intentionally not a claim
of compatibility with those inputs. Missing-object/default-field semantics stay
in the TypeScript adapter; oversized Decimal coefficients retain the legacy path.

RatString input preserves the existing separate 128-character numerator and
denominator limits (257 bytes including slash). No fraction reduction is added:
3/6 is unsupported, 0/8 renders 0.000, and 6/4 renders 1.50. All results are exact,
with no float or rounding. Unsupported input never produces numeric zero.

The pointer-free ABI consists of render_reset, render_input_byte(index, byte),
render_value(kind, exponent), render_output_len and render_output_byte(index).
Kinds 1 and 2 select Decimal and RatString; RatString requires exponent zero.
Status 1 means rendered and 2 means unsupported. Input writes must be contiguous;
a failed write poisons the request until reset. Any write or render clears old
output. Out-of-range output reads return 256. Buffers are fixed at 257 input and
256 output bytes. No caller address, callback, import or asynchronous operation
is accepted; the module is single-instance, non-reentrant presentation work.

Actual compiled WASM, bounded linear memory, source/asset binding and Chromium
execution are separate required checks. The existing TypeScript source is frozen under tests
as the differential oracle, alongside independent rational/string goldens.
The shared build helper owns production Vite assets and the qualification recipe.
The BFF/Chromium fixture covers loading, CSP and unavailable-module fallback; its
Linux CI result is required before delivery. No financial authority is transferred.
