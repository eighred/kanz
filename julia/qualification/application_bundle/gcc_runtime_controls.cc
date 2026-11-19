#include <algorithm>
#include <bitset>
#include <charconv>
#include <cmath>
#include <cstdlib>
#include <fstream>
#include <new>
#include <sstream>
#include <vector>
#include <parallel/algorithm>
#include <ext/pb_ds/priority_queue.hpp>

int main() {
    void* allocation = ::operator new(128, std::align_val_t(64));
    ::operator delete(allocation, std::align_val_t(64));
    std::vector<bool> flags(16, false);
    flags[7] = true;
    if (std::count(flags.begin(), flags.end(), true) != 1) return 1;
    std::bitset<8> bits;
    std::istringstream("10100101") >> bits;
    if (bits.to_ulong() != 165) return 2;
    const char digits[] = "12.5";
    double value = 0;
    auto parsed = std::from_chars(digits, digits + 4, value);
    if (parsed.ec != std::errc() || parsed.ptr != digits + 4 || value != 12.5) return 3;
    __gnu_pbds::priority_queue<int, std::less<int>, __gnu_pbds::binary_heap_tag> queue;
    queue.push(2);
    queue.push(7);
    if (queue.top() != 7) return 4;
    std::vector<int> values{2, 5, 8, 10};
    if (__gnu_parallel::find(values.begin(), values.end(), 8) != values.begin()+2) return 5;
    const char* file = "/tmp/kanz-regular-file";
    { std::ofstream out(file); out << "abc"; if (!out) return 6; }
    std::ifstream in(file);
    std::string actual;
    in >> actual;
    return actual == "abc" ? 0 : 7;
}
