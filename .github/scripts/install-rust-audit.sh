#!/usr/bin/env bash
# Tool installation may be cached by Cargo; audit results must never be cached.
set -euo pipefail
rustup toolchain install 1.90.0 --profile minimal
tools="$RUNNER_TEMP/kanz-rust-audit-tools"
cargo +1.90.0 install --locked --version 0.22.2 --root "$tools" cargo-audit
cargo +1.90.0 install --locked --version 0.5.4 --root "$tools" rust-audit-info
echo "$tools/bin" >> "$GITHUB_PATH"
