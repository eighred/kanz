// Diagnostic FP64 American option prices, one 256-lane workgroup per tree.
#pragma OPENCL EXTENSION cl_khr_fp64 : enable

__kernel void price(__global const double *contracts, __global double *prices)
{
    const size_t contract = get_group_id(0);
    const int node = get_local_id(0);
    __global const double *c = contracts + contract * 7;
    const double sign = c[0], spot = c[1], strike = c[2], time = c[3];
    const double rate = c[4], yield = c[5], vol = c[6];
    const int steps = 200;
    const double dt = time / steps;
    const double step = vol * sqrt(dt);
    const double up = exp(step), down = exp(-step);
    const double discount = exp(-rate * dt);
    const double p = (expm1((rate - yield) * dt) - expm1(-step)) / (up - down);
    __local double value[2][201];
    double asset = spot * exp((2 * node - steps) * step);
    if (node <= steps)
        value[0][node] = fmax(sign * (asset - strike), 0.0);
    barrier(CLK_LOCAL_MEM_FENCE);
    for (int level = steps - 1; level >= 0; --level) {
        const int previous = (steps - level - 1) % 2;
        if (node <= level) {
            asset *= up;
            const double continuation = discount *
                (p * value[previous][node + 1] + (1.0 - p) * value[previous][node]);
            value[1 - previous][node] = fmax(sign * (asset - strike), continuation);
        }
        barrier(CLK_LOCAL_MEM_FENCE);
    }
    if (node == 0)
        prices[contract] = value[steps % 2][0];
}
