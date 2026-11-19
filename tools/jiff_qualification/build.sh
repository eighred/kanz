set -eu
cd /src
rustc -V
rustc --edition=2024 --crate-name jiff_core --crate-type rlib \
    --cfg 'feature="alloc"' --cfg 'feature="std"' --cfg 'feature="tz-fat"' \
    src/jiff-core/src/lib.rs -o /tmp/libjiff_core.rlib
rustc --edition=2024 --crate-name jiff_tzdb --crate-type rlib \
    src/jiff-tzdb/lib.rs -o /tmp/libjiff_tzdb.rlib
rustc --edition=2024 --crate-name jiff --crate-type rlib \
    --cfg 'feature="alloc"' --cfg 'feature="std"' --cfg 'feature="tz-fat"' \
    --cfg 'feature="tzdb-bundle-always"' --extern jcore=/tmp/libjiff_core.rlib \
    --extern jiff_tzdb=/tmp/libjiff_tzdb.rlib -L dependency=/tmp \
    src/jiff/src/lib.rs -o /tmp/libjiff.rlib

rustc --edition=2024 --test duration_contract.rs \
    --extern jiff=/tmp/libjiff.rlib -L dependency=/tmp -o /tmp/duration
/tmp/duration

cc -std=c11 -O2 -Wall -Wextra -Werror -c closed.c -o /tmp/closed.o
rustc --edition=2024 -C opt-level=1 compare.rs \
    --extern jiff=/tmp/libjiff.rlib --extern jiff_tzdb=/tmp/libjiff_tzdb.rlib \
    -L dependency=/tmp -C link-arg=/tmp/closed.o -o /tmp/compare
/tmp/compare
